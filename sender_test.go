package emailkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestResendClient points a resendClient at an httptest server instead of
// the real Resend API. resendClient is unexported, but this file lives in
// package emailkit (not emailkit_test), so it can reach the unexported
// endpoint field without any test-only export being added to the public API.
func newTestResendClient(url string) *resendClient {
	return &resendClient{http: &http.Client{Timeout: resendRequestTimeout}, endpoint: url}
}

func TestSend_WellFormedBody_ReturnsProviderID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"abc123"}}`))
	}))
	defer ts.Close()

	c := newTestResendClient(ts.URL)
	id, err := c.Send(context.Background(), "secret-key", "from@example.com", "to@example.com", "subj", "<p>hi</p>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "abc123" {
		t.Fatalf("want provider id %q, got %q", "abc123", id)
	}
}

// TestSend_MalformedBody_ReturnsError is the regression test for Important
// finding 1: a 2xx with a malformed/truncated/non-JSON body must be reported
// as a failure, not silently treated as success with no provider ID.
func TestSend_MalformedBody_ReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer ts.Close()

	c := newTestResendClient(ts.URL)
	id, err := c.Send(context.Background(), "secret-key", "from@example.com", "to@example.com", "subj", "html")
	if err == nil {
		t.Fatalf("want error for malformed 2xx body, got id=%q", id)
	}
	if id != "" {
		t.Fatalf("want empty provider id on error, got %q", id)
	}
	if strings.Contains(err.Error(), "to@example.com") || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("error must not leak recipient or api key, got %q", err.Error())
	}
}

func TestSend_ValidJSONNoProviderID_ReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := newTestResendClient(ts.URL)
	id, err := c.Send(context.Background(), "secret-key", "from@example.com", "to@example.com", "subj", "html")
	if err == nil {
		t.Fatalf("want error when 2xx body carries no provider id, got id=%q", id)
	}
	if id != "" {
		t.Fatalf("want empty provider id on error, got %q", id)
	}
}

func TestSend_ErrorResponse_SurfacesProviderMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid recipient domain"}}`))
	}))
	defer ts.Close()

	c := newTestResendClient(ts.URL)
	_, err := c.Send(context.Background(), "secret-key", "from@example.com", "to@example.com", "subj", "html")
	if err == nil {
		t.Fatal("want error for >=400 response")
	}
	if !strings.Contains(err.Error(), "invalid recipient domain") {
		t.Fatalf("want provider error message surfaced, got %q", err.Error())
	}
}

func TestSend_ErrorResponseNoMessage_IncludesStatusCode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := newTestResendClient(ts.URL)
	_, err := c.Send(context.Background(), "secret-key", "from@example.com", "to@example.com", "subj", "html")
	if err == nil {
		t.Fatal("want error for >=400 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("want status code in error, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "to@example.com") || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("error must not leak recipient or api key, got %q", err.Error())
	}
}
