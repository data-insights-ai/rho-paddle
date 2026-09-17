package paddle_test

import (
	"encoding/json"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestTransactionProcessorMergedProviderLinePreservesQuoteFunding(t *testing.T) {
	for _, treatment := range []purchase.TaxTreatment{purchase.TaxInclusive, purchase.TaxExclusive} {
		t.Run(string(treatment), func(t *testing.T) {
			f := newPaymentFixture(t, "merged-"+string(treatment), false)
			service := purchase.New(f.store.Purchases(), func() time.Time { return f.clock })
			original, err := service.Quote(t.Context(), f.account, f.intent.QuoteID)
			if err != nil {
				t.Fatal(err)
			}
			price, err := service.PublishPrice(t.Context(), purchase.Price{Account: f.account, Revision: purchase.Revision{ID: "merged-price", Version: 1}, Offer: original.Lines[0].Offer.Revision, Currency: "USD", UnitAmount: 125, TaxTreatment: treatment})
			if err != nil {
				t.Fatal(err)
			}
			quote, err := service.CreateQuote(t.Context(), purchase.QuoteInput{Account: f.account, ID: "merged-quote", ValidUntil: f.clock.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "a", Price: price.Revision, Quantity: 1}, {ID: "b", Price: price.Revision, Quantity: 2}}})
			if err != nil {
				t.Fatal(err)
			}
			f.intent, err = service.CreateIntent(t.Context(), purchase.IntentInput{Account: f.account, ID: "merged-intent", Operation: "merged-operation", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: f.intent.Scope, Actor: "host", Reason: "merged-provider-price", ExpiresAt: quote.ValidUntil})
			if err != nil {
				t.Fatal(err)
			}
			f.binding.IntentID, f.binding.QuoteFingerprint = f.intent.ID, quote.Fingerprint()
			f.binding.Lines = []purchase.CollectionLine{{ProviderLineID: "txnitm_abcdefghijklmnopqrstuvwxyz", ProviderPriceID: "pri_abcdefghijklmnopqrstuvwxyz", Quantity: 3, Allocations: []purchase.CollectionAllocation{{QuoteLineID: "b", Quantity: 2}, {QuoteLineID: "a", Quantity: 1}}}}
			dispatchMergedCheckout(t, f)
			gross := "375"
			if treatment == purchase.TaxExclusive {
				gross = "406"
			}
			body := workflowPaymentBody(workflowPayment{EventID: "evt_00000000000000000000000009", EventType: "transaction.completed", OccurredAt: webhookNow, Status: "completed", AccountCustomer: f.binding.CustomerID, Total: gross, Tax: "31", Lines: []workflowLine{{ID: f.binding.Lines[0].ProviderLineID, PriceID: f.binding.Lines[0].ProviderPriceID, Quantity: 3, Total: gross, Tax: "31"}}, Attempts: []workflowAttempt{{ID: "99999999-9999-4999-8999-999999999999", Status: "captured", Amount: gross, CreatedAt: f.clock, CapturedAt: f.clock.Add(time.Second)}}})
			processWorkflowBody(t, &f, body, "merged-worker")
			funding, err := service.Funding(t.Context(), f.account, f.intent.Scope, f.binding.TransactionID)
			if err != nil || len(funding.Lines) != 2 {
				t.Fatalf("funding=%+v err=%v", funding, err)
			}
			if funding.Lines[0].LineID != "a" || funding.Lines[0].Tax != 10 || funding.Lines[1].LineID != "b" || funding.Lines[1].Tax != 21 {
				t.Fatalf("funding allocation=%+v", funding.Lines)
			}
			balance, err := credit.New(f.store.Credits(), func() time.Time { return webhookNow.Add(time.Minute) }).Balance(t.Context(), f.account, "credits", "")
			if err != nil || balance.Available != 300 {
				t.Fatalf("credits=%+v err=%v", balance, err)
			}
		})
	}
}

// Exercise the whole handoff: grouped provider creation, committed binding,
// and then the signed payment processed by the caller above.
func dispatchMergedCheckout(t *testing.T, f paymentFixture) {
	t.Helper()
	calls := 0
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		var request struct {
			CustomerID     string `json:"customer_id"`
			CollectionMode string `json:"collection_mode"`
			Items          []struct {
				PriceID  string `json:"price_id"`
				Quantity int64  `json:"quantity"`
			} `json:"items"`
			CustomData map[string]string `json:"custom_data"`
		}
		decodeRequest(t, req, &request)
		if req.Method != http.MethodPost || len(request.Items) != 1 || request.Items[0].Quantity != 3 || request.Items[0].PriceID != f.binding.Lines[0].ProviderPriceID {
			t.Fatalf("grouped request=%+v", request)
		}
		data := map[string]any{"id": f.binding.TransactionID, "customer_id": f.binding.CustomerID, "status": "ready", "currency_code": "USD", "origin": "api", "collection_mode": "automatic", "created_at": f.clock.Format(time.RFC3339Nano), "custom_data": request.CustomData, "details": map[string]any{"line_items": []map[string]any{{"id": f.binding.Lines[0].ProviderLineID, "price_id": f.binding.Lines[0].ProviderPriceID, "quantity": 3}}}, "checkout": map[string]string{"url": "https://checkout.example.test/grouped"}}
		raw, err := json.Marshal(map[string]any{"data": data})
		if err != nil {
			t.Fatal(err)
		}
		return jsonResponse(http.StatusCreated, string(raw)), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	price := billing.Reference{Scope: client.Scope(), ID: f.binding.Lines[0].ProviderPriceID}
	plan, err := checkout.PlanCheckout(t.Context(), f.intent.IntentInput, paddle.CustomerReference{Account: f.account, Reference: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID}}, []paddle.CheckoutDispatchLine{{QuoteLineID: "b", Price: price, Quantity: 2}, {QuoteLineID: "a", Price: price, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "merged-checkout", time.Now().Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	tx, err := checkout.Dispatch(t.Context(), claim)
	if err != nil || tx.CheckoutURL == "" {
		t.Fatalf("checkout=%+v err=%v", tx, err)
	}
	assertCheckoutCompleted(t, f, client, plan.Message.ID, f.binding.TransactionID)
	assertNoPaymentEffects(t, &f)
	binding, err := purchase.New(f.store.Purchases(), nil).CollectionBinding(t.Context(), f.account, client.Scope(), tx.Reference.ID)
	if err != nil || len(binding.Lines) != 1 || len(binding.Lines[0].Allocations) != 2 || calls != 1 {
		t.Fatalf("binding=%+v calls=%d err=%v", binding, calls, err)
	}
}
