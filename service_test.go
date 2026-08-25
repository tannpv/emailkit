package emailkit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// fakeStore honours the Store concurrency contract with a mutex, so the -race
// detector stays quiet even though sends run on their own goroutines.
type fakeStore struct {
	mu sync.Mutex

	// suppressed is keyed by the EXACT string deliver() passes to
	// IsSuppressed. Entries are written lowercase, so a test that sends to a
	// mixed-case address only matches if deliver() normalised it.
	suppressed map[string]bool
	supErr     error

	// queried records what deliver() actually looked up.
	queried []string

	logs    []SendRecord
	logErr  error
	tmpl    *TemplateDef
	tmplKey string
}

func (f *fakeStore) IsSuppressed(_ context.Context, email string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queried = append(f.queried, email)
	if f.supErr != nil {
		return false, f.supErr
	}
	return f.suppressed[email], nil
}

func (f *fakeStore) LogSend(_ context.Context, r SendRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, r)
	return f.logErr
}

func (f *fakeStore) Template(_ context.Context, key string) (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tmpl == nil {
		return "", "", false
	}
	f.tmplKey = key
	return f.tmpl.Subject, f.tmpl.Body, true
}

func (f *fakeStore) MarkByProviderID(context.Context, string, string, *string) error { return nil }
func (f *fakeStore) Suppress(context.Context, string, string) error                  { return nil }

func (f *fakeStore) snapshot() ([]SendRecord, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SendRecord(nil), f.logs...), append([]string(nil), f.queried...)
}

// fakeSvcSender is named distinctly from sender_test.go's fakeSender (same
// package, both are _test.go files) to avoid a redeclaration error. It records
// its arguments so tests can assert what actually reached the provider.
type fakeSvcSender struct {
	mu      sync.Mutex
	calls   int
	id      string
	err     error
	to      string
	subject string
	html    string
}

func (s *fakeSvcSender) Send(_ context.Context, _, _, to, subject, html string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.to, s.subject, s.html = to, subject, html
	return s.id, s.err
}

func (s *fakeSvcSender) seen() (int, string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.to, s.subject, s.html
}

func newTestService(st *fakeStore, sn *fakeSvcSender, key string) *Service {
	return NewServiceWithSender(st, Config{APIKey: key, From: "T <t@example.com>"},
		Registry{"k": {Subject: "registry-subject", Body: "registry-body"}}, sn)
}

// captureLogs installs a buffer-backed default logger for the duration of one
// test and restores the previous one afterwards.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestDeliver_SuppressedSkips(t *testing.T) {
	st := &fakeStore{suppressed: map[string]bool{"a@b.c": true}}
	sn := &fakeSvcSender{}
	svc := newTestService(st, sn, "key")
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()
	if calls, _, _, _ := sn.seen(); calls != 0 {
		t.Fatal("suppressed address must never reach the sender")
	}
	logs, _ := st.snapshot()
	if len(logs) != 1 || logs[0].Status != StatusSuppressed {
		t.Fatalf("want one suppressed log, got %+v", logs)
	}
	if logs[0].Error == nil || *logs[0].Error != reasonSuppressed {
		t.Fatalf("want suppression reason recorded, got %+v", logs[0].Error)
	}
}

// TestDeliver_SuppressionLookupIsCaseInsensitive pins the strings.ToLower in
// deliver(). The suppression entry is lowercase and the recipient is not:
// without normalisation the lookup misses and a suppressed address is mailed.
func TestDeliver_SuppressionLookupIsCaseInsensitive(t *testing.T) {
	st := &fakeStore{suppressed: map[string]bool{"user@example.com": true}}
	sn := &fakeSvcSender{}
	svc := newTestService(st, sn, "key")
	svc.Send(context.Background(), "k", "User@Example.COM", nil)
	svc.Wait()

	logs, queried := st.snapshot()
	if len(queried) != 1 || queried[0] != "user@example.com" {
		t.Fatalf("suppression lookup must be normalised to lowercase, queried %q", queried)
	}
	if calls, _, _, _ := sn.seen(); calls != 0 {
		t.Fatal("mixed-case recipient must still hit the lowercase suppression entry")
	}
	if len(logs) != 1 || logs[0].Status != StatusSuppressed {
		t.Fatalf("want one suppressed log, got %+v", logs)
	}
}

// TestDeliver_SuppressionLookupErrorWithholdsSend is the Fix 2 regression: an
// errored lookup used to fall through and send.
func TestDeliver_SuppressionLookupErrorWithholdsSend(t *testing.T) {
	buf := captureLogs(t)
	st := &fakeStore{supErr: errors.New("db connection refused")}
	sn := &fakeSvcSender{}
	svc := newTestService(st, sn, "key")
	svc.Send(context.Background(), "k", "user@example.com", nil)
	svc.Wait()

	if calls, _, _, _ := sn.seen(); calls != 0 {
		t.Fatal("a failed suppression lookup must fail CLOSED — no send")
	}
	logs, _ := st.snapshot()
	if len(logs) != 1 || logs[0].Status != StatusSkipped {
		t.Fatalf("want one skipped log, got %+v", logs)
	}
	if logs[0].Error == nil || *logs[0].Error != reasonSuppressionLookup {
		t.Fatalf("skip reason must name the lookup failure, got %v", logs[0].Error)
	}
	if !strings.Contains(buf.String(), "suppression lookup failed") {
		t.Fatalf("want a warning about the lookup failure, got %q", buf.String())
	}
}

func TestDeliver_NoKeySkips(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{}
	svc := newTestService(st, sn, "")
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()
	if calls, _, _, _ := sn.seen(); calls != 0 {
		t.Fatal("must not attempt a send with no API key")
	}
	logs, _ := st.snapshot()
	if len(logs) != 1 || logs[0].Status != StatusSkipped {
		t.Fatalf("want one skipped log, got %+v", logs)
	}
	if logs[0].Error == nil || *logs[0].Error != reasonNotConfigured {
		t.Fatalf("want the not-configured reason, got %v", logs[0].Error)
	}
	if strings.Contains(*logs[0].Error, "Resend") {
		t.Fatal("reason must be provider-neutral — Sender is the SES/Postmark seam")
	}
}

func TestDeliver_SendsAndLogsSent(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{id: "prov-1"}
	svc := newTestService(st, sn, "key")
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()
	if calls, _, _, _ := sn.seen(); calls != 1 {
		t.Fatalf("want 1 send, got %d", calls)
	}
	logs, _ := st.snapshot()
	if len(logs) != 1 || logs[0].Status != StatusSent {
		t.Fatalf("want one sent log, got %+v", logs)
	}
	if logs[0].ProviderID == nil || *logs[0].ProviderID != "prov-1" {
		t.Fatal("provider id must be recorded — the webhook joins on it")
	}
}

// TestDeliver_AuditRowCarriesRecipientAndSubject pins the field assignment in
// log(): swapping To and Subject leaves every other assertion in this suite
// green, and only surfaces later as an audit row nobody can join to a person.
func TestDeliver_AuditRowCarriesRecipientAndSubject(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{id: "prov-1"}
	svc := newTestService(st, sn, "key")
	svc.Send(context.Background(), "k", "recipient@example.com", nil)
	svc.Wait()

	logs, _ := st.snapshot()
	if len(logs) != 1 {
		t.Fatalf("want one log, got %+v", logs)
	}
	got := logs[0]
	if got.To != "recipient@example.com" {
		t.Fatalf("To must hold the recipient, got %q", got.To)
	}
	if got.Subject != "registry-subject" {
		t.Fatalf("Subject must hold the rendered subject, got %q", got.Subject)
	}
	if got.Type != "k" {
		t.Fatalf("Type must hold the template key/label, got %q", got.Type)
	}
}

// TestRender_StoreTemplateOverridesRegistry exercises the Store-override branch
// (previously dead in this suite: fakeStore.tmpl was never assigned) and pins
// the escape policy — subject raw, body escaped — that Fix 4 routed through the
// single definition in Registry.Render.
func TestRender_StoreTemplateOverridesRegistry(t *testing.T) {
	st := &fakeStore{tmpl: &TemplateDef{
		Subject: "override {{name}}",
		Body:    "<p>override {{name}}</p>",
	}}
	sn := &fakeSvcSender{id: "prov-1"}
	svc := newTestService(st, sn, "key")
	svc.Send(context.Background(), "k", "a@b.c", map[string]string{"name": `A&B<c>`})
	svc.Wait()

	_, _, subject, html := sn.seen()
	if subject != `override A&B<c>` {
		t.Fatalf("store subject must win over the registry and be substituted raw, got %q", subject)
	}
	if html != `<p>override A&amp;B&lt;c&gt;</p>` {
		t.Fatalf("store body must win over the registry and be escaped, got %q", html)
	}
	if strings.Contains(subject, "registry-") || strings.Contains(html, "registry-") {
		t.Fatal("registry template must not be consulted when the store overrides")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.tmplKey != "k" {
		t.Fatalf("store must be asked for the requested key, got %q", st.tmplKey)
	}
}

// TestSendRaw_ReachesChokepointAndLogs covers the second exported entry point:
// it must pass the same suppression → creds → send → log path.
func TestSendRaw_ReachesChokepointAndLogs(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{id: "prov-raw"}
	svc := newTestService(st, sn, "key")
	svc.SendRaw(context.Background(), "raw@example.com", "raw-subject", "<p>raw</p>", "raw-label")
	svc.Wait()

	calls, to, subject, html := sn.seen()
	if calls != 1 || to != "raw@example.com" || subject != "raw-subject" || html != "<p>raw</p>" {
		t.Fatalf("SendRaw must pass its arguments through verbatim, got %d %q %q %q", calls, to, subject, html)
	}
	logs, queried := st.snapshot()
	if len(queried) != 1 || queried[0] != "raw@example.com" {
		t.Fatalf("SendRaw must pass the suppression check, queried %q", queried)
	}
	if len(logs) != 1 || logs[0].Status != StatusSent {
		t.Fatalf("SendRaw must write an audit row, got %+v", logs)
	}
	if logs[0].To != "raw@example.com" || logs[0].Subject != "raw-subject" || logs[0].Type != "raw-label" {
		t.Fatalf("SendRaw audit row fields wrong: %+v", logs[0])
	}
}

// TestSendRaw_SuppressedNeverSends proves SendRaw is not a second door around
// the chokepoint.
func TestSendRaw_SuppressedNeverSends(t *testing.T) {
	st := &fakeStore{suppressed: map[string]bool{"raw@example.com": true}}
	sn := &fakeSvcSender{}
	svc := newTestService(st, sn, "key")
	svc.SendRaw(context.Background(), "RAW@example.com", "s", "h", "raw-label")
	svc.Wait()

	if calls, _, _, _ := sn.seen(); calls != 0 {
		t.Fatal("SendRaw must not bypass suppression")
	}
}

// TestLogging_UsesDomainOnlyNotFullAddress pins the PII guarantee: the audit
// row holds the address under the project's retention rules, application logs
// must not give it a second, unmanaged lifetime.
func TestLogging_UsesDomainOnlyNotFullAddress(t *testing.T) {
	const addr = "secret.user@example.com"

	cases := []struct {
		name  string
		store *fakeStore
		send  *fakeSvcSender
	}{
		{"send failure", &fakeStore{}, &fakeSvcSender{err: errors.New("boom")}},
		{"suppression lookup failure", &fakeStore{supErr: errors.New("db down")}, &fakeSvcSender{}},
		{"audit write failure", &fakeStore{logErr: errors.New("write failed")}, &fakeSvcSender{id: "p1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t)
			svc := newTestService(tc.store, tc.send, "key")
			svc.Send(context.Background(), "k", addr, nil)
			svc.Wait()

			out := buf.String()
			if out == "" {
				t.Fatal("want a log line, got none")
			}
			if strings.Contains(out, addr) {
				t.Fatalf("log must never carry the full recipient address, got %q", out)
			}
			if !strings.Contains(out, "domain=example.com") {
				t.Fatalf("log must carry the domain only, got %q", out)
			}
		})
	}
}

// TestFire_PanicIsContainedAndLogged pins Fix 3: the panic must not escape into
// the caller's request, and must not vanish without a trace either.
func TestFire_PanicIsContainedAndLogged(t *testing.T) {
	buf := captureLogs(t)
	st := &panicStore{}
	svc := NewServiceWithSender(st, Config{APIKey: "key"}, Registry{}, &fakeSvcSender{})
	svc.SendRaw(context.Background(), "boom@example.com", "s", "h", "lbl")
	svc.Wait() // must return: the panic was contained, not propagated

	out := buf.String()
	if !strings.Contains(out, "email send panicked") {
		t.Fatalf("want the recovered panic logged, got %q", out)
	}
	if !strings.Contains(out, "store exploded") {
		t.Fatalf("want the panic value logged, got %q", out)
	}
	if !strings.Contains(out, "stack=") {
		t.Fatalf("want a stack in the log, got %q", out)
	}
	if strings.Contains(out, "boom@example.com") {
		t.Fatalf("panic log must carry the domain only, got %q", out)
	}
}

type panicStore struct{ fakeStore }

func (p *panicStore) IsSuppressed(context.Context, string) (bool, error) {
	panic("store exploded")
}

func TestDeliver_SendFailLogsFailed(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{err: errors.New("boom")}
	svc := newTestService(st, sn, "key")
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()
	logs, _ := st.snapshot()
	if len(logs) != 1 || logs[0].Status != StatusFailed {
		t.Fatalf("want one failed log, got %+v", logs)
	}
	if logs[0].Error == nil || *logs[0].Error != "boom" {
		t.Fatal("failure reason must be recorded")
	}
}

func TestStrpOrNil(t *testing.T) {
	if got := strpOrNil(""); got != nil {
		t.Fatalf("empty provider id must be nil, not a pointer to \"\", got %q", *got)
	}
	got := strpOrNil("abc")
	if got == nil || *got != "abc" {
		t.Fatalf("want pointer to %q, got %v", "abc", got)
	}
}

func TestDomainOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"user@example.com", "example.com"},
		{"a@b@example.com", "example.com"}, // last @ wins
		{"no-at-sign", "invalid"},          // must not panic or index out of range
		{"", "invalid"},
		{"trailing@", ""},
	} {
		if got := domainOf(tc.in); got != tc.want {
			t.Fatalf("domainOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
