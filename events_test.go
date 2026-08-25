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
	}
	for _, c := range cases {
		if got := isPermanentBounce(c.bType, c.subType); got != c.want {
			t.Errorf("%s: isPermanentBounce(%q,%q) = %v, want %v",
				c.name, c.bType, c.subType, got, c.want)
		}
	}
}
