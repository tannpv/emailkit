# emailkit

One transactional email transport for draftright, liseuse and bacnam.

## Why this exists

Three projects needed the same Resend integration. Copying draftright's
`internal/email` into each would have made three definitions of one policy —
the pattern that already shipped DraftRight #22, where a copy-pasted
`app_releases` upsert drifted and Windows went two months without installer
integrity checks.

## What it does not own

Storage, tenancy and template content stay in the consuming project. This
module holds no secrets, no business logic and no product copy — that is why
the repository is public. See `doc.go` for the full boundary.

## Usage

Templates are yours — this module ships none, because content is product
vocabulary. A `Registry` maps a key to a `TemplateDef`; `{{token}}` placeholders
are filled from the `vars` map passed to `Send`:

```go
templates := emailkit.Registry{
    "password-reset": emailkit.TemplateDef{
        Subject: "Reset your password",
        Body:    `<p>Your code is {{code}}. It expires in {{minutes}} minutes.</p>`,
    },
}

svc := emailkit.NewService(
    myStore,                                  // your Store implementation
    emailkit.Config{APIKey: key, From: from}, // your credentials
    templates,                                // your Registry
)
svc.Send(ctx, "password-reset", user.Email, map[string]string{
    "code":    code,
    "minutes": "15",
})
```

`Subject` is substituted raw and `Body` is HTML-escaped, so a user-supplied
variable cannot inject markup into the body while subjects stay free of
`&amp;`. An unknown token renders as empty rather than leaving `{{code}}` in
someone's inbox, and an unknown *key* renders empty for both fields — which
`deliver()` records as skipped rather than mailing a blank email.

`Store.Template` is consulted first, so a project that keeps editable templates
in its database overrides the Registry entry for that key without changing this
call.

`Send` is fire-and-forget: it returns immediately and the whole send — template
resolution included — runs on its own goroutine, so email can never block or
fail the request that triggered it. The `vars` map is copied before that
goroutine starts, so it is safe to mutate or reuse as soon as `Send` returns.

Two further entry points:

- `svc.SendRaw(ctx, to, subject, html, label)` delivers an already-rendered
  subject and body through the identical suppression → credentials → send →
  audit path, for callers with no template. `label` is what the audit row
  records as `Type`, where `Send` would record the template key.
- `svc.Wait()` blocks until in-flight sends finish. Production never calls it;
  it is exported because consumers' tests live in other packages and need a
  deterministic point at which a fire-and-forget send has landed.

`NewService` builds the Resend client internally, so no exported function ever
hands a consumer a ready-to-use provider client — the suppression check in
`deliver()` cannot be routed around. To supply your own provider (SES,
Postmark, or a test double), use the seam instead:

```go
svc := emailkit.NewServiceWithSender(myStore, cfg, templates, mySender)
```

`mySender` must satisfy the `Sender` interface (`Send(ctx, apiKey, from, to,
subject, html) (providerID string, err error)`). Whichever constructor you
use, every send still passes through the suppression check and writes an
audit row, because `deliver()` remains the only caller of `Sender.Send`.

### Store

Each project implements `Store` against its own schema:

```go
type Store interface {
    IsSuppressed(ctx context.Context, email string) (bool, error)
    LogSend(ctx context.Context, r SendRecord) error
    Template(ctx context.Context, key string) (subject, html string, ok bool)
    MarkByProviderID(ctx context.Context, providerID, status string, reason *string) error
    Suppress(ctx context.Context, email, reason string) error
}
```

`MarkByProviderID` and `Suppress` **must be idempotent**. `Handle` (see below)
retries the store write on failure, and that retry replays whichever of the
two webhook-path calls already succeeded. A `Suppress` written as a bare
`INSERT` turns that retry into a permanent duplicate-key failure loop instead
of a successful redelivery — use an upsert or `ON CONFLICT DO NOTHING`.

Addresses reaching `Suppress` and `IsSuppressed` are **already normalised** by
emailkit (lowercased and whitespace-trimmed, by one internal helper both the
send path and the webhook path go through). Compare them exactly — a plain
`WHERE email = $1` is correct, and no `LOWER()` index or `citext` column is
needed. Do not add a second normalisation of your own: that would be a second
definition of one policy, and the two would drift. The address handed to
`LogSend` is deliberately *not* normalised — that row is the audit trail of what
was actually mailed.

### Webhook

Mount the webhook without body-consuming middleware and without auth (Resend
cannot authenticate):

```go
h := emailkit.NewWebhookHandler(myStore, webhookSecret)
mux.HandleFunc("POST /webhooks/resend", func(w http.ResponseWriter, r *http.Request) {
    if err := h.Handle(w, r); err != nil {
        switch {
        case errors.Is(err, emailkit.ErrStoreFailure):
            // Retryable: the event was authentic but the store write failed.
            // Answer 5xx so Resend redelivers, and log err (not the response)
            // for the cause.
            slog.Error("webhook store write failed", "err", err)
            w.WriteHeader(http.StatusInternalServerError)
        default:
            // ErrBadSignature, ErrStale, ErrBadPayload: never retryable.
            // Collapse all three into one opaque 4xx and never echo err to
            // the caller — the distinction has value only to whoever is
            // probing the endpoint.
            w.WriteHeader(http.StatusBadRequest)
        }
        return
    }
})
```

Getting this mapping wrong either breaks Resend's retries (any error answered
with 2xx) or silently drops delivery events (a store failure answered as if it
were a bad request, so Resend never redelivers).

`Handle` verifies the Svix signature **and** the timestamp, rejecting anything
outside `emailkit.DefaultTolerance` (5 minutes) in either direction — without
that window a captured request replays forever, and replaying a bounce
re-suppresses an address on demand. Two options tune it at construction:

```go
h := emailkit.NewWebhookHandler(myStore, webhookSecret,
    emailkit.WithTolerance(30*time.Second), // override DefaultTolerance
    emailkit.WithClock(func() time.Time { return fixed }), // pin "now" in tests
)
```

`WithClock` exists so the replay window is asserted by a test that moves the
clock rather than one that sleeps.

## Consumer CI

`.github/workflows/import-lint.yml` in this repo is meant to be copied into
each consuming project (draftright, liseuse, bacnam) so a direct import of a
mail provider SDK fails CI there — emailkit is the only permitted path to a
provider.
