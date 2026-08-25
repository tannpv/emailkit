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

```go
svc := emailkit.NewService(
    myStore,                                  // your Store implementation
    emailkit.Config{APIKey: key, From: from}, // your credentials
    myTemplates,                              // your Registry
)
svc.Send(ctx, "password-reset", user.Email, map[string]string{"code": code})
```

`NewService` builds the Resend client internally, so no exported function ever
hands a consumer a ready-to-use provider client — the suppression check in
`deliver()` cannot be routed around. To supply your own provider (SES,
Postmark, or a test double), use the seam instead:

```go
svc := emailkit.NewServiceWithSender(myStore, cfg, myTemplates, mySender)
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
            log.Error("webhook store write failed", "err", err)
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

## Consumer CI

`.github/workflows/import-lint.yml` in this repo is meant to be copied into
each consuming project (draftright, liseuse, bacnam) so a direct import of a
mail provider SDK fails CI there — emailkit is the only permitted path to a
provider.
