package emailkit

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"strings"
	"sync"
)

// Store is the project's own persistence. This package never sees a schema,
// which is how bacnam's tenant_id stays out of it: bacnam's implementation
// closes over the tenant and emailkit never learns tenancy exists.
//
// CONCURRENCY CONTRACT: implementations MUST be safe for concurrent use by
// multiple goroutines. Send and SendRaw are fire-and-forget — each spawns a
// goroutine — so N in-flight sends call these methods at the same time with no
// serialisation from this package. A *sql.DB-backed implementation satisfies
// this for free; a map-backed fake in a consumer's tests does not, and needs
// its own mutex.
//
// PII CONTRACT: returned errors MUST NOT embed the recipient address.
// emailkit logs the error verbatim ("err", err), so an implementation that
// wraps as fmt.Errorf("suppression lookup %s: %w", email, err) puts the full
// address into the very log line this package keeps to a bare domain. Wrap
// with the domain, a row id, or nothing — the audit row written by LogSend is
// the intended home for the address, under each project's own retention rules.
// emailkit cannot enforce this: redacting arbitrary error text is not
// reliable, which is why it is stated here as a contract.
//
// Store methods are also called on a goroutine the caller does not wait for.
// A method that blocks forever leaks that goroutine and stalls Wait; use the
// supplied ctx (or your driver's own timeout) to bound it.
//
// ADDRESS NORMALISATION CONTRACT: every address emailkit passes to Suppress or
// IsSuppressed has ALREADY been normalised by normalizeAddress — lowercased and
// whitespace-trimmed. Implementations MUST NOT re-case or otherwise rewrite it,
// and MAY compare it exactly (a plain `WHERE email = $1` on a plain column is
// correct; no LOWER() index or citext column is required). The point of stating
// it here is that both sides come from one definition in this package, so an
// implementation that added its own second normalisation would be a second
// source of truth for the same policy — and the two would drift.
//
// The address written to LogSend is deliberately NOT normalised: that row is the
// audit trail of what was actually mailed, and it should record the address as
// the caller gave it.
type Store interface {
	// send path
	IsSuppressed(ctx context.Context, email string) (bool, error)
	LogSend(ctx context.Context, r SendRecord) error
	Template(ctx context.Context, key string) (subject, html string, ok bool)

	// webhook path — two distinct operations, deliberately not merged. One
	// updates an existing log row by provider id; the other grows the
	// suppression list by address. A delivered event does only the first.
	//
	// IDEMPOTENCY CONTRACT: both operations MUST be idempotent — applying the
	// same event twice must reach the same state and MUST NOT error the second
	// time. WebhookHandler.Handle answers a failed store write with a retryable
	// error so the provider redelivers the event, and that redelivery replays
	// whichever of the two calls had already succeeded. Suppress written as a
	// bare INSERT therefore converts one transient failure into a permanent
	// duplicate-key loop: every retry fails on the row the first attempt wrote,
	// the event is never accepted, and — for a hard bounce — the address is
	// never suppressed. Use set semantics (an upsert, ON CONFLICT DO NOTHING,
	// or an equivalent) for Suppress, and a plain UPDATE by provider id for
	// MarkByProviderID. See storeFailure in webhook.go for why retry is the
	// right answer given this property.
	MarkByProviderID(ctx context.Context, providerID, status string, reason *string) error
	Suppress(ctx context.Context, email, reason string) error

	// WRITING SUPPRESSION ROWS FROM YOUR OWN CODE: the guarantee above covers
	// only addresses emailkit hands you — it has already lowercased and trimmed
	// those. Anything your project inserts itself (an admin unsubscribe form, a
	// complaint import, a manual block) is outside that path and must apply the
	// same lowercase-and-trim BEFORE storing.
	//
	// Skipping it reproduces, one layer out, the bug this contract exists to
	// prevent: a row stored as "User@Example.com" is never matched by the
	// lookup, which queries "user@example.com". The address stays mailable, and
	// nothing anywhere reports a problem — the suppression simply has no effect.
}

// SendRecord is the audit row. A thin struct so the port does not leak any
// project's generated query types into fakes.
type SendRecord struct {
	To, Type, Subject, Status string
	ProviderID, Error         *string
}

// Config carries per-project credentials. There is no default From: a shared
// module with one project's address baked in is the hardcoding this extraction
// exists to remove.
type Config struct {
	APIKey string
	From   string

	// Resolve, when non-nil, supplies credentials per send instead of the
	// static fields above, and REPLACES them rather than layering over them.
	//
	// draftright stores its Resend key in a table its admin UI writes, so an
	// operator must be able to change it without a restart. A static Config
	// would keep using the old value while nothing errors — the failure mode
	// where the mechanism moves and the symptom appears far from the change.
	//
	// An error from Resolve skips the send. It deliberately does NOT fall back
	// to APIKey/From: sending with credentials the operator believes they
	// replaced is worse than not sending.
	Resolve func(ctx context.Context) (apiKey, from string, err error)
}

// Reasons written to SendRecord.Error when a send does not reach the provider.
//
// Provider-neutral by design: Sender is explicitly the seam for SES or
// Postmark, so a message naming Resend would be wrong the moment a consumer
// supplies its own implementation via NewServiceWithSender.
const (
	reasonNotConfigured     = "Email provider not configured (no API key)"
	reasonSuppressed        = "Recipient on suppression list (bounce/complaint)"
	reasonSuppressionLookup = "Suppression lookup failed; send withheld"
	reasonNothingToSend     = "Empty subject and body; nothing to send"
	reasonPanicked          = "Send panicked; see application logs"
)

// Service is the only way to send. wg tracks in-flight sends so tests can
// await them deterministically; production never calls Wait.
type Service struct {
	store  Store
	cfg    Config
	reg    Registry
	client Sender
	wg     sync.WaitGroup

	// logger is where this Service writes its own diagnostics. Nil means
	// slog.Default(), resolved per call by log() rather than captured at
	// construction so a later slog.SetDefault still takes effect. It exists
	// so tests can capture output by injecting a logger instead of swapping
	// the process-wide default — global mutation would silently forbid
	// t.Parallel() in every test in this package.
	logger *slog.Logger
}

// log returns the destination for this Service's own diagnostics. See the
// logger field for why the fallback is resolved here and not in the
// constructor.
func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// NewService builds a Service on the production provider client. The client is
// constructed here rather than accepted as a parameter so that no exported
// function in this package ever returns a ready-to-use live provider client —
// the suppression check cannot be routed around.
func NewService(store Store, cfg Config, reg Registry) *Service {
	return NewServiceWithSender(store, cfg, reg, newResendSender())
}

// NewServiceWithSender builds a Service on a caller-supplied Sender. This is
// the seam for a different provider (SES, Postmark) or for a consumer's own
// test double.
//
// The supplied Sender is the CALLER'S responsibility: emailkit does not
// validate it, retry it, or bound its runtime beyond the caller's context.
// What emailkit does still guarantee is routing — every send through this
// Service, whichever Sender backs it, passes the suppression check and writes
// an audit row, because deliver() remains the only caller of Sender.Send.
//
// That guarantee covers sends made THROUGH the Service. A consumer that
// constructs its own Sender still holds that reference and can call Send on it
// directly, with no suppression check and no audit row; emailkit cannot
// prevent that. What NewService protects against is obtaining a live provider
// client FROM this package (see newResendSender) — a consumer's own Sender is
// a consumer's own discipline.
func NewServiceWithSender(store Store, cfg Config, reg Registry, s Sender) *Service {
	return &Service{store: store, cfg: cfg, reg: reg, client: s}
}

// Send renders key from the Store override (if any) or the Registry, then
// delivers. Fire-and-forget: an email must never block or fail the HTTP
// request that triggered it.
//
// The render happens on the send goroutine, not here. Resolving the template
// calls the consumer's Store.Template, and that used to run on the caller's
// goroutine — so a slow store blocked the HTTP request and a panicking one
// killed it, both of which contradict the fire-and-forget contract above.
//
// vars may be mutated or reused as soon as Send returns: it is copied here,
// on the CALLER'S goroutine, before the send goroutine can read it. Handing
// the caller's own map to a goroutine nobody waits for would be a genuine data
// race — the caller's next write and the render's read are unordered.
func (s *Service) Send(ctx context.Context, key, to string, vars map[string]string) {
	snapshot := maps.Clone(vars) // nil stays nil; render treats it as no vars
	s.fire(ctx, to, key, func(ctx context.Context) (string, string) {
		return s.render(ctx, key, snapshot)
	})
}

// SendRaw delivers a pre-rendered subject and body through the identical
// suppression → creds → send → log path. Used by callers with no template.
func (s *Service) SendRaw(ctx context.Context, to, subject, html, label string) {
	s.fire(ctx, to, label, func(context.Context) (string, string) { return subject, html })
}

// Wait blocks until in-flight sends finish. Test-only in practice, but
// exported because consumers' tests live in other packages.
//
// CONCURRENCY CONTRACT: Wait is only meaningful when no further Send or
// SendRaw calls are in flight or about to be made. It reports on the sends
// started before it was entered; a Send racing a Wait may or may not be
// awaited. Callers must sequence their own sends before calling it.
func (s *Service) Wait() { s.wg.Wait() }

// render resolves a template, preferring the Store override over the Registry.
//
// The override is wrapped in a one-entry Registry rather than calling
// substitute() directly, so the "subject raw, body escaped" policy has exactly
// one definition (Registry.Render). A second copy here drifted the moment one
// side changed.
func (s *Service) render(ctx context.Context, key string, vars map[string]string) (string, string) {
	if subj, html, ok := s.store.Template(ctx, key); ok {
		return Registry{key: {Subject: subj, Body: html}}.Render(key, vars)
	}
	return s.reg.Render(key, vars)
}

// fire runs one whole send on its own goroutine. render is a function rather
// than an already-rendered pair so that template resolution — which calls
// consumer code — happens inside this goroutine's recover(), off the caller's
// request goroutine.
func (s *Service) fire(ctx context.Context, to, label string, render func(context.Context) (string, string)) {
	s.wg.Add(1)
	go func() {
		// DEFER ORDER IS LOAD-BEARING: wg.Done is registered FIRST, so LIFO
		// runs it LAST — after the recover-and-log below. Wait() therefore
		// cannot return until the panic line has been written, which is what
		// lets the panic test read the log buffer right after Wait(). Register
		// it after the recover below instead and that test goes flaky.
		defer s.wg.Done()
		// WithoutCancel: the request that triggered the mail may return (and
		// cancel its ctx) long before the send finishes. Resolved before the
		// recover below so the panic path has a live context to audit on —
		// the caller's may already be cancelled, which is the whole point.
		sendCtx := context.WithoutCancel(ctx)
		// An email must never panic the caller's request — but a swallowed
		// panic left zero trace: no log, no stack, no audit row, the mail
		// simply vanished. Contain it AND record it.
		defer func() {
			if r := recover(); r != nil {
				// panicText, not r: rendering the value must not be left to
				// the log handler, which runs outside this recover. See
				// panicText.
				s.log().Error("email send panicked",
					"label", label, "domain", domainOf(to),
					"panic", panicText(r), "stack", string(debug.Stack()))
				s.auditPanic(sendCtx, to, label)
			}
		}()
		subject, html := render(sendCtx)
		s.deliver(sendCtx, to, subject, html, label)
	}()
}

// deliver is THE chokepoint. It is unexported and is the only caller of
// s.client.Send — see chokepoint_test.go. Every send therefore passes the
// suppression check by construction rather than by convention.
func (s *Service) deliver(ctx context.Context, to, subject, html, label string) {
	// Registry.Render documents an unknown key as "nothing to send" by
	// returning two empty strings; deliver used to ignore that and hand the
	// pair to the provider, so a typo'd key mailed a blank email and the audit
	// row said StatusSent. Honour the documented contract here.
	//
	// BOTH empty, not either: a subject with an empty body is a real (if
	// terse) email — a "your export is ready" notification with everything in
	// the subject line is legitimate — and an empty subject with a body is a
	// caller's formatting choice, not an unresolvable template.
	//
	// This is deliberately at the chokepoint rather than in Send, so it covers
	// SendRaw too. A SendRaw caller passing both fields empty is a different
	// CAUSE (its own bug rather than a bad key) but the same OUTCOME — a blank
	// email — and the outcome is what the recipient and the audit row see. One
	// rule here beats one copy per entry point.
	if subject == "" && html == "" {
		s.audit(ctx, to, subject, label, StatusSkipped, nil, strp(reasonNothingToSend))
		return
	}
	// Fail CLOSED on a lookup error. Treating an errored lookup as "not
	// suppressed" meant one database blip mailed every hard-bounced address
	// on the list — the exact outcome suppression exists to prevent. A
	// withheld email is recoverable; a complaint-driven domain reputation
	// hit is not.
	sup, err := s.store.IsSuppressed(ctx, normalizeAddress(to))
	if err != nil {
		s.log().Warn("suppression lookup failed; withholding send",
			"label", label, "domain", domainOf(to), "err", err)
		s.audit(ctx, to, subject, label, StatusSkipped, nil, strp(reasonSuppressionLookup))
		return
	}
	if sup {
		s.audit(ctx, to, subject, label, StatusSuppressed, nil, strp(reasonSuppressed))
		return
	}
	apiKey, from, err := s.credentials(ctx)
	if err != nil {
		s.log().Warn("email credential resolution failed",
			"label", label, "domain", domainOf(to), "err", err)
		s.audit(ctx, to, subject, label, StatusSkipped, nil, strp(reasonNotConfigured))
		return
	}
	if apiKey == "" {
		s.audit(ctx, to, subject, label, StatusSkipped, nil, strp(reasonNotConfigured))
		return
	}
	id, sendErr := s.client.Send(ctx, apiKey, from, to, subject, html)
	if sendErr != nil {
		// Recipient is logged as a hash-free domain only. The full address is
		// PII and the audit row already holds it under the project's own
		// retention rules; repeating it in application logs spreads it to a
		// second lifetime nobody manages.
		//
		// Scope of that guarantee, stated exactly: emailkit never puts the
		// address into a field it controls. It cannot vouch for "err" — that
		// text comes from a Store or Sender implementation, and redacting
		// arbitrary error strings is not reliable. Keeping the address out of
		// those errors is the implementation's contract; see Store.
		s.log().Warn("email send failed", "label", label, "domain", domainOf(to), "err", sendErr)
		s.audit(ctx, to, subject, label, StatusFailed, nil, strp(sendErr.Error()))
		return
	}
	s.audit(ctx, to, subject, label, StatusSent, strpOrNil(id), nil)
}

// credentials returns the key and from-address for this send. One definition,
// used by the single send path — see the Config.Resolve doc for why an error
// here must not fall back to the static fields.
func (s *Service) credentials(ctx context.Context) (apiKey, from string, err error) {
	if s.cfg.Resolve == nil {
		return s.cfg.APIKey, s.cfg.From, nil
	}
	return s.cfg.Resolve(ctx)
}

// panicUnprintable stands in for a recovered panic value that could not be
// rendered. It deliberately says nothing about the value: what just failed is
// the act of describing it, so there is nothing left worth trusting.
const panicUnprintable = "(unprintable panic value: formatting it panicked)"

// panicText renders a recovered panic value as a plain string, containing any
// panic that the rendering itself raises.
//
// The value comes from CONSUMER code and carries consumer methods. Handing it
// to slog as an attribute defers the formatting to the log handler, which runs
// OUTSIDE the deferred recover in fire() — so a String, Error or MarshalText
// method that panics escapes and takes the process down, which is the one
// outcome fire() exists to prevent. Rendering here, and passing slog a finished
// string, moves that work back inside a recover we own.
//
// fmt already contains the ordinary case (it prints "%!v(PANIC=String method:
// …)"), but it re-panics when formatting the recovered value panics in turn —
// a value that panics with itself does exactly that. So the containment cannot
// be delegated to fmt either.
//
// The value recovered below is deliberately NOT formatted or logged: formatting
// it is precisely what just failed.
//
// HONEST LIMIT: this recover() only catches a panic raised BY formatting r.
// It cannot protect against a String()/Error()/MarshalText() that recurses
// infinitely (the goroutine's stack simply exhausts) or one that triggers a
// runtime fatal error such as a concurrent map write. Both are fatal errors
// the runtime raises outside normal panic/recover, and no recover in this
// package — here or anywhere else — can catch them; the process still dies.
// Stated plainly rather than left for the surrounding code to imply full
// protection.
func panicText(r any) (text string) {
	defer func() {
		if recover() != nil {
			text = panicUnprintable
		}
	}()
	return fmt.Sprintf("%+v", r)
}

// auditPanic records a recovered panic in the audit trail, so the mail leaves a
// row rather than only a log line an operator has to already be looking at.
//
// The recovered value is NOT written to the row: it comes from consumer code
// and can embed the recipient address (a Store panicking inside a formatted
// message is the obvious way), and the row's Error column is not the place to
// give that a second copy. The full value with its stack is in the log line
// this is called from; the row names the cause and points there.
//
// Subject is empty because there may not be one: a panic in Store.Template
// means nothing was ever rendered, and inventing a subject would be worse than
// an honestly blank field.
//
// The write is wrapped in its own recover: a Store that panicked once may well
// panic again here, and a panic raised inside a deferred recover takes the
// whole process down — the exact outcome fire() exists to prevent.
func (s *Service) auditPanic(ctx context.Context, to, label string) {
	defer func() {
		if r := recover(); r != nil {
			s.log().Error("email panic audit write panicked",
				"label", label, "domain", domainOf(to))
		}
	}()
	s.audit(ctx, to, "", label, StatusFailed, nil, strp(reasonPanicked))
}

// audit writes the audit row. A failure here is not recoverable at this point —
// the mail decision has already been taken — but it must not be silent: this
// row is the only retained record of the recipient, so a store that has
// stopped accepting writes would otherwise lose every send trace invisibly.
func (s *Service) audit(ctx context.Context, to, subject, label, status string, providerID, errMsg *string) {
	if err := s.store.LogSend(ctx, SendRecord{
		To: to, Type: label, Subject: subject, Status: status,
		ProviderID: providerID, Error: errMsg,
	}); err != nil {
		s.log().Error("email audit write failed",
			"label", label, "domain", domainOf(to), "status", status, "err", err)
	}
}

// normalizeAddress is the SINGLE definition of the suppression-key policy: the
// form in which an address is both written to and read from the suppression
// list. Both halves of that policy must agree or the list does nothing — a hard
// bounce for "User@Example.com" that stored the raw address, while the next send
// queried "user@example.com", left an exact-match Store missing and the dead
// address being mailed forever. That is the whole harm the list exists to
// prevent, so the policy gets one definition and no call site spells it out
// again. See the Store doc comment for the contract this places on
// implementations.
//
// WHAT IT DOES, AND DELIBERATELY NOTHING MORE:
//
//   - Lowercase. RFC 5321 makes the local part technically case-SENSITIVE, but
//     no mailbox provider in practice treats "User@" and "user@" as different
//     people, and a provider echoing a bounce back in a different case is
//     routine. Folding case can therefore only over-suppress an address the
//     same human owns; not folding it under-suppresses a genuinely dead one.
//   - Trim surrounding whitespace. Free, and a provider may echo a padded
//     address out of its own payload. Leading/trailing space is never part of an
//     addr-spec, so trimming cannot change which mailbox is meant.
//
// NOT stripping "+tag" suffixes and NOT removing dots from the local part, even
// though both are Gmail conventions. Those change IDENTITY, not spelling:
// "a+news@x.com" and "a@x.com" are separate addresses at most providers, and
// collapsing them would suppress mail to an address that never bounced — locking
// a live user out of password resets on the strength of a different mailbox's
// bounce. Over-normalising fails in the direction that silently drops real mail,
// which is worse than the miss it would fix.
func normalizeAddress(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}

// domainOf reduces an address to the part safe to log. A malformed address
// yields "invalid" rather than the empty string: an empty domain= field is
// indistinguishable from a missing one and tells an operator nothing, whereas
// "invalid" says plainly that the address never had a domain to begin with.
// Both malformed shapes — no '@' at all, and a trailing '@' with nothing after
// it — therefore report the same thing.
func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 && i+1 < len(addr) {
		return addr[i+1:]
	}
	return "invalid"
}

func strp(s string) *string { return &s }

func strpOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
