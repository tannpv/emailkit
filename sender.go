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

// NewResendSender returns the production sender as an interface, so the
// concrete type cannot be named or built outside this package.
func NewResendSender() Sender {
	return &resendClient{http: &http.Client{Timeout: resendRequestTimeout}, endpoint: resendEndpoint}
}

const resendEndpoint = "https://api.resend.com/emails"

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
	var out struct {
		Data *struct {
			ID string `json:"id"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&out)

	// >=400: prefer the provider's own error message; fall back to the status
	// code (never the raw body — it may contain recipient data) so production
	// logs at least say what failed.
	if resp.StatusCode >= 400 {
		if out.Error != nil && out.Error.Message != "" {
			return "", fmt.Errorf("resend: send failed: %s", out.Error.Message)
		}
		return "", fmt.Errorf("resend: send failed: status %d", resp.StatusCode)
	}

	// A 2xx with a body we couldn't parse is not success: without a decoded
	// provider ID the caller would record the email as sent with nothing to
	// reconcile the delivery webhook against.
	if decodeErr != nil {
		return "", fmt.Errorf("resend: send returned malformed response body: %w", decodeErr)
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", fmt.Errorf("resend: send failed: %s", out.Error.Message)
	}
	if out.Data == nil || out.Data.ID == "" {
		return "", fmt.Errorf("resend: send returned no provider id")
	}
	return out.Data.ID, nil
}
