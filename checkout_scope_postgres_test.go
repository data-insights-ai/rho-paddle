package paddle_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestCheckoutDispatchPostgresRejectsForeignIntentScopeBeforeBegin(t *testing.T) {
	f := newPaymentFixture(t, "checkout-foreign-scope", false)
	calls := 0
	client, err := paddle.New(paddle.Config{
		Merchant:    "merchant-test",
		Environment: paddle.Sandbox,
		APIKey:      testAPIKey,
		HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("provider transport must not be called")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return f.clock.Add(time.Minute) }
	purchases := purchase.New(f.store.Purchases(), now)
	foreignInput := f.intent.IntentInput
	foreignInput.ID = "intent-foreign-provider-scope"
	foreignInput.Operation = "operation-foreign-intent-scope"
	foreignInput.Scope = billing.Scope{Provider: "paddle", Merchant: "foreign-merchant", Environment: "sandbox"}
	foreign, err := purchases.CreateIntent(t.Context(), foreignInput)
	if err != nil {
		t.Fatal(err)
	}

	line := paddle.CheckoutDispatchLine{
		QuoteLineID: f.binding.Lines[0].Allocations[0].QuoteLineID,
		Price:       billing.Reference{Scope: client.Scope(), ID: f.binding.Lines[0].ProviderPriceID},
		Quantity:    1,
	}
	correlation := checkoutScopeDigest(f.account, foreign.Operation, foreign.ID, foreign.QuoteFingerprint, f.binding.CustomerID, []paddle.CheckoutDispatchLine{line}, "")
	payload := paddle.CheckoutDispatch{
		Account: f.account, IntentID: foreign.ID, Operation: foreign.Operation,
		QuoteFingerprint: foreign.QuoteFingerprint,
		Customer:         billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID},
		Lines:            []paddle.CheckoutDispatchLine{line}, Correlation: correlation,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := integration.ProviderMessageID(client.Scope().Provider, client.Scope().Merchant, client.Scope().Environment, foreign.Operation)
	if err != nil {
		t.Fatal(err)
	}
	message := integration.Message{
		Account: f.account, ID: messageID, Scope: client.Scope(), Kind: "checkout.dispatch", Direction: integration.Outbound,
		OccurredAt: foreign.CreatedAt, Payload: raw,
	}
	if err := f.store.Atomic(t.Context(), f.account, func(session integration.Session) error {
		return session.Enqueue(t.Context(), message)
	}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "foreign-scope-worker", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v error=%v", ok, err)
	}
	service, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := service.Dispatch(t.Context(), claim); !errors.Is(err, billing.ErrConflict) || got.Reference.ID != "" {
		t.Fatalf("dispatch=%+v error=%v", got, err)
	}
	if calls != 0 {
		t.Fatalf("provider calls=%d, want zero", calls)
	}
	delivery, err := f.store.Queue().Outbox(t.Context(), f.account, client.Scope(), message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.State != "processing" || !delivery.BegunAt.IsZero() || delivery.LastResult != nil || delivery.ProviderReference != "" {
		t.Fatalf("delivery mutated by rejected begin: %+v", delivery)
	}
	current, err := purchases.Intent(t.Context(), f.account, foreign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Command != purchase.CommandPlanned || current.Revision != foreign.Revision {
		t.Fatalf("intent mutated by rejected begin: before=%+v after=%+v", foreign, current)
	}
}

func checkoutScopeDigest(values ...any) string {
	parts := make([]string, len(values))
	for i, value := range values {
		raw, _ := json.Marshal(value)
		parts[i] = strings.TrimSpace(string(raw))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
