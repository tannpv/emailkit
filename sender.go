package emailkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Sender posts one email. resendClient satisfies it in production; tests and
// other providers supply their own. This is the third-case seam: adding SES or
// Postmark is a new implementation, not an edit here.
type Sender interface {
	Send(ctx context.Context, apiKey, from, to, subject, html string) (providerID string, err error)
}

// resendClient posts to the Resend HTTP API directly — no SDK, no dependency.
// Unexported on purpose: if a consumer could construct one, it could send
// without passing the suppression check in deliver().
//
// endpoint defaults to resendEndpoint in production; tests in this package
// point it at an httptest server instead. It stays unexported so no other
// package can construct or redirect a client.
type resendClient struct {
	http     *http.Client
	endpoint string
}

// resendRequestTimeout bounds how long a single Send call may block. This is
// a *shared* library imported by three independent products — we cannot
// assume every caller's context.Context carries a deadline, and one that
// doesn't (or one set generously by an unrelated subsystem) would otherwise
// let a hung or slow Resend response block the caller forever. A fixed
// client-level timeout is the backstop regardless of what the caller passes.
const resendRequestTimeout = 15 * time.Second

// newResendSender returns the production sender as an interface.
//
// Unexported deliberately. An exported constructor handed any consumer a live,
// ready-to-use provider client whose Send is exported, so
// `emailkit.NewResendSender().Send(...)` mailed a hard-bounced address with no
// suppression check and no audit row — bypassing the guarantee this whole
// package exists to provide. The only way to obtain a working sender is now
// NewService, which wires it behind deliver().
func newResendSender() Sender {
	return &resendClient{http: &http.Client{Timeout: resendRequestTimeout}, endpoint: resendEndpoint}
}

const resendEndpoint = "https://api.resend.com/emails"

// resendResponse is the subset of Resend's JSON response this client reads.
// Named rather than declared inline so providerError below can be defined on it
// once instead of the extraction being retyped per branch.
type resendResponse struct {
	Data *struct {
		ID string `json:"id"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// providerError returns the failure the provider named in its own response
// body, or nil when the body named none.
//
// One definition for both branches that consult it — the >=400 path and the 2xx
// path. They were two copies of "is out.Error populated, and if so what wording"
// with only their FALLBACKS differing (a status code vs. the missing-provider-id
// check), and the fallbacks are what stayed at the call sites. Two copies of one
// extraction is two things to change when the provider's error shape moves.
func (r resendResponse) providerError() error {
	if r.Error == nil || r.Error.Message == "" {
		return nil
	}
	return fmt.Errorf("resend: send failed: %s", r.Error.Message)
}

func (c *resendClient) Send(ctx context.Context, apiKey, from, to, subject, html string) (string, error) {
	body, err := json.Marshal(map[string]string{"from": from, "to": to, "subject": subject, "html": html})
	if err != nil {
		return "", fmt.Errorf("resend: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out resendResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&out)

	// >=400: prefer the provider's own error message; fall back to the status
	// code (never the raw body — it may contain recipient data) so production
	// logs at least say what failed.
	if resp.StatusCode >= 400 {
		if err := out.providerError(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("resend: send failed: status %d", resp.StatusCode)
	}

	// A 2xx with a body we couldn't parse is not success: without a decoded
	// provider ID the caller would record the email as sent with nothing to
	// reconcile the delivery webhook against.
	if decodeErr != nil {
		return "", fmt.Errorf("resend: send returned malformed response body: %w", decodeErr)
	}
	// A 2xx carrying an error object is not success either: on this path the
	// body is the authority, not the status line.
	if err := out.providerError(); err != nil {
		return "", err
	}
	if out.Data == nil || out.Data.ID == "" {
		return "", fmt.Errorf("resend: send returned no provider id")
	}
	return out.Data.ID, nil
}
