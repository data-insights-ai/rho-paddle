package paddle

import (
	"errors"
	"io"
	"net/http"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

// WebhookHandlerConfig wires the verifier to the durable inbox.
type WebhookHandlerConfig struct {
	// Verifier authenticates the raw request body.
	Verifier *WebhookVerifier
	// Receiver is the billing core's inbox, normally store.Queue().
	Receiver integration.WebhookReceiver
	// OnError observes a rejected or undeliverable notification. It is the only
	// place detail is surfaced: the HTTP response deliberately says nothing
	// about why verification failed, because the caller is unauthenticated.
	// Optional.
	OnError func(*http.Request, error)
}

// NewWebhookHandler returns the HTTP endpoint a provider notification
// destination posts to.
//
// The status codes are the contract with the provider's retry machinery, and
// getting them wrong is expensive in both directions: a 5xx for an event
// already stored invites a redelivery storm, and a 2xx for one that was never
// stored loses it silently. That mapping is small, easy to get subtly wrong,
// and identical for every host, so it lives here rather than in each one.
//
// The response says only whether the event was durably received. Applying it to
// billing state happens later, in the worker that drains the inbox: a provider
// must not wait on a host's database work.
func NewWebhookHandler(config WebhookHandlerConfig) (http.Handler, error) {
	if config.Verifier == nil || config.Receiver == nil {
		return nil, ErrInvalid
	}
	scope := config.Verifier.Scope()
	if !scope.Valid() {
		return nil, ErrInvalid
	}
	return &webhookHandler{config: config, scope: scope}, nil
}

type webhookHandler struct {
	config WebhookHandlerConfig
	scope  billing.Scope
}

func (h *webhookHandler) fail(r *http.Request, err error) {
	if h.config.OnError != nil {
		h.config.OnError(r, err)
	}
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, integration.MaxWebhookBody))
	if err != nil {
		// Oversized or truncated. Retrying a body that was never read is
		// harmless, so ask for one.
		h.fail(r, err)
		http.Error(w, "unreadable request body", http.StatusBadRequest)
		return
	}

	_, err = integration.ReceiveWebhook(r.Context(), h.config.Receiver, h.config.Verifier,
		body, integration.WebhookHeaders(r.Header), h.scope)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, billing.ErrConflict):
		// Already received. A duplicate delivery is a success from the
		// provider's side; telling it otherwise invites a retry storm for an
		// event that is safely stored.
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrSignature), errors.Is(err, ErrTimestamp), errors.Is(err, ErrInvalid),
		errors.Is(err, billing.ErrInvalid), errors.Is(err, billing.ErrNotFound):
		// A bad or missing signature, a stale timestamp, an unparseable
		// payload, or an event this host cannot attribute to an account.
		// Redelivery will not change any of those, and answering 5xx would
		// have the provider retry a forgery until it gave up. The verifier's
		// own errors are listed explicitly: they are distinct values from the
		// core's, and mapping only the core's sent every rejection to 503.
		h.fail(r, err)
		http.Error(w, "rejected", http.StatusBadRequest)
	default:
		// The host's problem, not the provider's. 5xx asks for redelivery.
		h.fail(r, err)
		http.Error(w, "could not record event", http.StatusServiceUnavailable)
	}
}

// Scope reports the provider scope this verifier accepts events for.
func (v *WebhookVerifier) Scope() billing.Scope {
	if v == nil {
		return billing.Scope{}
	}
	return v.scope
}
