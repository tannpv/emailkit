package emailkit

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// Store is the project's own persistence. This package never sees a schema,
// which is how bacnam's tenant_id stays out of it: bacnam's implementation
// closes over the tenant and emailkit never learns tenancy exists.
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

// Service is the only way to send. wg tracks in-flight sends so tests can
// await them deterministically; production never calls Wait.
type Service struct {
	store  Store
	cfg    Config
	reg    Registry
	client Sender
	wg     sync.WaitGroup
}

func NewService(st Store, cfg Config, reg Registry, s Sender) *Service {
	return &Service{store: st, cfg: cfg, reg: reg, client: s}
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
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) render(ctx context.Context, key string, vars map[string]string) (string, string) {
	if subj, html, ok := s.store.Template(ctx, key); ok {
		return substitute(subj, vars, false), substitute(html, vars, true)
	}
	return s.reg.Render(key, vars)
}

func (s *Service) fire(ctx context.Context, to, subject, html, label string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { _ = recover() }() // an email must never panic a request
		s.deliver(context.WithoutCancel(ctx), to, subject, html, label)
	}()
}

// deliver is THE chokepoint. It is unexported and is the only caller of
// s.client.Send — see chokepoint_test.go. Every send therefore passes the
// suppression check by construction rather than by convention.
func (s *Service) deliver(ctx context.Context, to, subject, html, label string) {
	if sup, err := s.store.IsSuppressed(ctx, strings.ToLower(to)); err == nil && sup {
		s.log(ctx, to, subject, label, StatusSuppressed, nil,
			strp("Recipient on suppression list (bounce/complaint)"))
		return
	}
	if s.cfg.APIKey == "" {
		s.log(ctx, to, subject, label, StatusSkipped, nil, strp("Resend not configured"))
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

func (s *Service) log(ctx context.Context, to, subject, label, status string, providerID, errMsg *string) {
	_ = s.store.LogSend(ctx, SendRecord{
		To: to, Type: label, Subject: subject, Status: status,
		ProviderID: providerID, Error: errMsg,
	})
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
