package emailkit

import "testing"

func TestRender_UnknownKeyEmpty(t *testing.T) {
	r := Registry{}
	subj, html := r.Render("nope", nil)
	if subj != "" || html != "" {
		t.Fatalf("want empty for unknown key, got %q / %q", subj, html)
	}
}

func TestRender_UnknownTokenEmpty(t *testing.T) {
	r := Registry{"k": {Subject: "hi {{missing}}", Body: "x"}}
	subj, _ := r.Render("k", map[string]string{})
	if subj != "hi " {
		t.Fatalf("unknown token must render empty, got %q", subj)
	}
}

func TestRender_HTMLEscapesBodyNotSubject(t *testing.T) {
	r := Registry{"k": {Subject: "{{v}}", Body: "<p>{{v}}</p>"}}
	subj, html := r.Render("k", map[string]string{"v": "<b>&x"})
	if subj != "<b>&x" {
		t.Fatalf("subject must NOT be escaped, got %q", subj)
	}
	if html != "<p>&lt;b&gt;&amp;x</p>" {
		t.Fatalf("body must be escaped, got %q", html)
	}
}

func TestRender_SubjectEscapeFalseRaw(t *testing.T) {
	if got := substitute("{{v}}", map[string]string{"v": "a&b"}, false); got != "a&b" {
		t.Fatalf("escape=false must pass through, got %q", got)
	}
}
