package paddle_test

import (
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"testing"

	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestSubscriptionGetWireMapping(t *testing.T) {
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodGet, "https://sandbox-api.paddle.com/subscriptions/"+testSubID)
		return jsonResponse(http.StatusOK, `{"data":{"id":"`+testSubID+`","status":"active","customer_id":"`+testCustomerID+`","collection_mode":"manual"}}`), nil
	})
	got, err := client.Subscription(t.Context(), billing.Reference{Scope: client.Scope(), ID: testSubID})
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != testSubID || got.Customer.ID != testCustomerID || got.Status != "active" || got.CollectionMode != "manual" {
		t.Fatalf("subscription=%#v", got)
	}
}

func TestUpdateSubscriptionProrationWireMapping(t *testing.T) {
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodPatch, "https://sandbox-api.paddle.com/subscriptions/"+testSubID)
		var body struct {
			Proration string `json:"proration_billing_mode"`
			Items     []struct {
				PriceID  string `json:"price_id"`
				Quantity int64  `json:"quantity"`
			} `json:"items"`
		}
		decodeRequest(t, req, &body)
		if body.Proration != "prorated_immediately" || len(body.Items) != 1 || body.Items[0].Quantity != 2 || body.Items[0].PriceID != testPriceID {
			t.Fatalf("body=%#v", body)
		}
		return jsonResponse(http.StatusOK, `{"data":{"id":"`+testSubID+`","status":"active","customer_id":"`+testCustomerID+`","collection_mode":"automatic","items":[{"quantity":2,"price":{"id":"`+testPriceID+`"}}]}}`), nil
	})
	got, err := client.UpdateSubscription(t.Context(), paddle.SubscriptionUpdate{
		Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		Proration:    "prorated_immediately",
		Items:        []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.CollectionMode != "automatic" || len(got.Items) != 1 || got.Items[0].Quantity != 2 {
		t.Fatalf("subscription=%#v", got)
	}
}

func TestPreviewSubscriptionUpdateWireMapping(t *testing.T) {
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodPatch, "https://sandbox-api.paddle.com/subscriptions/"+testSubID+"/preview")
		var body struct {
			Proration string `json:"proration_billing_mode"`
			Items     []struct {
				PriceID  string `json:"price_id"`
				Quantity int64  `json:"quantity"`
			} `json:"items"`
		}
		decodeRequest(t, req, &body)
		if body.Proration != "prorated_immediately" || len(body.Items) != 1 || body.Items[0].PriceID != testPriceID {
			t.Fatalf("body=%#v", body)
		}
		return jsonResponse(http.StatusOK, `{"data":{"currency_code":"EUR","next_billed_at":"2026-10-19T07:07:20Z","update_summary":{"result":{"action":"charge","amount":"12345","currency_code":"EUR"},"charge":{"amount":"20000","currency_code":"EUR"},"credit":{"amount":"7655","currency_code":"EUR"}},"recurring_transaction_details":{"totals":{"total":"49900"}}}}`), nil
	})
	got, err := client.PreviewSubscriptionUpdate(t.Context(), paddle.SubscriptionUpdate{
		Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		Proration:    "prorated_immediately",
		Items:        []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "charge" || got.Amount != 12345 || got.Currency != "EUR" || got.Charge != 20000 || got.Credit != 7655 || got.RecurringAmount != 49900 || got.NextBilledAt.IsZero() {
		t.Fatalf("preview=%#v", got)
	}
	// Nothing due now: the summary is absent.
	client = newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"currency_code":"EUR","next_billed_at":"2026-10-19T07:07:20Z"}}`), nil
	})
	got, err = client.PreviewSubscriptionUpdate(t.Context(), paddle.SubscriptionUpdate{
		Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID}, Proration: "prorated_next_billing_period",
		Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}},
	})
	if err != nil || got.Action != "" || got.Amount != 0 {
		t.Fatalf("preview without summary=%#v err=%v", got, err)
	}
	// Invalid input never reaches the provider.
	if _, err := client.PreviewSubscriptionUpdate(t.Context(), paddle.SubscriptionUpdate{Proration: "prorated_immediately"}); err == nil {
		t.Fatal("empty update previewed")
	}
	// A malformed amount is a response error, not a zero.
	client = newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"update_summary":{"result":{"action":"refund","amount":"x"}}}}`), nil
	})
	if _, err := client.PreviewSubscriptionUpdate(t.Context(), paddle.SubscriptionUpdate{
		Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID}, Proration: "prorated_immediately",
		Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}},
	}); err == nil {
		t.Fatal("unknown action accepted")
	}
}
