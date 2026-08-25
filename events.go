package emailkit

import "strings"

// Resend webhook event types. External-spec constants — the vocabulary belongs
// to Resend (https://resend.com/docs/dashboard/webhooks/event-types), so the
// literals are theirs; naming them once stops four repos retyping them.
const (
	EventDelivered  = "email.delivered"
	EventBounced    = "email.bounced"
	EventComplained = "email.complained"
)

// Statuses written to the audit row.
const (
	StatusSent       = "sent"
	StatusFailed     = "failed"
	StatusSuppressed = "suppressed"
	StatusSkipped    = "skipped"
	StatusDelivered  = "delivered"
	StatusBounced    = "bounced"
	StatusComplained = "complained"
)

// Reasons an address enters the suppression list.
const (
	ReasonBounced    = "bounced"
	ReasonComplained = "complained"
)

// permanentBounceMarkers are the tokens that mean "this address will never
// accept mail". Kept as data so a new category from Resend is one row here,
// not another strings.Contains at a call site.
//
// "permanent" is SES/Resend vocabulary (see the doc comment on
// isPermanentBounce). "hard" is NOT part of that vocabulary — it is
// SendGrid-style terminology inherited from draftright's earlier bounce
// handling, and the Resend event-types URL above does not cover it. It is
// kept here only as a defensive synonym in case some provider or a future
// Resend revision emits that wording; it is not expected to ever match SES
// bounceType values ("Permanent", "Transient", "Undetermined").
var permanentBounceMarkers = []string{"permanent", "hard"}

// isPermanentBounce reports whether a bounce should suppress the address.
// Only permanent bounces suppress: a transient bounce (full mailbox,
// greylisting) must not lock a real user out of password resets.
//
// Amazon SES (which Resend runs on) carries bounce permanence entirely in
// bounceType: the values are "Permanent", "Transient" and "Undetermined".
// subType never carries permanence — Permanent has General/NoEmail/
// Suppressed/OnAccountSuppressionList, Transient has General/MailboxFull/
// MessageTooLarge/ContentRejected/AttachmentRejected. So only bounceType is
// matched here; subType is accepted (Task 6 already calls this with both
// arguments) but deliberately not consulted.
//
// This is NOT a behaviour change: for every bounce shape Resend emits today,
// matching bounceType alone gives the same result as matching the old
// "bounceType + subType" concatenation did. It removes a future failure
// mode instead — concatenating the two meant a later subtype that happened
// to contain "permanent" or "hard" under a Transient bounceType would flip
// the result to true and wrongly suppress a live user. Do not "restore" the
// concatenation; it is the bug, not a regression from removing it.
func isPermanentBounce(bounceType, subType string) bool {
	kind := strings.ToLower(bounceType)
	for _, m := range permanentBounceMarkers {
		if strings.Contains(kind, m) {
			return true
		}
	}
	return false
}
