package paddle

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/data-insights-ai/rho-billing/provider"

	"github.com/data-insights-ai/rho-paddle/internal/signature"
)

const (
	maxWebhookBody = 1 << 20
	// signatureTolerance bounds replays of a captured webhook.
	signatureTolerance = 5 * time.Minute
)

// Verify checks a webhook's Paddle-Signature against the raw body and
// decodes it.
func (c *Client) Verify(header string, body []byte) (provider.Event, error) {
	if err := signature.Verify(body, header, c.secrets, c.now(), signatureTolerance); err != nil {
		return provider.Event{}, errors.Join(provider.ErrSignature, err)
	}
	return Decode(body)
}

// Webhooks serves Paddle's notifications: verified events go to handle;
// Paddle hears 200 when handle succeeded or the event is one the host
// ignores, and retries anything else. handle must treat an event it has
// processed before as success, since Paddle delivers at least once.
func (c *Client) Webhooks(handle func(context.Context, provider.Event) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
		if err != nil || len(body) > maxWebhookBody {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		e, err := c.Verify(r.Header.Get("Paddle-Signature"), body)
		switch {
		case errors.Is(err, provider.ErrIgnored):
			w.WriteHeader(http.StatusOK)
			return
		case errors.Is(err, provider.ErrSignature):
			w.WriteHeader(http.StatusUnauthorized)
			return
		case err != nil:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := handle(r.Context(), e); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
