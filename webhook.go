package emailkit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultTolerance is the Svix-recommended replay window.
const DefaultTolerance = 5 * time.Minute

// Sentinels returned by Handle. The first three are NOT retryable (the sender
// sent something this handler will never accept); ErrStoreFailure is the only
// retryable one. See the error-mapping duty documented on Handle.
var (
	ErrBadSignature = errors.New("emailkit: invalid webhook signature")
	ErrStale        = errors.New("emailkit: webhook timestamp outside tolerance")
	ErrBadPayload   = errors.New("emailkit: malformed webhook payload")

	// ErrStoreFailure means the event was understood and authentic but could
	// not be recorded. It wraps the underlying store error so a consumer can
	// log the cause while still matching the sentinel.
	ErrStoreFailure = errors.New("emailkit: webhook store write failed")
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
// onto its own error response shape; writes only the success body, and only
// when it returns nil.
//
// ERROR-MAPPING DUTY — this is the consumer's, not this package's:
//
//   - ErrBadSignature, ErrStale and ErrBadPayload are NOT retryable: resending
//     the identical request cannot succeed. Map ALL THREE to one opaque 4xx,
//     and never echo the error text (or any distinguishing detail) to the
//     caller. They are kept distinguishable so an operator reading server-side
//     logs can tell a rotated secret from clock skew — the timestamp check runs
//     before the HMAC, so ErrStale reveals nothing about signature validity.
//     Surfacing the difference to the caller hands that same discrimination to
//     whoever is probing the endpoint, which is the one place it has value to
//     an attacker.
//   - ErrStoreFailure IS retryable and is the one case that should be a 5xx, so
//     the sender redelivers. It wraps the underlying store error, so logging it
//     verbatim server-side carries the cause; do not echo it to the caller
//     either. (Per the Store PII contract, that cause must not embed the
//     recipient address.)
//
// The 5xx matters because a 200 tells Resend the event was durably accepted, so
// it never retries — discarding the only at-least-once mechanism available. A
// hard bounce whose Suppress failed then leaves IsSuppressed answering false,
// and that dead address keeps being mailed indefinitely, which degrades
// deliverability for every user on the shared sending domain.
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
	// Normalised HERE, once, rather than at each Suppress call below: the two
	// call sites are one policy, and the bug this replaces was exactly one of
	// them being written without it. Normalising at the single point where the
	// address enters the handler means a third event type added later cannot
	// forget. It must be the same normalizeAddress deliver() reads through —
	// the suppression list is only worth anything when the write key and the
	// read key are produced by one definition.
	//
	// Also tightens the `to != ""` guards below: a whitespace-only recipient
	// trims to empty and is correctly skipped instead of suppressing " ".
	to := normalizeAddress(firstRecipient(event.Data.To))

	// An event can trigger TWO store calls (a bounce writes the log row AND may
	// suppress). Both are attempted even when the first fails, and every result
	// is collected — see storeFailure for why that converges.
	var errs []error

	switch event.Type {
	case EventDelivered:
		if id != "" {
			errs = append(errs, h.store.MarkByProviderID(ctx, id, StatusDelivered, nil))
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
			errs = append(errs, h.store.MarkByProviderID(ctx, id, StatusBounced, &reasonPtr))
		}
		if to != "" && isPermanentBounce(event.Data.Bounce.Type, event.Data.Bounce.SubType) {
			errs = append(errs, h.store.Suppress(ctx, to, ReasonBounced))
		}
	case EventComplained:
		if id != "" {
			reasonPtr := "Recipient marked as spam"
			errs = append(errs, h.store.MarkByProviderID(ctx, id, StatusComplained, &reasonPtr))
		}
		if to != "" {
			errs = append(errs, h.store.Suppress(ctx, to, ReasonComplained))
		}
	default:
		// sent / opened / clicked / delivery_delayed carry no state we keep
	}

	// Return BEFORE the success body: a 200 with `{"received":true}` on a failed
	// write is precisely the lie that stops Resend retrying.
	if err := storeFailure(errs...); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"received":true}`))
	return nil
}

// storeFailure folds the results of the store calls one event triggered into a
// single retryable error, or nil when every call succeeded (errors.Join drops
// nils, so the all-nil case returns nil).
//
// PROPAGATING IS SAFE BECAUSE BOTH STORE OPERATIONS ARE IDEMPOTENT:
// MarkByProviderID sets a status on the row identified by the provider id, and
// Suppress adds an address to a set. Re-running either with the same event
// yields the same state, so the redelivery the 5xx provokes cannot double-count
// anything. Without that property, asking for a retry would trade a lost write
// for a duplicated one.
//
// That idempotency is also why BOTH calls are attempted before this is
// consulted, rather than aborting at the first failure. Aborting would leave a
// hard bounce's Suppress unattempted whenever the log-row write happened to
// fail first — deferring the safety-critical write to a retry that may never
// arrive — and would need one retry round per failing call. Attempting both
// converges in a single retry: whichever call already succeeded replays
// harmlessly, and whichever failed gets its second chance in the same round.
func storeFailure(errs ...error) error {
	joined := errors.Join(errs...)
	if joined == nil {
		return nil
	}
	// Two %w verbs: callers match ErrStoreFailure with errors.Is and still
	// reach the underlying cause for their logs.
	return fmt.Errorf("%w: %w", ErrStoreFailure, joined)
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
