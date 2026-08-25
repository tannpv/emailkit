package emailkit

import (
	"context"
	"errors"
	"testing"
)

type fakeStore struct {
	suppressed bool
	logs       []SendRecord
	tmpl       *TemplateDef
}

func (f *fakeStore) IsSuppressed(context.Context, string) (bool, error) { return f.suppressed, nil }
func (f *fakeStore) LogSend(_ context.Context, r SendRecord) error {
	f.logs = append(f.logs, r)
	return nil
}
func (f *fakeStore) Template(context.Context, string) (string, string, bool) {
	if f.tmpl == nil {
		return "", "", false
	}
	return f.tmpl.Subject, f.tmpl.Body, true
}
func (f *fakeStore) MarkByProviderID(context.Context, string, string, *string) error { return nil }
func (f *fakeStore) Suppress(context.Context, string, string) error                  { return nil }

// fakeSvcSender is named distinctly from sender_test.go's fakeSender (same
// package, both are _test.go files) to avoid a redeclaration error.
type fakeSvcSender struct {
	calls int
	id    string
	err   error
}

func (s *fakeSvcSender) Send(context.Context, string, string, string, string, string) (string, error) {
	s.calls++
	return s.id, s.err
}

func newTestService(st *fakeStore, sn *fakeSvcSender, key string) *Service {
	return NewService(st, Config{APIKey: key, From: "T <t@example.com>"},
		Registry{"k": {Subject: "s", Body: "b"}}, sn)
}

func TestDeliver_SuppressedSkips(t *testing.T) {
	st := &fakeStore{suppressed: true}
	sn := &fakeSvcSender{}
	svc := newTestService(st, sn, "key")
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()
	if sn.calls != 0 {
		t.Fatal("suppressed address must never reach the sender")
	}
	if len(st.logs) != 1 || st.logs[0].Status != StatusSuppressed {
		t.Fatalf("want one suppressed log, got %+v", st.logs)
	}
}

func TestDeliver_NoKeySkips(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{}
	svc := newTestService(st, sn, "")
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()
	if sn.calls != 0 {
		t.Fatal("must not attempt a send with no API key")
	}
	if len(st.logs) != 1 || st.logs[0].Status != StatusSkipped {
		t.Fatalf("want one skipped log, got %+v", st.logs)
	}
}

func TestDeliver_SendsAndLogsSent(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{id: "prov-1"}
	svc := newTestService(st, sn, "key")
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()
	if sn.calls != 1 {
		t.Fatalf("want 1 send, got %d", sn.calls)
	}
	if len(st.logs) != 1 || st.logs[0].Status != StatusSent {
		t.Fatalf("want one sent log, got %+v", st.logs)
	}
	if st.logs[0].ProviderID == nil || *st.logs[0].ProviderID != "prov-1" {
		t.Fatal("provider id must be recorded — the webhook joins on it")
	}
}

func TestDeliver_SendFailLogsFailed(t *testing.T) {
	st := &fakeStore{}
	sn := &fakeSvcSender{err: errors.New("boom")}
	svc := newTestService(st, sn, "key")
	svc.Send(context.Background(), "k", "a@b.c", nil)
	svc.Wait()
	if len(st.logs) != 1 || st.logs[0].Status != StatusFailed {
		t.Fatalf("want one failed log, got %+v", st.logs)
	}
	if st.logs[0].Error == nil || *st.logs[0].Error != "boom" {
		t.Fatal("failure reason must be recorded")
	}
}
