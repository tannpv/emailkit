# emailkit — working agreement

One transactional-email transport for **draftright, liseuse and bacnam**. Public,
zero non-stdlib dependencies, Go directive `1.25.0` (draftright's floor — raising
it would lock out the first consumer).

## RULE #1 — Clean, Reusable, Extendable

Canonical statement lives in `~/.claude/CLAUDE.md`. Short version, because this
repo is cloned by people who do not have that file:

Every value that carries meaning has **one** source of truth. Duplicated logic
counts as hardcoding — two copies drift. Cross-cutting concerns get a chokepoint
plus a machine that proves nothing bypassed it.

This module exists *because* of that rule: draftright, liseuse and bacnam would
otherwise hold three copies of one sending policy, and this codebase has already
paid for that pattern once — a copy-pasted `app_releases` upsert drifted, `sha256`
landed in one copy only, and Windows shipped installers with no integrity
verification for two months (DraftRight #22).

## The guarantee, and why it is structural rather than documented

`deliver()` is unexported and is the **only** caller of `Sender.Send`. `Send` and
`SendRaw` are the only exported ways in, and both funnel through it. Skipping the
suppression check is therefore not a discipline problem — it is unrepresentable.

`chokepoint_test.go` enforces it and fails the build in **both** directions:

- a second caller appears → fails, naming the offender
- it matches **zero** call sites → fails, because a renamed field would otherwise
  leave the guard silently dead while still passing

The guard is syntactic. It documents the evasions it cannot catch (local copy,
method value, method expression, a package-level `var` holding a func literal).
It is a tripwire for the regression that actually happens — someone later adding
a second send path because it seemed convenient — not a proof.

**Do not add an exported function that returns a live provider client.** The
spec once claimed returning the `Sender` interface rather than the concrete
struct was enough; it is not. Returning an interface stops a consumer *naming* a
type, not *calling* the exported method on the value handed to them.
`NewService` builds the client internally; `NewServiceWithSender` is the seam for
a consumer's own provider, and its doc states plainly that the caller keeps that
reference.

## What this module refuses to own

| not ours | why |
|---|---|
| **Storage** | each project implements `Store` against its own schema. This is how bacnam's `tenant_id` stays out entirely — its implementation closes over the tenant and emailkit never learns tenancy exists |
| **Product vocabulary** | `SendRenewalReminder` belongs to draftright. A shared module carrying one consumer's subscription wording is a union, not a shared module |
| **Template content** | callers supply a `Registry`. We substitute and escape; we do not decide what an email says |

One caveat on the tenancy claim: it holds for `Service` (one per tenant) but not
for the webhook. A `WebhookHandler` is one per *endpoint* and a Resend event
carries no tenant, so bacnam must resolve it from the provider id inside
`MarkByProviderID`.

## Contracts a `Store` implementer must honour

These are stated on the interface because emailkit cannot enforce them:

- **Idempotency.** `Suppress` and `MarkByProviderID` must be idempotent. `Handle`
  answers a failed store write with a retryable error so the provider redelivers,
  and that redelivery replays whichever call already succeeded. A `Suppress`
  written as a bare `INSERT` turns one transient failure into a permanent
  duplicate-key loop in which the address is never suppressed.
- **Address normalisation.** Addresses emailkit passes are already lowercased and
  trimmed. Do not re-case them; compare exactly. **But if your own code writes
  suppression rows** — an admin unsubscribe, a complaint import — apply the same
  lowercase-and-trim first, or the row is never matched by the lookup and the
  suppression silently has no effect.
- **No PII in errors.** emailkit logs returned errors verbatim. An error wrapped
  as `fmt.Errorf("lookup %s: %w", email, err)` puts the address into the log line
  `domainOf` exists to keep clean.
- **Concurrency.** `Send` and `SendRaw` are fire-and-forget; N in-flight sends
  call your `Store` from N goroutines with no serialisation from here.

## Before changing anything

- **Prove a test can fail before trusting it.** Break the behaviour, watch the
  specific test fail, restore. This repo has twice nearly shipped tests that
  could not fail: a `String()`-panics fixture that `fmt` already handled, and a
  chokepoint guard that passed vacuously when its target was renamed.
- `gofmt -l .`, `go vet ./...`, `go test ./... -race -count=3` — all clean.
- CI compiles the README's Go examples. That file has already shipped a
  `log.Error` call that does not exist and an example using a constructor that
  was removed for letting consumers bypass the chokepoint. Three projects copy
  from it.
- CI fails if `go.mod` gains a `require` block or a `go.sum` appears. Zero
  dependencies is a constraint with a machine behind it, not an aspiration.
