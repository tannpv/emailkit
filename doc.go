// Package emailkit is the one transactional-email transport shared by
// draftright, liseuse and bacnam.
//
// THE RULE: deliver() is the only code path that reaches a Sender, and it is
// unexported. Send and SendRaw are the only exported ways in, and both funnel
// through it. That is what makes the suppression check unbypassable rather than
// merely customary — see chokepoint_test.go, which fails the build if a second
// caller appears.
//
// What this package deliberately does NOT own:
//
//   - Storage. Each project implements Store against its own schema, which is
//     how bacnam keeps tenant_id out of here entirely.
//   - Product vocabulary. "Renewal reminder" belongs to draftright, not to a
//     module a language-learning app imports.
//   - Template content. Callers supply a Registry; this package only substitutes
//     and escapes.
package emailkit
