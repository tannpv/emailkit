package emailkit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
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

	// marks + suppress record the webhook path (webhook_test.go). They live on
	// the one fake rather than a second webhook-only fake, because Store is a
	// single port and two fakes would be two things to keep in step with it.
	//
	// markErr/suppressErr inject a failure into those two calls. The call is
	// still RECORDED before the error is returned, which is what lets a test
	// assert the handler attempted the second store call after the first one
	// failed.
	marks       []markCall
	markErr     error
	suppress    []suppressCall
	suppressErr error
}

// markCall + suppressCall capture what the webhook handler asked the port to do.
type markCall struct {
	id, status string
	reason     *string
}
type suppressCall struct{ email, reason string }

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

func (f *fakeStore) MarkByProviderID(_ context.Context, providerID, status string, reason *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks = append(f.marks, markCall{id: providerID, status: status, reason: reason})
	return f.markErr
}

// Suppress records the call AND applies it to the same map IsSuppressed reads,
// with an EXACT-MATCH key. That is what makes this fake a faithful model of a
// real store: emailkit's Store contract says addresses arrive already
// normalised, so an implementation is entitled to a plain `WHERE email = $1`.
// A fake that lowercased here would absorb the very asymmetry the suppression
// tests exist to detect and pass no matter which side forgot to normalise.
func (f *fakeStore) Suppress(_ context.Context, email, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suppress = append(f.suppress, suppressCall{email: email, reason: reason})
	if f.suppressErr != nil {
		return f.suppressErr
	}
	if f.suppressed == nil {
		f.suppressed = map[string]bool{}
	}
	f.suppressed[email] = true
	return nil
}

func (f *fakeStore) snapshot() ([]SendRecord, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SendRecord(nil), f.logs...), append([]string(nil), f.queried...)
}

// fakeSvcSender is named distinctly from sender_test.go's fakeSender (same
// package, both are _test.go files) to avoid a redeclaration error. It records
// its arguments so tests can assert what actually reached the provider.
type fakeSvcSender struct {
	mu                   sync.Mutex
	calls                int
	id                   string
	err                  error
	to                   string
	subject              string
	html                 string
	lastAPIKey, lastFrom string
}

func (s *fakeSvcSender) Send(_ context.Context, apiKey, from, to, subject, html string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastAPIKey, s.lastFrom = apiKey, from
	s.to, s.subject, s.html = to, subject, html
	return s.id, s.err
}

func (s *fakeSvcSender) seen() (int, string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.to, s.subject, s.html
}

// newTestService takes the two ports, not the concrete fakes, so every double
// below (panicking store, blocking store, context-aware pair) reuses it instead
// of open-coding a constructor each.
func newTestService(st Store, sn Sender, key string) *Service {
	return NewServiceWithSender(st, Config{APIKey: key, From: "T <t@example.com>"},
		Registry{"k": {Subject: "registry-subject", Body: "registry-body"}}, sn)
}

// captureLogs points one Service at a buffer-backed logger and returns the
// buffer. It injects rather than calling slog.SetDefault: the process-wide
// default is shared state, and mutating it would make every test in this
// package silently incompatible with t.Parallel().
//
// Read the buffer only after svc.Wait(): sends log from their own goroutine,
// and Wait is the happens-before edge that makes those writes visible.
func captureLogs(t *testing.T, svc *Service) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	svc.logger = slog.New(slog.NewTextHandler(&buf, nil))
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

// TestDeliver_SuppressionLookupIsCaseInsensitive pins the normalizeAddress call
// in deliver(). The suppression entry is lowercase and the recipient is not:
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

// TestNormalizeAddress pins the policy itself, including its deliberate limits.
// The "unchanged" rows are the load-bearing ones: they fail if someone later
// adds "+tag" stripping or Gmail dot-folding, which would suppress addresses
// that never bounced and lock live users out of password resets.
func TestNormalizeAddress(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"already normalised", "user@example.com", "user@example.com"},
		{"mixed case", "User@Example.COM", "user@example.com"},
		{"surrounding spaces", "  user@example.com  ", "user@example.com"},
		{"tab and newline", "\tuser@example.com\n", "user@example.com"},
		{"whitespace only", "   ", ""},
		{"empty", "", ""},
		{"plus tag is part of the identity", "User+news@Example.com", "user+news@example.com"},
		{"dots are part of the identity", "First.Last@Example.com", "first.last@example.com"},
		{"inner space is not trimmed", "us er@example.com", "us er@example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeAddress(c.in); got != c.want {
				t.Fatalf("normalizeAddress(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestDeliver_SuppressionLookupErrorWithholdsSend is the Fix 2 regression: an
// errored lookup used to fall through and send.
func TestDeliver_SuppressionLookupErrorWithholdsSend(t *testing.T) {
	st := &fakeStore{supErr: errors.New("db connection refused")}
	sn := &fakeSvcSender{}
	svc := newTestService(st, sn, "key")
	buf := captureLogs(t, svc)
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
}

// TestReasons_AreProviderNeutral guards the constants themselves. It used to be
// a strings.Contains on the value the previous test had just asserted equal to
// reasonNotConfigured, so it could only fire if someone edited the constant —
// which is precisely the change worth catching, and it belongs here where it
// covers all three reasons independently of any send path.
//
// Sender is explicitly the seam for SES or Postmark, so a reason naming a
// provider is wrong the moment a consumer supplies its own implementation.
func TestReasons_AreProviderNeutral(t *testing.T) {
	providers := []string{"Resend", "SES", "Postmark", "SendGrid", "Mailgun"}
	reasons := []string{
		reasonNotConfigured, reasonSuppressed, reasonSuppressionLookup,
		reasonNothingToSend, reasonPanicked,
	}
	for _, reason := range reasons {
		for _, p := range providers {
			if strings.Contains(strings.ToLower(reason), strings.ToLower(p)) {
				t.Fatalf("reason %q names provider %q — reasons must be provider-neutral", reason, p)
			}
		}
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

// TestSend_UnknownKeyIsNothingToSend pins the contract Registry.Render
// documents: an unknown key means "nothing to send". deliver used to ignore
// that and hand the provider two empty strings, so a typo'd key mailed a blank
// email and the audit row recorded it as StatusSent.
func TestSend_UnknownKeyIsNothingToSend(t *testing.T) {
	st := &fakeStore{} // no override, so Template reports !ok
	sn := &fakeSvcSender{}
	svc := newTestService(st, sn, "key") // its Registry holds "k" and nothing else
	svc.Send(context.Background(), "no-such-key", "a@b.c", nil)
	svc.Wait()

	if calls, _, _, _ := sn.seen(); calls != 0 {
		t.Fatal("a key in neither the store nor the registry must not reach the sender — that mails a blank email")
	}
	logs, _ := st.snapshot()
	if len(logs) != 1 {
		t.Fatalf("want exactly one audit row, got %+v", logs)
	}
	if logs[0].Status != StatusSkipped {
		t.Fatalf("want %q, got %q — a blank email must never be recorded as sent", StatusSkipped, logs[0].Status)
	}
	if logs[0].Error == nil || *logs[0].Error != reasonNothingToSend {
		t.Fatalf("skip reason must name the empty render, got %v", logs[0].Error)
	}
	if logs[0].Type != "no-such-key" {
		t.Fatalf("audit row must name the key that could not be resolved, got %q", logs[0].Type)
	}
}

// TestSend_OnlyBothEmptyIsNothingToSend pins the width of the rule above. A
// subject with no body is a real email — "your export is ready" with the whole
// message in the subject line — and withholding it would be a worse bug than
// the one being fixed.
func TestSend_OnlyBothEmptyIsNothingToSend(t *testing.T) {
	for _, tc := range []struct {
		name       string
		def        TemplateDef
		wantSends  int
		wantStatus string
	}{
		{"subject only", TemplateDef{Subject: "subject-only"}, 1, StatusSent},
		{"body only", TemplateDef{Body: "<p>body-only</p>"}, 1, StatusSent},
		{"both empty", TemplateDef{}, 0, StatusSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			sn := &fakeSvcSender{id: "prov-1"}
			svc := NewServiceWithSender(st, Config{APIKey: "key", From: "T <t@example.com>"},
				Registry{"k": tc.def}, sn)
			svc.Send(context.Background(), "k", "a@b.c", nil)
			svc.Wait()

			if calls, _, _, _ := sn.seen(); calls != tc.wantSends {
				t.Fatalf("want %d sends, got %d", tc.wantSends, calls)
			}
			logs, _ := st.snapshot()
			if len(logs) != 1 || logs[0].Status != tc.wantStatus {
				t.Fatalf("want one %q row, got %+v", tc.wantStatus, logs)
			}
		})
	}
}

// TestSendRaw_NothingToSendAppliesToRawToo states the deliberate decision that
// the rule lives at the chokepoint and therefore covers SendRaw. The CAUSE
// differs — a caller's own bug, not an unresolvable key — but the OUTCOME is
// the same blank email, and the outcome is what the recipient gets.
func TestSendRaw_NothingToSendAppliesToRawToo(t *testing.T) {
	for _, tc := range []struct {
		name, subject, html string
		wantSends           int
		wantStatus          string
	}{
		{"both empty", "", "", 0, StatusSkipped},
		{"subject only", "raw-subject", "", 1, StatusSent},
		{"body only", "", "<p>raw</p>", 1, StatusSent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			sn := &fakeSvcSender{id: "prov-raw"}
			svc := newTestService(st, sn, "key")
			svc.SendRaw(context.Background(), "raw@example.com", tc.subject, tc.html, "raw-label")
			svc.Wait()

			if calls, _, _, _ := sn.seen(); calls != tc.wantSends {
				t.Fatalf("want %d sends, got %d", tc.wantSends, calls)
			}
			logs, _ := st.snapshot()
			if len(logs) != 1 || logs[0].Status != tc.wantStatus {
				t.Fatalf("want one %q row, got %+v", tc.wantStatus, logs)
			}
			if tc.wantStatus == StatusSkipped &&
				(logs[0].Error == nil || *logs[0].Error != reasonNothingToSend) {
				t.Fatalf("skip reason must name the empty pair, got %v", logs[0].Error)
			}
		})
	}
}

// ctxAwareStore behaves the way a real *sql.DB-backed Store does: every call
// fails fast on a context that is already cancelled. The plain fakeStore
// ignores ctx entirely, which is why the whole suite stayed green with
// context.WithoutCancel removed — nothing was watching.
type ctxAwareStore struct{ fakeStore }

func (c *ctxAwareStore) IsSuppressed(ctx context.Context, email string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return c.fakeStore.IsSuppressed(ctx, email)
}

func (c *ctxAwareStore) LogSend(ctx context.Context, r SendRecord) error {
	if err := ctx.Err(); err != nil {
		return err // a cancelled context writes no audit row
	}
	return c.fakeStore.LogSend(ctx, r)
}

// ctxAwareSender records the call before consulting ctx, so the fake observes
// the attempt either way and the assertions can tell "never reached" apart
// from "reached with a dead context".
type ctxAwareSender struct{ fakeSvcSender }

func (c *ctxAwareSender) Send(ctx context.Context, key, from, to, subject, html string) (string, error) {
	id, err := c.fakeSvcSender.Send(ctx, key, from, to, subject, html)
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	return id, err
}

// TestFire_SendSurvivesCallerContextCancellation pins the one line the
// fire-and-forget design rests on: context.WithoutCancel in fire(). The HTTP
// request that triggered the mail routinely returns — and cancels its ctx —
// before the send goroutine gets scheduled. Passing that ctx straight through
// means the store lookup, the provider call and the audit write all fail with
// context.Canceled and the mail silently never happens.
//
// The context here is cancelled BEFORE Send is even called, which is the
// worst case and removes any scheduling race from the test.
func TestFire_SendSurvivesCallerContextCancellation(t *testing.T) {
	st := &ctxAwareStore{}
	sn := &ctxAwareSender{fakeSvcSender{id: "prov-1"}}
	svc := newTestService(st, sn, "key")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc.Send(ctx, "k", "a@b.c", nil)
	svc.Wait()

	calls, to, subject, _ := sn.seen()
	if calls != 1 {
		t.Fatalf("want 1 send after the caller's context was cancelled, got %d — fire() must detach the context", calls)
	}
	if to != "a@b.c" || subject != "registry-subject" {
		t.Fatalf("the detached send must carry the same arguments, got %q %q", to, subject)
	}
	logs, queried := st.snapshot()
	if len(queried) != 1 {
		t.Fatalf("the suppression lookup must still run on a live context, queried %q", queried)
	}
	if len(logs) != 1 || logs[0].Status != StatusSent {
		t.Fatalf("want one sent audit row written on a live context, got %+v", logs)
	}
	if logs[0].ProviderID == nil || *logs[0].ProviderID != "prov-1" {
		t.Fatalf("provider id must survive too, got %v", logs[0].ProviderID)
	}
}

// TestLogging_UsesDomainOnlyNotFullAddress pins the PII guarantee that emailkit
// can actually keep: no field emailkit controls carries the address. The audit
// row holds it under the project's retention rules; application logs must not
// give it a second, unmanaged lifetime.
//
// Deliberately NOT pinned here: the "err" value. That text comes from a Store
// or Sender the consumer wrote, and emailkit logs it verbatim — keeping the
// address out of it is the implementation's contract (see Store), not
// something this package can enforce. The fixtures below honour that contract,
// which is why the assertion below holds.
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
			svc := newTestService(tc.store, tc.send, "key")
			buf := captureLogs(t, svc)
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
	st := &panicStore{}
	svc := NewServiceWithSender(st, Config{APIKey: "key"}, Registry{}, &fakeSvcSender{})
	buf := captureLogs(t, svc)
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

	// fire()'s comment promises "contain it AND record it": a log line alone
	// is only seen by someone already looking, so the mail must leave a row.
	logs, _ := st.snapshot()
	if len(logs) != 1 || logs[0].Status != StatusFailed {
		t.Fatalf("want one failed audit row for the panicked send, got %+v", logs)
	}
	if logs[0].Error == nil || *logs[0].Error != reasonPanicked {
		t.Fatalf("audit reason must name the panic, got %v", logs[0].Error)
	}
	// The recovered value is consumer text and may embed the address; it
	// belongs in the log line (with its stack), not in a second copy on the row.
	if strings.Contains(*logs[0].Error, "store exploded") {
		t.Fatalf("audit row must not carry the recovered value, got %q", *logs[0].Error)
	}
}

type panicStore struct{ fakeStore }

func (p *panicStore) IsSuppressed(context.Context, string) (bool, error) {
	panic("store exploded")
}

// templateStore controls the timing and failure mode of Store.Template — the
// one piece of consumer code the templated send path runs before deliver().
// entered/release make "Template is executing right now" an assertable state
// rather than a guess; everything else comes from the embedded fakeStore.
type templateStore struct {
	fakeStore
	entered chan struct{} // closed on entry when non-nil
	release chan struct{} // Template parks until this is closed, when non-nil
	delay   time.Duration // unsynchronised pause; see TestSend_CopiesVars...
	explode bool
}

func (t *templateStore) Template(ctx context.Context, key string) (string, string, bool) {
	if t.entered != nil {
		close(t.entered)
	}
	if t.explode {
		panic("template exploded")
	}
	if t.release != nil {
		<-t.release
	}
	if t.delay > 0 {
		time.Sleep(t.delay)
	}
	return t.fakeStore.Template(ctx, key)
}

// sendReturnTimeout bounds "Send returned promptly". Generous on purpose: a
// loaded CI box being slow to schedule a goroutine is not the failure under
// test, a Send that never returns is.
const sendReturnTimeout = 2 * time.Second

// TestSend_TemplatePanicIsContainedAndLogged is the templated path's half of
// the fire-and-forget contract. render() used to run on the CALLER'S goroutine,
// so a panicking Store.Template took down the HTTP request that triggered the
// mail; fire()'s recover() only ever covered deliver(). This test has no
// recover() of its own by design — if the panic escapes, the test binary dies,
// which is exactly the production symptom.
func TestSend_TemplatePanicIsContainedAndLogged(t *testing.T) {
	st := &templateStore{explode: true}
	sn := &fakeSvcSender{}
	svc := newTestService(st, sn, "key")
	buf := captureLogs(t, svc)

	svc.Send(context.Background(), "k", "boom@example.com", nil)
	svc.Wait()

	out := buf.String()
	if !strings.Contains(out, "email send panicked") {
		t.Fatalf("want the recovered render panic logged, got %q", out)
	}
	if !strings.Contains(out, "template exploded") {
		t.Fatalf("want the panic value logged, got %q", out)
	}
	if !strings.Contains(out, "domain=example.com") {
		t.Fatalf("want the domain logged, got %q", out)
	}
	if strings.Contains(out, "boom@example.com") {
		t.Fatalf("panic log must carry the domain only, got %q", out)
	}
	if calls, _, _, _ := sn.seen(); calls != 0 {
		t.Fatal("a render that panicked must not reach the sender")
	}
	logs, _ := st.snapshot()
	if len(logs) != 1 || logs[0].Status != StatusFailed {
		t.Fatalf("want one failed audit row for the panicked render, got %+v", logs)
	}
	// Nothing rendered, so there is no subject to record. An honestly blank
	// field beats an invented one.
	if logs[0].Subject != "" {
		t.Fatalf("a panicked render has no subject to record, got %q", logs[0].Subject)
	}
}

// TestSend_BlockingTemplateDoesNotBlockSend is the other half: a Store.Template
// that is slow (or hung on a database) must not hold the caller's request open.
// With the render back on the caller's goroutine this deadlocks until the
// timeout below fires.
func TestSend_BlockingTemplateDoesNotBlockSend(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	// Cleanup rather than a timeout inside the store: if an assertion below
	// fails, this still frees the parked send goroutine.
	t.Cleanup(unblock)

	st := &templateStore{entered: entered, release: release}
	sn := &fakeSvcSender{id: "prov-1"}
	svc := newTestService(st, sn, "key")

	returned := make(chan struct{})
	go func() {
		svc.Send(context.Background(), "k", "a@b.c", nil)
		close(returned)
	}()

	<-entered // Store.Template is now executing and parked on release
	select {
	case <-returned:
	case <-time.After(sendReturnTimeout):
		t.Fatal("Send did not return while Store.Template was blocked — the render is on the caller's goroutine")
	}

	unblock()
	svc.Wait()
	if calls, _, _, _ := sn.seen(); calls != 1 {
		t.Fatalf("the send must still complete once the store answers, got %d calls", calls)
	}
}

// varsRenderDelay holds the render open long enough that a non-copying
// implementation would substitute the caller's MUTATED value. It is a delay,
// not a synchroniser: introducing a happens-before edge here (channel, mutex)
// would order the caller's write ahead of the goroutine's read and hide the
// very race -race exists to catch.
const varsRenderDelay = 20 * time.Millisecond

// TestSend_CopiesVarsSoCallerCanReuseTheMap pins the copy in Send. Moving the
// render onto the send goroutine handed it a map the caller still owns, so
// without the copy the caller's next write races the substitution. Two
// failures are pinned at once: the wrong rendered value (assertions below) and
// the data race itself (under -race).
func TestSend_CopiesVarsSoCallerCanReuseTheMap(t *testing.T) {
	const original, mutated = "original", "mutated"
	st := &templateStore{delay: varsRenderDelay}
	sn := &fakeSvcSender{id: "prov-1"}
	svc := NewServiceWithSender(st, Config{APIKey: "key", From: "T <t@example.com>"},
		Registry{"k": {Subject: "hello {{name}}", Body: "<p>{{name}}</p>"}}, sn)

	vars := map[string]string{"name": original}
	svc.Send(context.Background(), "k", "a@b.c", vars)
	vars["name"] = mutated // the caller owns this map and may reuse it at once

	svc.Wait()
	_, _, subject, html := sn.seen()
	if subject != "hello "+original {
		t.Fatalf("subject must render the value vars held when Send was called, got %q", subject)
	}
	if html != "<p>"+original+"</p>" {
		t.Fatalf("body must render the value vars held when Send was called, got %q", html)
	}
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
		// A trailing '@' is as malformed as no '@' at all and reports the
		// same way. It used to yield "", which logged a bare `domain=` an
		// operator cannot distinguish from a field that was never set.
		{"trailing@", "invalid"},
	} {
		if got := domainOf(tc.in); got != tc.want {
			t.Fatalf("domainOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// doublePanicStore panics on BOTH store calls the panic path makes: the
// suppression lookup that begins the send, and the audit write that records the
// resulting panic. That is the exact shape auditPanic's inner recover exists
// for — a Store broken enough to panic once is very likely to panic again on
// the next call — and without it the second panic is raised inside a deferred
// recover, which no further recover can catch.
type doublePanicStore struct{ fakeStore }

func (d *doublePanicStore) IsSuppressed(context.Context, string) (bool, error) {
	panic("suppression lookup exploded")
}

func (d *doublePanicStore) LogSend(context.Context, SendRecord) error {
	panic("audit write exploded")
}

// TestFire_PanicInPanicAuditDoesNotKillTheProcess pins the inner recover in
// auditPanic. Deleting that defer leaves the rest of the suite green — every
// other panic test uses a Store that panics exactly once, and by then the
// recover has already fired — while the failure it hides is total: the process
// dies. There is no recover() here on purpose; if the second panic escapes, the
// test binary dies, which is precisely the production symptom.
func TestFire_PanicInPanicAuditDoesNotKillTheProcess(t *testing.T) {
	st := &doublePanicStore{}
	svc := NewServiceWithSender(st, Config{APIKey: "key"}, Registry{}, &fakeSvcSender{})
	buf := captureLogs(t, svc)

	svc.SendRaw(context.Background(), "boom@example.com", "s", "h", "lbl")

	// Wait is asserted to RETURN, not merely called: a send goroutine that died
	// mid-defer would never run wg.Done, so a plain svc.Wait() would hang the
	// suite instead of failing it.
	waited := make(chan struct{})
	go func() { defer close(waited); svc.Wait() }()
	select {
	case <-waited:
	case <-time.After(sendReturnTimeout):
		t.Fatal("Wait() must return: both panics were supposed to be contained")
	}

	out := buf.String()
	if !strings.Contains(out, "email send panicked") {
		t.Fatalf("want the first panic logged, got %q", out)
	}
	if !strings.Contains(out, "email panic audit write panicked") {
		t.Fatalf("want the audit-write panic logged too, got %q", out)
	}
	if strings.Contains(out, "boom@example.com") {
		t.Fatalf("neither panic line may carry the full address, got %q", out)
	}
}

// ctxAwarePanicStore panics on the suppression lookup like panicStore, and —
// like a real *sql.DB-backed Store — refuses to write on a dead context. The
// combination is what makes the context auditPanic runs on observable.
type ctxAwarePanicStore struct{ ctxAwareStore }

func (c *ctxAwarePanicStore) IsSuppressed(context.Context, string) (bool, error) {
	panic("store exploded on an already-returned request")
}

// TestFire_PanicAuditWritesOnTheDetachedContext pins the ordering inside fire():
// sendCtx is resolved BEFORE the recover is registered, so the panic path has a
// live context to audit on.
//
// Nothing else in the suite covers a cancelled caller AND a panic together —
// TestFire_SendSurvivesCallerContextCancellation never panics, and every panic
// test runs on context.Background() — so handing auditPanic the caller's ctx
// keeps the whole suite green. In production it loses the audit row for every
// panicking send whose request has already returned, which for a fire-and-forget
// send is the normal case, not the edge case.
func TestFire_PanicAuditWritesOnTheDetachedContext(t *testing.T) {
	st := &ctxAwarePanicStore{}
	svc := NewServiceWithSender(st, Config{APIKey: "key"}, Registry{}, &fakeSvcSender{})
	buf := captureLogs(t, svc)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the request returned before the send goroutine was scheduled

	svc.SendRaw(ctx, "boom@example.com", "s", "h", "lbl")
	svc.Wait()

	logs, _ := st.snapshot()
	if len(logs) != 1 || logs[0].Status != StatusFailed {
		t.Fatalf("the panic audit row must be written on the detached context, got %+v", logs)
	}
	if logs[0].Error == nil || *logs[0].Error != reasonPanicked {
		t.Fatalf("audit reason must name the panic, got %v", logs[0].Error)
	}
	if strings.Contains(buf.String(), "email audit write failed") {
		t.Fatalf("the panic audit write must not be refused by a cancelled context, got %q", buf.String())
	}
}

// selfFormattingPanic is a panic value whose String() panics — and panics with
// a value of its own type, which defeats fmt's own containment too: fmt
// normally prints "%!v(PANIC=String method: …)", but re-panics when formatting
// the recovered value panics in turn. A consumer's panic value is arbitrary, so
// this is reachable from outside the module.
type selfFormattingPanic struct{}

func (selfFormattingPanic) String() string { panic(selfFormattingPanic{}) }

// unprintablePanicStore panics with a value that cannot be rendered.
type unprintablePanicStore struct{ fakeStore }

func (u *unprintablePanicStore) IsSuppressed(context.Context, string) (bool, error) {
	panic(selfFormattingPanic{})
}

// TestFire_UnprintablePanicValueIsContained pins panicText. Logging the
// recovered value directly defers its formatting to the slog handler, which
// runs OUTSIDE fire()'s recover — so this panic value escapes there and kills
// the process, defeating the exact guarantee fire() exists to provide. Again no
// recover() here: an escape kills the test binary, as it would the server.
// TestDeliver_ResolverSuppliesCredentials pins that a non-nil Resolve REPLACES
// the static Config fields rather than layering over them.
func TestDeliver_ResolverSuppliesCredentials(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{id: "p1"}
	cfg := Config{
		APIKey: "static-key",
		From:   "static@example.com",
		Resolve: func(context.Context) (string, string, error) {
			return "resolved-key", "resolved@example.com", nil
		},
	}
	svc := NewServiceWithSender(st, cfg, Registry{"k": {Subject: "s", Body: "b"}}, sn)
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()

	if sn.lastAPIKey != "resolved-key" {
		t.Fatalf("resolver must win over the static key, got %q", sn.lastAPIKey)
	}
	if sn.lastFrom != "resolved@example.com" {
		t.Fatalf("resolver must win over the static from, got %q", sn.lastFrom)
	}
}

// TestDeliver_ResolverErrorSkipsWithoutSending is the load-bearing case: a
// Resolve error must skip the send rather than falling back to the static
// APIKey/From. Sending with credentials the operator believes they replaced
// is worse than not sending.
func TestDeliver_ResolverErrorSkipsWithoutSending(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{}
	cfg := Config{
		APIKey: "static-key",
		Resolve: func(context.Context) (string, string, error) {
			return "", "", errors.New("settings table unreachable")
		},
	}
	svc := NewServiceWithSender(st, cfg, Registry{"k": {Subject: "s", Body: "b"}}, sn)
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()

	// Falling back to the static key would send with credentials the operator
	// believes they replaced — worse than not sending.
	if sn.calls != 0 {
		t.Fatal("a resolver error must not fall back to the static credentials")
	}
	if len(st.logs) != 1 || st.logs[0].Status != StatusSkipped {
		t.Fatalf("want one skipped row, got %+v", st.logs)
	}
}

// TestDeliver_NilResolverUsesStaticConfig pins that nil Resolve preserves
// v0.1.0 behaviour exactly: liseuse and bacnam pass static credentials and
// must be unaffected by this change.
func TestDeliver_NilResolverUsesStaticConfig(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{id: "p1"}
	svc := NewServiceWithSender(st, Config{APIKey: "static-key", From: "static@example.com"},
		Registry{"k": {Subject: "s", Body: "b"}}, sn)
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()

	if sn.lastAPIKey != "static-key" || sn.lastFrom != "static@example.com" {
		t.Fatalf("nil Resolve must use the static fields, got %q / %q", sn.lastAPIKey, sn.lastFrom)
	}
}

func TestFire_UnprintablePanicValueIsContained(t *testing.T) {
	st := &unprintablePanicStore{}
	svc := NewServiceWithSender(st, Config{APIKey: "key"}, Registry{}, &fakeSvcSender{})
	buf := captureLogs(t, svc)

	svc.SendRaw(context.Background(), "boom@example.com", "s", "h", "lbl")

	waited := make(chan struct{})
	go func() { defer close(waited); svc.Wait() }()
	select {
	case <-waited:
	case <-time.After(sendReturnTimeout):
		t.Fatal("Wait() must return: formatting the panic value must not escape")
	}

	out := buf.String()
	if !strings.Contains(out, "email send panicked") {
		t.Fatalf("want the panic logged despite the unprintable value, got %q", out)
	}
	if !strings.Contains(out, panicUnprintable) {
		t.Fatalf("want the unprintable placeholder in place of the value, got %q", out)
	}
	// The rest of the panic path must be unaffected: a value nobody can print
	// is still a send that must leave a row.
	logs, _ := st.snapshot()
	if len(logs) != 1 || logs[0].Status != StatusFailed {
		t.Fatalf("an unprintable panic must still leave an audit row, got %+v", logs)
	}
	if logs[0].Error == nil || *logs[0].Error != reasonPanicked {
		t.Fatalf("audit reason must name the panic, got %v", logs[0].Error)
	}
}
