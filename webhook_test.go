package paddle_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	paddle "github.com/data-insights-ai/rho-paddle"
)

const (
	webhookSecret  = "synthetic-webhook-secret"
	webhookEventID = "evt_0123456789abcdefghijklmnop"
)

var webhookNow = time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)

type resolverFunc func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error)

func (f resolverFunc) ResolveAccount(ctx context.Context, scope billing.Scope, event paddle.Event) (billing.AccountID, error) {
	return f(ctx, scope, event)
}

type receiverFunc func(context.Context, integration.Message) error

func (f receiverFunc) Receive(ctx context.Context, message integration.Message) error {
	return f(ctx, message)
}

func TestWebhookVerificationPrecedesResolverAndReceive(t *testing.T) {
	body := webhookBody(webhookEventID, "notification-a", `{"customer_id":"ctm_1"}`)
	resolverCalls := 0
	receiveCalls := 0
	verifier := newTestWebhookVerifier(t, resolverFunc(func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error) {
		resolverCalls++
		return "account-webhook", nil
	}))
	receiver := receiverFunc(func(context.Context, integration.Message) error {
		receiveCalls++
		return nil
	})

	_, err := integration.ReceiveWebhook(t.Context(), receiver, verifier, body, webhookHeaders("bad-signature"), webhookScope())
	if !errors.Is(err, paddle.ErrSignature) {
		t.Fatalf("ReceiveWebhook() error = %v, want %v", err, paddle.ErrSignature)
	}
	if resolverCalls != 0 || receiveCalls != 0 {
		t.Fatalf("unauthenticated body reached resolver or receiver: resolver=%d receiver=%d", resolverCalls, receiveCalls)
	}

	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] = '2'
	_, err = integration.ReceiveWebhook(t.Context(), receiver, verifier, tampered, signedWebhookHeaders(body), webhookScope())
	if !errors.Is(err, paddle.ErrSignature) {
		t.Fatalf("tampered ReceiveWebhook() error = %v, want %v", err, paddle.ErrSignature)
	}
	if resolverCalls != 0 || receiveCalls != 0 {
		t.Fatalf("tampered body reached resolver or receiver: resolver=%d receiver=%d", resolverCalls, receiveCalls)
	}
}

func TestWebhookRejectsAmbiguousHeadersAndJSON(t *testing.T) {
	resolverCalls := 0
	verifier := newTestWebhookVerifier(t, resolverFunc(func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error) {
		resolverCalls++
		return "account-webhook", nil
	}))

	body := webhookBody(webhookEventID, "notification-a", `{}`)
	signature := webhookSignature(body)
	tests := []struct {
		name    string
		body    []byte
		headers integration.WebhookHeaders
		want    error
	}{
		{
			name:    "duplicate signature values",
			body:    body,
			headers: integration.WebhookHeaders{"Paddle-Signature": {signature, signature}},
			want:    paddle.ErrSignature,
		},
		{
			name:    "duplicate case-insensitive signature headers",
			body:    body,
			headers: integration.WebhookHeaders{"Paddle-Signature": {signature}, "paddle-signature": {signature}},
			want:    paddle.ErrSignature,
		},
		{
			name: "duplicate top-level JSON key",
			body: []byte(`{"event_id":"` + webhookEventID + `","event_id":"` + webhookEventID + `","event_type":"transaction-completed","occurred_at":"2026-09-16T08:00:00Z","data":{}}`),
			want: paddle.ErrInvalid,
		},
		{
			name: "duplicate nested JSON key",
			body: webhookBody(webhookEventID, "notification-a", `{"customer_id":"one","customer_id":"two"}`),
			want: paddle.ErrInvalid,
		},
		{
			name: "excessive JSON depth",
			body: webhookBody(webhookEventID, "notification-a", nestedObject(66)),
			want: paddle.ErrInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := tt.headers
			if headers == nil {
				headers = signedWebhookHeaders(tt.body)
			}
			_, err := verifier.Verify(t.Context(), tt.body, headers)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.want)
			}
		})
	}
	if resolverCalls != 0 {
		t.Fatalf("invalid input reached resolver %d times", resolverCalls)
	}
}

func TestWebhookResolverCannotMutateRetainedEvidence(t *testing.T) {
	body := webhookBody(webhookEventID, "notification-a", `{"customer_id":"ctm_original"}`)
	verifier := newTestWebhookVerifier(t, resolverFunc(func(_ context.Context, scope billing.Scope, event paddle.Event) (billing.AccountID, error) {
		if scope != webhookScope() {
			t.Fatalf("resolver scope = %+v, want %+v", scope, webhookScope())
		}
		// The provider payload is unexported, so a resolver cannot reach the
		// retained evidence to mutate it at all — the guarantee this test used
		// to check at runtime is now enforced by the type system. What is still
		// worth asserting is that the accessors read the original bytes and the
		// stored payload is untouched.
		if event.CustomerID() != "ctm_original" {
			t.Fatalf("resolver saw customer %q, want ctm_original", event.CustomerID())
		}
		return "account-webhook", nil
	}))

	message, err := verifier.Verify(t.Context(), body, signedWebhookHeaders(body))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !strings.Contains(string(message.Payload), `"customer_id":"ctm_original"`) {
		t.Fatalf("resolver changed retained evidence: %s", message.Payload)
	}
	if strings.Contains(string(message.Payload), "notification-a") {
		t.Fatalf("delivery metadata retained in event evidence: %s", message.Payload)
	}
}

func TestWebhookCanceledContextStopsBeforeResolver(t *testing.T) {
	resolverCalls := 0
	verifier := newTestWebhookVerifier(t, resolverFunc(func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error) {
		resolverCalls++
		return "account-webhook", nil
	}))
	body := webhookBody(webhookEventID, "notification-a", `{}`)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := verifier.Verify(ctx, body, signedWebhookHeaders(body))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify() error = %v, want %v", err, context.Canceled)
	}
	if resolverCalls != 0 {
		t.Fatalf("canceled request reached resolver %d times", resolverCalls)
	}
}

func newTestWebhookVerifier(t *testing.T, resolver paddle.AccountResolver) *paddle.WebhookVerifier {
	t.Helper()
	verifier, err := paddle.NewWebhookVerifier(paddle.WebhookConfig{
		Merchant:    "merchant-test",
		Environment: paddle.Sandbox,
		Secrets:     []string{webhookSecret},
		Resolver:    resolver,
		Now:         func() time.Time { return webhookNow },
		Tolerance:   time.Minute,
	})
	if err != nil {
		t.Fatalf("NewWebhookVerifier() error = %v", err)
	}
	return verifier
}

func webhookScope() billing.Scope {
	return billing.Scope{Provider: "paddle", Merchant: "merchant-test", Environment: "sandbox"}
}

func webhookBody(eventID, notificationID, data string) []byte {
	return []byte(`{"event_id":"` + eventID + `","event_type":"transaction-completed","occurred_at":"2026-09-16T08:00:00Z","notification_id":"` + notificationID + `","data":` + data + `}`)
}

func signedWebhookHeaders(body []byte) integration.WebhookHeaders {
	return webhookHeaders(webhookSignature(body))
}

func webhookHeaders(signature string) integration.WebhookHeaders {
	return integration.WebhookHeaders{"Paddle-Signature": {signature}}
}

func webhookSignature(body []byte) string {
	timestamp := strconv.FormatInt(webhookNow.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	_, _ = mac.Write([]byte(timestamp + ":"))
	_, _ = mac.Write(body)
	return "ts=" + timestamp + ";h1=" + hex.EncodeToString(mac.Sum(nil))
}

func nestedObject(depth int) string {
	return strings.Repeat(`{"value":`, depth) + `0` + strings.Repeat(`}`, depth)
}

// The inbox payload is persisted evidence that later worker runs decode, so its
// shape is part of the durable contract. Unexporting the provider bytes emptied
// every stored event until Event got an explicit JSON encoding; this pins it.
func TestEventPayloadRoundTripsThroughTheDurableShape(t *testing.T) {
	body := webhookBody(webhookEventID, "notification-a", `{"customer_id":"ctm_original","id":"txn_x"}`)
	verifier := newTestWebhookVerifier(t, resolverFunc(func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error) {
		return "account-webhook", nil
	}))
	message, err := verifier.Verify(t.Context(), body, signedWebhookHeaders(body))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	var stored struct {
		ID   string          `json:"event_id"`
		Type string          `json:"event_type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(message.Payload, &stored); err != nil {
		t.Fatalf("stored payload is not the durable shape: %v", err)
	}
	if stored.ID != webhookEventID || stored.Type == "" || len(stored.Data) == 0 {
		t.Fatalf("durable payload lost a field: %+v", stored)
	}
	var round paddle.Event
	if err := json.Unmarshal(message.Payload, &round); err != nil {
		t.Fatalf("Event does not decode its own durable shape: %v", err)
	}
	if round.ID != webhookEventID || round.CustomerID() != "ctm_original" {
		t.Fatalf("round trip lost provider evidence: id=%q customer=%q", round.ID, round.CustomerID())
	}
}
