package emailkit

import (
	"regexp"
	"strings"
)

// TemplateDef is one built-in template. Subject is substituted raw; Body is
// substituted with HTML escaping.
type TemplateDef struct {
	Subject string
	Body    string
}

// Registry is the caller's built-in template set. This package never ships
// templates of its own — content is product vocabulary and belongs to the
// project that sends it.
type Registry map[string]TemplateDef

// Render returns empty strings for an unknown key. Callers treat that as
// "nothing to send" rather than an error, matching the ported behaviour.
func (r Registry) Render(key string, vars map[string]string) (subject, html string) {
	def, ok := r[key]
	if !ok {
		return "", ""
	}
	return substitute(def.Subject, vars, false), substitute(def.Body, vars, true)
}

// tokenRe deliberately has no capture group: the name is extracted below by
// slicing the full match (m[2:len(m)-2]), so a capturing group here would
// just be unused — write the pattern to match that, not to imply a group
// something reads.
var tokenRe = regexp.MustCompile(`\{\{\w+\}\}`)

// substitute replaces {{token}} from vars. An unknown token renders empty
// rather than leaving the literal in place, so a missing variable never leaks
// template syntax into a user's inbox.
//
// escape is false for subjects and true for bodies: a subject is not HTML and
// escaping it would show users "&amp;" in their inbox list.
func substitute(s string, vars map[string]string, escape bool) string {
	return tokenRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-2]
		v := vars[name]
		if escape {
			return escapeHTML(v)
		}
		return v
	})
}

func escapeHTML(s string) string { return htmlEscaper.Replace(s) }

var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
)
