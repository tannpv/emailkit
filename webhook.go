package emailkit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultTolerance is the Svix-recommended replay window.
const DefaultTolerance = 5 * time.Minute

var (
	ErrBadSignature = errors.New("emailkit: invalid webhook signature")
	ErrStale        = errors.New("emailkit: webhook timestamp outside tolerance")
	ErrBadPayload   = errors.New("emailkit: malformed webhook payload")
)

// WebhookOption tunes a WebhookHandler at construction. The two knobs exist so
// the replay window is asserted by a test that moves the clock rather than one
// that sleeps.
type WebhookOption func(*WebhookHandler)

// WithTolerance overrides DefaultTolerance.
func WithTolerance(d time.Duration) WebhookOption {
	return func(h *WebhookHandler) { h.tolerance = d }
}

// WithClock overrides time.Now, so tests can pin "now".
func WithClock(now func() time.Time) WebhookOption {
	return func(h *WebhookHandler) { h.now = now }
}

// WebhookHandler receives Resend delivery events and reflects them onto the
// project's log and suppression list.
type WebhookHandler struct {
	store     Store
	secret    string
	tolerance time.Duration
	now       func() time.Time
}

// NewWebhookHandler wires the Store port and the Resend webhook secret.
func NewWebhookHandler(st Store, secret string, opts ...WebhookOption) *WebhookHandler {
	h := &WebhookHandler{store: st, secret: secret, tolerance: DefaultTolerance, now: time.Now}
	for _, o := range opts {
		o(h)
	}
	return h
}

// webhookEvent mirrors the Resend payload shape the handler reads.
type webhookEvent struct {
	Type string `json:"type"`
	Data struct {
		EmailID string          `json:"email_id"`
		To      json.RawMessage `json:"to"` // string OR []string
		Reason  string          `json:"reason"`
		Bounce  struct {
			Type    string `json:"type"`
			SubType string `json:"subType"`
			Message string `json:"message"`
		} `json:"bounce"`
	} `json:"data"`
}

// Handle processes one webhook POST. Mount it WITHOUT body-consuming
// middleware (the raw body is needed for signature verification) and WITHOUT
// auth (Resend cannot authenticate). Returns an error for the caller to map
// onto its own error response shape; writes only the success body itself.
func (h *WebhookHandler) Handle(w http.ResponseWriter, r *http.Request) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		// Fail closed: a body we cannot read is a body we cannot verify.
		return ErrBadSignature
	}
	if h.secret == "" {
		return ErrBadSignature
	}
	if err := h.verify(r.Header, raw); err != nil {
		return err
	}

	var event webhookEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return ErrBadPayload
	}

	ctx := r.Context()
	id := event.Data.EmailID
	to := firstRecipient(event.Data.To)

	switch event.Type {
	case EventDelivered:
		if id != "" {
			_ = h.store.MarkByProviderID(ctx, id, StatusDelivered, nil)
		}
	case EventBounced:
		reason := event.Data.Bounce.Message
		if reason == "" {
			reason = event.Data.Reason
		}
		if reason == "" {
			reason = StatusBounced
		}
		if id != "" {
			reasonPtr := reason
			_ = h.store.MarkByProviderID(ctx, id, StatusBounced, &reasonPtr)
		}
		if to != "" && isPermanentBounce(event.Data.Bounce.Type, event.Data.Bounce.SubType) {
			_ = h.store.Suppress(ctx, to, ReasonBounced)
		}
	case EventComplained:
		if id != "" {
			reasonPtr := "Recipient marked as spam"
			_ = h.store.MarkByProviderID(ctx, id, StatusComplained, &reasonPtr)
		}
		if to != "" {
			_ = h.store.Suppress(ctx, to, ReasonComplained)
		}
	default:
		// sent / opened / clicked / delivery_delayed carry no state we keep
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"received":true}`))
	return nil
}

// verify checks the Svix signature AND the timestamp. The ported version
// signed over svix-timestamp but never validated it, so a captured request
// stayed valid forever — replaying a bounce re-suppressed an address on
// demand, and a suppressed user stops receiving password resets.
//
// The secret is handled as the ported version handled it: an optional whsec_
// prefix is stripped and the remainder base64-decoded as the HMAC key; a
// secret that is not a valid key simply fails the comparison.
func (h *WebhookHandler) verify(hdr http.Header, body []byte) error {
	id := hdr.Get("svix-id")
	ts := hdr.Get("svix-timestamp")
	sigHeader := hdr.Get("svix-signature")
	if id == "" || ts == "" || sigHeader == "" {
		return ErrBadSignature
	}

	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	// Checked in both directions: a far-future timestamp would otherwise mint a
	// request that stays valid until that time arrives.
	if d := h.now().Sub(time.Unix(secs, 0)); d > h.tolerance || d < -h.tolerance {
		return ErrStale
	}

	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h.secret, "whsec_"))
	if err != nil {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "." + string(body)))
	expected := []byte(base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	// The header is a space-separated list of `v1,<base64sig>` entries.
	for _, part := range strings.Split(sigHeader, " ") {
		idx := strings.IndexByte(part, ',')
		if idx < 0 {
			continue
		}
		sig := []byte(part[idx+1:])
		// Constant-time: length is compared first only because hmac.Equal on
		// unequal lengths returns early anyway; the content compare is never
		// short-circuited.
		if len(sig) == len(expected) && hmac.Equal(sig, expected) {
			return nil
		}
	}
	return ErrBadSignature
}

// firstRecipient handles Resend sending `to` as either a string or an array.
func firstRecipient(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		if len(arr) > 0 {
			return arr[0]
		}
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}
