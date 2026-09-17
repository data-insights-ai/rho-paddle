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
