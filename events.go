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
var permanentBounceMarkers = []string{"permanent", "hard"}

// isPermanentBounce reports whether a bounce should suppress the address.
// Only permanent bounces suppress: a transient bounce (full mailbox,
// greylisting) must not lock a real user out of password resets.
func isPermanentBounce(bounceType, subType string) bool {
	kind := strings.ToLower(bounceType + " " + subType)
	for _, m := range permanentBounceMarkers {
		if strings.Contains(kind, m) {
			return true
		}
	}
	return false
}
