package emailkit

import "testing"

func TestIsPermanentBounce(t *testing.T) {
	cases := []struct {
		name, bType, subType string
		want                 bool
	}{
		{"permanent general", "Permanent", "General", true},
		{"permanent nomailbox", "Permanent", "NoEmail", true},
		{"hard synonym", "hard", "", true},
		{"lowercase permanent", "permanent", "suppressed", true},
		{"transient mailbox full", "Transient", "MailboxFull", false},
		{"transient greylist", "Transient", "General", false},
		{"undetermined", "Undetermined", "", false},
		{"empty", "", "", false},
		// Regression guards: bounceType alone must decide permanence. Under
		// the old "bounceType + subType" concatenation, these subtypes would
		// have flipped the result to true and wrongly suppressed a live user.
		{"transient subtype says permanently unavailable", "Transient", "PermanentlyUnavailable", false},
		{"transient subtype says hard", "Transient", "HardFull", false},
		{"permanent with transient-sounding subtype", "Permanent", "MailboxFull", true},
	}
	for _, c := range cases {
		if got := isPermanentBounce(c.bType, c.subType); got != c.want {
			t.Errorf("%s: isPermanentBounce(%q,%q) = %v, want %v",
				c.name, c.bType, c.subType, got, c.want)
		}
	}
}
