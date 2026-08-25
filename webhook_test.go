package emailkit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A real base64 key. verify() strips an optional whsec_ prefix and
// base64-decodes the remainder as the HMAC key.
const testSecret = "whsec_c3VwZXJzZWNyZXR0ZXN0a2V5MTIzNDU2"

// testNow freezes the clock for the ported cases. They sign at this instant and
// the handler reads it back through WithClock, so a suite that used to sign a
// fixed 2023 timestamp keeps passing now that timestamps are validated — the
// window is asserted by the three replay tests, not by wall-clock luck.
var testNow = time.Unix(1_700_000_000, 0)

func signedRequest(t *testing.T, secret string, ts time.Time, body string) *http.Request {
	t.Helper()
	id := "msg_test"
	tss := strconv.FormatInt(ts.Unix(), 10)
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		t.Fatalf("bad test secret: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + tss + "." + body))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/resend", strings.NewReader(body))
	req.Header.Set("svix-id", id)
	req.Header.Set("svix-timestamp", tss)
	req.Header.Set("svix-signature", "v1,"+sig)
	return req
}

// frozenWebhook is the ported cases' constructor: the clock is pinned to
// testNow so they exercise event handling, not the replay window.
func frozenWebhook(st Store, secret string) *WebhookHandler {
	return NewWebhookHandler(st, secret, WithClock(func() time.Time { return testNow }))
}

func serve(h *WebhookHandler, r *http.Request) (*httptest.ResponseRecorder, error) {
	w := httptest.NewRecorder()
	return w, h.Handle(w, r)
}

func TestWebhook_BadSignature(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		body   string
		mutate func(*http.Request)
	}{
		{name: "empty secret", secret: "", body: `{}`},
		{name: "non-whsec secret", secret: "nope", body: `{}`},
		{
			name:   "wrong signature",
			secret: testSecret,
			body:   `{}`,
			mutate: func(r *http.Request) { r.Header.Set("svix-signature", "v1,deadbeef") },
		},
		{
			name:   "missing svix headers",
			secret: testSecret,
			body:   `{}`,
			mutate: func(r *http.Request) {
				r.Header.Del("svix-id")
				r.Header.Del("svix-timestamp")
				r.Header.Del("svix-signature")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := frozenWebhook(&fakeStore{}, c.secret)
			r := signedRequest(t, testSecret, testNow, c.body)
			if c.mutate != nil {
				c.mutate(r)
			}
			w, err := serve(h, r)
			if !errors.Is(err, ErrBadSignature) {
				t.Fatalf("err = %v, want %v", err, ErrBadSignature)
			}
			if w.Body.Len() != 0 {
				t.Fatalf("rejected request wrote a body: %q", w.Body.String())
			}
		})
	}
}

func TestWebhook_MalformedJSON(t *testing.T) {
	h := frozenWebhook(&fakeStore{}, testSecret)
	w, err := serve(h, signedRequest(t, testSecret, testNow, `{not json`))
	if !errors.Is(err, ErrBadPayload) {
		t.Fatalf("err = %v, want %v", err, ErrBadPayload)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("rejected request wrote a body: %q", w.Body.String())
	}
}

func TestWebhook_Delivered(t *testing.T) {
	f := &fakeStore{}
	h := frozenWebhook(f, testSecret)
	body := `{"type":"email.delivered","data":{"email_id":"e_123","to":["a@x.com"]}}`
	w, err := serve(h, signedRequest(t, testSecret, testNow, body))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["received"] != true {
		t.Fatalf("received = %v, want true", resp["received"])
	}
	if len(f.marks) != 1 {
		t.Fatalf("marks = %d, want 1", len(f.marks))
	}
	m := f.marks[0]
	if m.id != "e_123" || m.status != "delivered" || m.reason != nil {
		t.Fatalf("mark = %+v, want {e_123 delivered <nil>}", m)
	}
	if len(f.suppress) != 0 {
		t.Fatalf("unexpected suppress: %+v", f.suppress)
	}
}

func TestWebhook_BouncedPermanent(t *testing.T) {
	f := &fakeStore{}
	h := frozenWebhook(f, testSecret)
	body := `{"type":"email.bounced","data":{"email_id":"e_9","to":"hard@x.com","bounce":{"type":"Permanent","subType":"General","message":"mailbox does not exist"}}}`
	w, err := serve(h, signedRequest(t, testSecret, testNow, body))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(f.marks) != 1 {
		t.Fatalf("marks = %d, want 1", len(f.marks))
	}
	m := f.marks[0]
	if m.id != "e_9" || m.status != "bounced" || m.reason == nil || *m.reason != "mailbox does not exist" {
		t.Fatalf("mark = %+v reason=%v, want bounced/mailbox does not exist", m, m.reason)
	}
	if len(f.suppress) != 1 {
		t.Fatalf("suppress = %d, want 1", len(f.suppress))
	}
	if s := f.suppress[0]; s.email != "hard@x.com" || s.reason != "bounced" {
		t.Fatalf("suppress = %+v, want {hard@x.com bounced}", s)
	}
}

func TestWebhook_BouncedTransientNoSuppress(t *testing.T) {
	f := &fakeStore{}
	h := frozenWebhook(f, testSecret)
	body := `{"type":"email.bounced","data":{"email_id":"e_4","to":"soft@x.com","bounce":{"type":"Transient","subType":"MailboxFull","message":"mailbox full"}}}`
	w, err := serve(h, signedRequest(t, testSecret, testNow, body))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(f.marks) != 1 {
		t.Fatalf("marks = %d, want 1", len(f.marks))
	}
	if m := f.marks[0]; m.status != "bounced" || m.reason == nil || *m.reason != "mailbox full" {
		t.Fatalf("mark = %+v reason=%v", m, m.reason)
	}
	if len(f.suppress) != 0 {
		t.Fatalf("transient bounce must not suppress, got %+v", f.suppress)
	}
}

func TestWebhook_Complained(t *testing.T) {
	f := &fakeStore{}
	h := frozenWebhook(f, testSecret)
	body := `{"type":"email.complained","data":{"email_id":"e_7","to":["spam@x.com"]}}`
	w, err := serve(h, signedRequest(t, testSecret, testNow, body))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(f.marks) != 1 {
		t.Fatalf("marks = %d, want 1", len(f.marks))
	}
	if m := f.marks[0]; m.id != "e_7" || m.status != "complained" || m.reason == nil || *m.reason != "Recipient marked as spam" {
		t.Fatalf("mark = %+v reason=%v", m, m.reason)
	}
	if len(f.suppress) != 1 {
		t.Fatalf("suppress = %d, want 1", len(f.suppress))
	}
	if s := f.suppress[0]; s.email != "spam@x.com" || s.reason != "complained" {
		t.Fatalf("suppress = %+v, want {spam@x.com complained}", s)
	}
}

func TestWebhook_IgnoredEventType(t *testing.T) {
	f := &fakeStore{}
	h := frozenWebhook(f, testSecret)
	body := `{"type":"email.delivery_delayed","data":{"email_id":"e_x","to":["q@x.com"]}}`
	w, err := serve(h, signedRequest(t, testSecret, testNow, body))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(f.marks) != 0 || len(f.suppress) != 0 {
		t.Fatalf("no-op event mutated state: marks=%+v suppress=%+v", f.marks, f.suppress)
	}
}

func TestWebhook_RejectsStaleTimestamp(t *testing.T) {
	st := &fakeStore{}
	now := time.Unix(1_700_000_000, 0)
	h := NewWebhookHandler(st, testSecret,
		WithTolerance(5*time.Minute),
		WithClock(func() time.Time { return now }))

	// Signed six minutes ago — a validly-signed request that must still fail.
	stale := now.Add(-6 * time.Minute)
	req := signedRequest(t, testSecret, stale, `{"type":"email.bounced"}`)

	// ErrStale specifically, not merely "some error": a rejection for the wrong
	// reason (a broken signature check, say) would satisfy err != nil while
	// proving nothing about the replay window this test exists to assert.
	err := h.Handle(httptest.NewRecorder(), req)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want %v; a six-minute-old signed request must be rejected "+
			"as stale — accepting it lets a captured bounce be replayed to re-suppress an address", err, ErrStale)
	}
	assertStoreUntouched(t, st)
}

// assertStoreUntouched proves a rejection happened BEFORE any state was
// written. A handler that suppressed the address and then returned ErrStale
// would pass an error-only assertion while having already done the damage.
func assertStoreUntouched(t *testing.T, f *fakeStore) {
	t.Helper()
	if len(f.marks) != 0 || len(f.suppress) != 0 {
		t.Fatalf("rejected request touched the store: marks=%+v suppress=%+v", f.marks, f.suppress)
	}
}

func TestWebhook_RejectsFutureTimestamp(t *testing.T) {
	st := &fakeStore{}
	now := time.Unix(1_700_000_000, 0)
	h := NewWebhookHandler(st, testSecret,
		WithTolerance(5*time.Minute),
		WithClock(func() time.Time { return now }))

	future := now.Add(6 * time.Minute)
	req := signedRequest(t, testSecret, future, `{"type":"email.bounced"}`)

	err := h.Handle(httptest.NewRecorder(), req)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want %v; a far-future timestamp must be rejected as stale — "+
			"otherwise an attacker mints a request that stays valid indefinitely", err, ErrStale)
	}
	assertStoreUntouched(t, st)
}

func TestWebhook_AcceptsFreshTimestamp(t *testing.T) {
	st := &fakeStore{}
	now := time.Unix(1_700_000_000, 0)
	h := NewWebhookHandler(st, testSecret,
		WithTolerance(5*time.Minute),
		WithClock(func() time.Time { return now }))

	req := signedRequest(t, testSecret, now.Add(-30*time.Second),
		`{"type":"email.delivered","data":{"email_id":"p1"}}`)

	if err := h.Handle(httptest.NewRecorder(), req); err != nil {
		t.Fatalf("a fresh request must be accepted, got %v", err)
	}
}

// errStoreDown is the cause a failing store returns. The tests below assert it
// survives the ErrStoreFailure wrap: a consumer that can match the sentinel but
// cannot log the cause has no way to diagnose why Resend keeps retrying.
var errStoreDown = errors.New("store unavailable")

// A hard bounce whose Suppress fails is the worst case in the package: with the
// error swallowed, IsSuppressed keeps answering false and that dead address
// keeps being mailed, costing the SHARED sending domain its reputation. The
// error must reach the consumer so it can answer 5xx and let Resend redeliver.
func TestWebhook_SuppressFailureIsRetryable(t *testing.T) {
	f := &fakeStore{suppressErr: errStoreDown}
	h := frozenWebhook(f, testSecret)
	body := `{"type":"email.bounced","data":{"email_id":"e_9","to":"hard@x.com","bounce":{"type":"Permanent","subType":"General"}}}`

	w, err := serve(h, signedRequest(t, testSecret, testNow, body))
	if !errors.Is(err, ErrStoreFailure) {
		t.Fatalf("err = %v, want %v", err, ErrStoreFailure)
	}
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("err = %v, want the underlying cause %v to remain loggable", err, errStoreDown)
	}
	// A success body is the lie that stops the retry: it tells Resend the event
	// was durably accepted when nothing was written.
	if w.Body.Len() != 0 {
		t.Fatalf("failed store write wrote the success body: %q", w.Body.String())
	}
}

func TestWebhook_MarkFailureIsRetryable(t *testing.T) {
	f := &fakeStore{markErr: errStoreDown}
	h := frozenWebhook(f, testSecret)
	body := `{"type":"email.delivered","data":{"email_id":"e_123","to":["a@x.com"]}}`

	w, err := serve(h, signedRequest(t, testSecret, testNow, body))
	if !errors.Is(err, ErrStoreFailure) {
		t.Fatalf("err = %v, want %v", err, ErrStoreFailure)
	}
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("err = %v, want the underlying cause %v to remain loggable", err, errStoreDown)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("failed store write wrote the success body: %q", w.Body.String())
	}
}

// A bounce triggers TWO store calls. When the log-row write fails, the
// suppression must still be ATTEMPTED rather than skipped: it is the
// safety-critical one, and deferring it to a redelivery that may never arrive
// leaves the dead address mailable in the meantime. Both operations are
// idempotent, so the retry provoked by the returned error converges.
func TestWebhook_MarkFailureStillAttemptsSuppress(t *testing.T) {
	f := &fakeStore{markErr: errStoreDown}
	h := frozenWebhook(f, testSecret)
	body := `{"type":"email.bounced","data":{"email_id":"e_9","to":"hard@x.com","bounce":{"type":"Permanent","subType":"General"}}}`

	_, err := serve(h, signedRequest(t, testSecret, testNow, body))
	if !errors.Is(err, ErrStoreFailure) {
		t.Fatalf("err = %v, want %v", err, ErrStoreFailure)
	}
	if len(f.suppress) != 1 {
		t.Fatalf("suppress = %+v, want one attempt even though the mark failed", f.suppress)
	}
	if s := f.suppress[0]; s.email != "hard@x.com" || s.reason != ReasonBounced {
		t.Fatalf("suppress = %+v, want {hard@x.com bounced}", s)
	}
}

// Every other webhook test injects WithClock, so none of them exercises
// NewWebhookHandler(store, secret) — the constructor every consumer actually
// uses in production. That left both defaults unasserted: a reviewer changed
// `tolerance: DefaultTolerance` to `tolerance: 0` and all ten tests stayed
// green, and dropping `now: time.Now` would ship green too and then nil-panic
// on the first production request.
//
// The request is signed 30s in the past — comfortably inside DefaultTolerance
// but unambiguously outside a zero tolerance. Signing at exactly time.Now()
// would not discriminate, because signedRequest truncates to whole seconds and
// the resulting sub-second skew can round to zero.
func TestWebhook_ZeroOptionConstructorUsesRealDefaults(t *testing.T) {
	f := &fakeStore{}
	h := NewWebhookHandler(f, testSecret) // no options: the production path

	body := `{"type":"email.delivered","data":{"email_id":"e_default","to":["a@x.com"]}}`
	w, err := serve(h, signedRequest(t, testSecret, time.Now().Add(-30*time.Second), body))
	if err != nil {
		t.Fatalf("the default constructor must accept a request signed 30s ago, got %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != `{"received":true}` {
		t.Fatalf("body = %q, want %q", got, `{"received":true}`)
	}
	if len(f.marks) != 1 || f.marks[0].id != "e_default" || f.marks[0].status != StatusDelivered {
		t.Fatalf("marks = %+v, want one delivered mark for e_default", f.marks)
	}
}
