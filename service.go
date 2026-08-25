package emailkit

import (
	"context"
	"log/slog"
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
func NewServiceWithSender(store Store, cfg Config, reg Registry, s Sender) *Service {
	return &Service{store: store, cfg: cfg, reg: reg, client: s}
}

// Send renders key from the Store override (if any) or the Registry, then
// delivers. Fire-and-forget: an email must never block or fail the HTTP
// request that triggered it.
func (s *Service) Send(ctx context.Context, key, to string, vars map[string]string) {
	subject, html := s.render(ctx, key, vars)
	s.fire(ctx, to, subject, html, key)
}

// SendRaw delivers a pre-rendered subject and body through the identical
// suppression → creds → send → log path. Used by callers with no template.
func (s *Service) SendRaw(ctx context.Context, to, subject, html, label string) {
	s.fire(ctx, to, subject, html, label)
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

func (s *Service) fire(ctx context.Context, to, subject, html, label string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// An email must never panic the caller's request — but a swallowed
		// panic left zero trace: no log, no stack, no audit row, the mail
		// simply vanished. Contain it AND record it.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("email send panicked",
					"label", label, "domain", domainOf(to),
					"panic", r, "stack", string(debug.Stack()))
			}
		}()
		s.deliver(context.WithoutCancel(ctx), to, subject, html, label)
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
		slog.Warn("suppression lookup failed; withholding send",
			"label", label, "domain", domainOf(to), "err", err)
		s.log(ctx, to, subject, label, StatusSkipped, nil, strp(reasonSuppressionLookup))
		return
	}
	if sup {
		s.log(ctx, to, subject, label, StatusSuppressed, nil, strp(reasonSuppressed))
		return
	}
	if s.cfg.APIKey == "" {
		s.log(ctx, to, subject, label, StatusSkipped, nil, strp(reasonNotConfigured))
		return
	}
	id, err := s.client.Send(ctx, s.cfg.APIKey, s.cfg.From, to, subject, html)
	if err != nil {
		// Recipient is logged as a hash-free domain only. The full address is
		// PII and the audit row already holds it under the project's own
		// retention rules; repeating it in application logs spreads it to a
		// second lifetime nobody manages.
		slog.Warn("email send failed", "label", label, "domain", domainOf(to), "err", err)
		s.log(ctx, to, subject, label, StatusFailed, nil, strp(err.Error()))
		return
	}
	s.log(ctx, to, subject, label, StatusSent, strpOrNil(id), nil)
}

// log writes the audit row. A failure here is not recoverable at this point —
// the mail decision has already been taken — but it must not be silent: this
// row is the only retained record of the recipient, so a store that has
// stopped accepting writes would otherwise lose every send trace invisibly.
func (s *Service) log(ctx context.Context, to, subject, label, status string, providerID, errMsg *string) {
	if err := s.store.LogSend(ctx, SendRecord{
		To: to, Type: label, Subject: subject, Status: status,
		ProviderID: providerID, Error: errMsg,
	}); err != nil {
		slog.Error("email audit write failed",
			"label", label, "domain", domainOf(to), "status", status, "err", err)
	}
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
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
