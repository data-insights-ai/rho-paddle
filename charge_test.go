package paddle_test

import (
	"context"
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"sync/atomic"
	"testing"

	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestChargeAndCollectionCapabilitiesFailClosedBeforeMutation(t *testing.T) {
	var calls atomic.Int32
	client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("provider must not be contacted")
	})
	if err := billing.RequireSupported(paddle.CollectionModeCapability(paddle.CollectionAutomatic)); err != nil {
		t.Fatal(err)
	}
	if err := billing.RequireSupported(paddle.ChargeTimingCapability(paddle.ChargeImmediately)); err != nil {
		t.Fatal(err)
	}
	if err := billing.RequireSupported(paddle.ChargeTimingCapability(paddle.ChargeNextPeriod)); err != nil {
		t.Fatal(err)
	}
	manual, ok := errors.AsType[*billing.CapabilityError](billing.RequireSupported(paddle.CollectionModeCapability(paddle.CollectionManual)))
	if !ok || manual.Support != billing.SupportUnsupported || manual.Reason != "manual_invoice_collection" {
		t.Fatalf("manual capability=%v", manual)
	}
	timing, ok := errors.AsType[*billing.CapabilityError](billing.RequireSupported(paddle.ChargeTimingCapability("whenever")))
	if !ok || timing.Support != billing.SupportUnsupported || timing.Reason != "charge_timing_not_supported" {
		t.Fatalf("timing capability=%v", timing)
	}
	_, err := client.ChargeSubscription(t.Context(), paddle.ChargeInput{
		Account: testAccount, Operation: testOperation, Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		When: paddle.ChargeImmediately, CollectionMode: paddle.CollectionManual,
		Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}},
	})
	if !errors.Is(err, manual) && capabilityReason(err) != "manual_invoice_collection" {
		t.Fatalf("manual charge error=%v", err)
	}
	_, err = client.ChargeSubscription(t.Context(), paddle.ChargeInput{
		Account: testAccount, Operation: testOperation, Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		When: "later", CollectionMode: paddle.CollectionAutomatic,
		Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}},
	})
	if capabilityReason(err) != "charge_timing_not_supported" {
		t.Fatalf("invalid timing error=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("unsupported charge issued %d provider calls", calls.Load())
	}
}

func TestChargeSubscriptionImmediateAndNextPeriodWireMapping(t *testing.T) {
	for _, when := range []paddle.ChargeWhen{paddle.ChargeImmediately, paddle.ChargeNextPeriod} {
		t.Run(string(when), func(t *testing.T) {
			var posts int
			client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					assertRequest(t, req, http.MethodGet, "https://sandbox-api.paddle.com/subscriptions/"+testSubID)
					return jsonResponse(http.StatusOK, `{"data":{"id":"`+testSubID+`","status":"active","customer_id":"`+testCustomerID+`","collection_mode":"automatic"}}`), nil
				}
				posts++
				assertRequest(t, req, http.MethodPost, "https://sandbox-api.paddle.com/subscriptions/"+testSubID+"/charge")
				var body struct {
					EffectiveFrom string `json:"effective_from"`
					Items         []struct {
						PriceID  string `json:"price_id"`
						Quantity int64  `json:"quantity"`
					} `json:"items"`
				}
				decodeRequest(t, req, &body)
				if body.EffectiveFrom != string(when) || len(body.Items) != 1 || body.Items[0].PriceID != testPriceID || body.Items[0].Quantity != 2 {
					t.Fatalf("charge body=%#v", body)
				}
				return jsonResponse(http.StatusCreated, `{"data":{"id":"`+testSubID+`","customer_id":"`+testCustomerID+`","status":"active","collection_mode":"automatic"}}`), nil
			})
			got, err := client.ChargeSubscription(t.Context(), paddle.ChargeInput{
				Account: testAccount, Operation: testOperation, Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
				When: when, CollectionMode: paddle.CollectionAutomatic, Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 2}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Subscription.ID != testSubID || got.Customer.ID != testCustomerID || got.Status != "active" || got.CollectionMode != "automatic" {
				t.Fatalf("result=%#v", got)
			}
			if posts != 1 {
				t.Fatalf("charge posts=%d", posts)
			}
		})
	}
}

func TestChargeSubscriptionManualSubscriptionFailsClosedBeforeMutation(t *testing.T) {
	var posts int
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			posts++
			return nil, errors.New("provider must not be charged")
		}
		assertRequest(t, req, http.MethodGet, "https://sandbox-api.paddle.com/subscriptions/"+testSubID)
		return jsonResponse(http.StatusOK, `{"data":{"id":"`+testSubID+`","status":"active","customer_id":"`+testCustomerID+`","collection_mode":"manual"}}`), nil
	})
	_, err := client.ChargeSubscription(t.Context(), paddle.ChargeInput{
		Account: testAccount, Operation: testOperation, Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		When: paddle.ChargeImmediately, CollectionMode: paddle.CollectionAutomatic,
		Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}},
	})
	if capabilityReason(err) != "manual_invoice_collection" {
		t.Fatalf("manual subscription error=%v", err)
	}
	if posts != 0 {
		t.Fatalf("manual subscription issued %d charge posts", posts)
	}
}

func TestChargeSubscriptionManualResponseIsUncertain(t *testing.T) {
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return jsonResponse(http.StatusOK, `{"data":{"id":"`+testSubID+`","status":"active","customer_id":"`+testCustomerID+`","collection_mode":"automatic"}}`), nil
		}
		return jsonResponse(http.StatusCreated, `{"data":{"id":"`+testSubID+`","customer_id":"`+testCustomerID+`","status":"active","collection_mode":"manual"}}`), nil
	})
	_, err := client.ChargeSubscription(t.Context(), paddle.ChargeInput{
		Account: testAccount, Operation: testOperation, Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		When: paddle.ChargeImmediately, CollectionMode: paddle.CollectionAutomatic,
		Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}},
	})
	if !errors.Is(err, paddle.ErrUncertain) || capabilityReason(err) != "manual_invoice_collection" {
		t.Fatalf("manual charge response error=%v", err)
	}
}

func TestChargeSubscriptionTimeoutAfterSubmitIsUncertain(t *testing.T) {
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return jsonResponse(http.StatusOK, `{"data":{"id":"`+testSubID+`","status":"active","customer_id":"`+testCustomerID+`","collection_mode":"automatic"}}`), nil
		}
		return nil, context.DeadlineExceeded
	})
	_, err := client.ChargeSubscription(t.Context(), paddle.ChargeInput{
		Account: testAccount, Operation: testOperation, Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		When: paddle.ChargeImmediately, CollectionMode: paddle.CollectionAutomatic,
		Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}},
	})
	if !errors.Is(err, paddle.ErrUncertain) {
		t.Fatalf("timeout error=%v, want uncertain", err)
	}
}

func capabilityReason(err error) string {
	capability, ok := errors.AsType[*billing.CapabilityError](err)
	if !ok {
		return ""
	}
	return capability.Reason
}
