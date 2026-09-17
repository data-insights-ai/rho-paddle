package paddle_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestCheckoutPageFrozenAcrossHostConfigurationChanges(t *testing.T) {
	f := newPaymentFixture(t, "checkout-page", false)
	calls := 0
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		var request struct {
			CustomerID     string            `json:"customer_id"`
			CollectionMode string            `json:"collection_mode"`
			Items          []map[string]any  `json:"items"`
			CustomData     map[string]string `json:"custom_data"`
			Checkout       struct {
				URL string `json:"url"`
			} `json:"checkout"`
		}
		decodeRequest(t, req, &request)
		if request.Checkout.URL != "https://billing.example.test/checkout" {
			t.Fatalf("request used changed host configuration: %s", request.Checkout.URL)
		}
		return jsonResponse(http.StatusCreated, recoveryTransactionResponse(f.binding.TransactionID, request.CustomData, f.clock, f)), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), CheckoutPageURL: "https://billing.example.test/checkout", Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	customer := paddle.CustomerReference{Account: f.account, Reference: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID}}
	lines := []paddle.CheckoutDispatchLine{{QuoteLineID: f.binding.Lines[0].Allocations[0].QuoteLineID, Price: billing.Reference{Scope: client.Scope(), ID: f.binding.Lines[0].ProviderPriceID}, Quantity: 1}}
	plan, err := first.PlanCheckout(t.Context(), f.intent.IntentInput, customer, lines)
	if err != nil {
		t.Fatal(err)
	}
	second, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), CheckoutPageURL: "https://billing.example.test/changed", Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.PlanCheckout(t.Context(), f.intent.IntentInput, customer, lines); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed page replay=%v", err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "page-worker", time.Now().Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := second.Dispatch(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	assertCheckoutCompleted(t, f, client, plan.Message.ID, f.binding.TransactionID)
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	for _, page := range []string{"https://user:password@example.test/checkout", "https://example.test/checkout?token=secret", "http://example.test/checkout", "https://example.test/checkout#token"} {
		if _, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), CheckoutPageURL: page}); !errors.Is(err, paddle.ErrInvalid) {
			t.Fatalf("unsafe page configuration accepted: %v", err)
		}
	}
}
