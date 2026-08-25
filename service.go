package emailkit

import (
	"context"
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
type Store interface {
	// send path
	IsSuppressed(ctx context.Context, email string) (bool, error)
	LogSend(ctx context.Context, r SendRecord) error
	Template(ctx context.Context, key string) (subject, html string, ok bool)

	// webhook path — two distinct operations, deliberately not merged. One
	// updates an existing log row by provider id; the other grows the
	// suppression list by address. A delivered event does only the first.
	MarkByProviderID(ctx context.Context, providerID, status string, reason *string) error
	Suppress(ctx context.Context, email, reason string) error
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
		// lets the panic test read the log buffer right after Wait(). Swap
		// these two lines and that test goes flaky.
		defer s.wg.Done()
		// An email must never panic the caller's request — but a swallowed
		// panic left zero trace: no log, no stack, no audit row, the mail
		// simply vanished. Contain it AND record it.
		defer func() {
			if r := recover(); r != nil {
				s.log().Error("email send panicked",
					"label", label, "domain", domainOf(to),
					"panic", r, "stack", string(debug.Stack()))
			}
		}()
		// WithoutCancel: the request that triggered the mail may return (and
		// cancel its ctx) long before the send finishes.
		sendCtx := context.WithoutCancel(ctx)
		subject, html := render(sendCtx)
		s.deliver(sendCtx, to, subject, html, label)
	}()
}

// deliver is THE chokepoint. It is unexported and is the only caller of
// s.client.Send — see chokepoint_test.go. Every send therefore passes the
// suppression check by construction rather than by convention.
func (s *Service) deliver(ctx context.Context, to, subject, html, label string) {
	// Fail CLOSED on a lookup error. Treating an errored lookup as "not
	// suppressed" meant one database blip mailed every hard-bounced address
	// on the list — the exact outcome suppression exists to prevent. A
	// withheld email is recoverable; a complaint-driven domain reputation
	// hit is not.
	sup, err := s.store.IsSuppressed(ctx, strings.ToLower(to))
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
	if s.cfg.APIKey == "" {
		s.audit(ctx, to, subject, label, StatusSkipped, nil, strp(reasonNotConfigured))
		return
	}
	id, err := s.client.Send(ctx, s.cfg.APIKey, s.cfg.From, to, subject, html)
	if err != nil {
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
		s.log().Warn("email send failed", "label", label, "domain", domainOf(to), "err", err)
		s.audit(ctx, to, subject, label, StatusFailed, nil, strp(err.Error()))
		return
	}
	s.audit(ctx, to, subject, label, StatusSent, strpOrNil(id), nil)
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
