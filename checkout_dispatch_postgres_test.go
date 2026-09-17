package paddle_test

import (
	"context"
	"errors"
	"fmt"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/integration"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestCheckoutDispatchPostgresBindsAndCompletesOutbox(t *testing.T) {
	f := newPaymentFixture(t, "checkout-dispatch", false)
	quoteLineID := f.binding.Lines[0].Allocations[0].QuoteLineID
	calls := 0
	var request struct {
		CustomerID     string `json:"customer_id"`
		CollectionMode string `json:"collection_mode"`
		Items          []struct {
			PriceID  string `json:"price_id"`
			Quantity int64  `json:"quantity"`
		} `json:"items"`
		CustomData map[string]string `json:"custom_data"`
	}
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		decodeRequest(t, req, &request)
		line := f.binding.Lines[0]
		return jsonResponse(http.StatusCreated, fmt.Sprintf(`{"data":{"id":%q,"customer_id":%q,"status":"ready","currency_code":"USD","origin":"api","collection_mode":"automatic","created_at":%q,"custom_data":{"billing_account_id":%q,"billing_operation_id":%q,"billing_checkout_schema":%q,"billing_intent_id":%q,"billing_quote_fingerprint":%q,"billing_request_fingerprint":%q},"details":{"line_items":[{"id":%q,"price_id":%q,"quantity":1}]},"checkout":{"url":"https://checkout.example.test/session"}}}`, f.binding.TransactionID, f.binding.CustomerID, f.clock.Format("2006-01-02T15:04:05Z"), request.CustomData["billing_account_id"], request.CustomData["billing_operation_id"], request.CustomData["billing_checkout_schema"], request.CustomData["billing_intent_id"], request.CustomData["billing_quote_fingerprint"], request.CustomData["billing_request_fingerprint"], line.ProviderLineID, line.ProviderPriceID)), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanCheckout(t.Context(), f.intent.IntentInput, paddle.CustomerReference{Account: f.account, Reference: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID}}, []paddle.CheckoutDispatchLine{{QuoteLineID: quoteLineID, Price: billing.Reference{Scope: client.Scope(), ID: f.binding.Lines[0].ProviderPriceID}, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "checkout-worker", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	got, err := service.Dispatch(t.Context(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != f.binding.TransactionID || got.CheckoutURL != "https://checkout.example.test/session" {
		t.Fatalf("transaction=%+v", got)
	}
	if _, err := service.Dispatch(t.Context(), claim); err == nil || calls != 1 {
		t.Fatalf("replayed dispatch err=%v calls=%d, want no second provider send", err, calls)
	}
	delivery, err := f.store.Queue().Outbox(t.Context(), f.account, client.Scope(), plan.Message.ID)
	if err != nil || string(delivery.State) != string(integration.OutboxCompleted) {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
}

func TestCheckoutDispatchPostgresTimeoutPersistsUnknownWithoutResend(t *testing.T) {
	f := newPaymentFixture(t, "checkout-timeout", false)
	quoteLineID := f.binding.Lines[0].Allocations[0].QuoteLineID
	var calls int
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, context.DeadlineExceeded })}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanCheckout(t.Context(), f.intent.IntentInput, paddle.CustomerReference{Account: f.account, Reference: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID}}, []paddle.CheckoutDispatchLine{{QuoteLineID: quoteLineID, Price: billing.Reference{Scope: client.Scope(), ID: f.binding.Lines[0].ProviderPriceID}, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "checkout-timeout-worker", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := service.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
		t.Fatalf("dispatch error=%v", err)
	}
	delivery, err := f.store.Queue().Outbox(t.Context(), f.account, client.Scope(), plan.Message.ID)
	if err != nil || string(delivery.State) != string(integration.OutboxUnknown) || calls != 1 {
		t.Fatalf("delivery=%+v calls=%d err=%v", delivery, calls, err)
	}
}
