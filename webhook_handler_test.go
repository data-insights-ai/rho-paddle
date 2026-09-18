package paddle_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	paddle "github.com/data-insights-ai/rho-paddle"
)

type recordingReceiver struct{ received int }

func (r *recordingReceiver) Receive(context.Context, integration.Message) error {
	r.received++
	return nil
}

// The status code is the contract with the provider's retry machinery. A
// request the verifier rejects must get a 4xx: a 5xx tells the provider to
// redeliver, so a forgery would be retried until the provider gave up, and
// nothing would ever reach the inbox for it anyway.
func TestWebhookHandlerRejectsUnverifiableRequestsWith4xx(t *testing.T) {
	receiver := &recordingReceiver{}
	handler, err := paddle.NewWebhookHandler(paddle.WebhookHandlerConfig{
		Verifier: newTestWebhookVerifier(t, resolverFunc(func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error) { return "account-a", nil })),
		Receiver: receiver,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"event_id":"evt_01forged","event_type":"transaction.completed","occurred_at":"2026-01-01T00:00:00Z","data":{}}`
	cases := map[string]map[string]string{
		"no signature":     {},
		"bad signature":    {"Paddle-Signature": "ts=1700000000;h1=deadbeef"},
		"malformed header": {"Paddle-Signature": "garbage"},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("status = %d, want 4xx", rec.Code)
			}
		})
	}
	if receiver.received != 0 {
		t.Fatalf("%d rejected requests reached the inbox", receiver.received)
	}
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}
