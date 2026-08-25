package emailkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
type resendClient struct{ http *http.Client }

// NewResendSender returns the production sender as an interface, so the
// concrete type cannot be named or built outside this package.
func NewResendSender() Sender { return &resendClient{http: &http.Client{}} }

const resendEndpoint = "https://api.resend.com/emails"

func (c *resendClient) Send(ctx context.Context, apiKey, from, to, subject, html string) (string, error) {
	body, _ := json.Marshal(map[string]string{"from": from, "to": to, "subject": subject, "html": html})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resendEndpoint, bytes.NewReader(body))
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
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 400 || out.Error != nil {
		msg := "send failed"
		if out.Error != nil {
			msg = out.Error.Message
		}
		return "", fmt.Errorf("%s", msg)
	}
	if out.Data != nil {
		return out.Data.ID, nil
	}
	return "", nil
}
