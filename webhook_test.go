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

	if err := h.Handle(httptest.NewRecorder(), req); err == nil {
		t.Fatal("a six-minute-old signed request must be rejected; " +
			"accepting it lets a captured bounce be replayed to re-suppress an address")
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

	if err := h.Handle(httptest.NewRecorder(), req); err == nil {
		t.Fatal("a far-future timestamp must be rejected — otherwise an attacker " +
			"mints a request that stays valid indefinitely")
	}
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
