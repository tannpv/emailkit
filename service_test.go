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

// newTestService takes a Store, not a *fakeStore, so the panicking and
// blocking doubles below reuse it instead of open-coding a constructor each.
func newTestService(st Store, sn *fakeSvcSender, key string) *Service {
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
	for _, reason := range []string{reasonNotConfigured, reasonSuppressed, reasonSuppressionLookup} {
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
