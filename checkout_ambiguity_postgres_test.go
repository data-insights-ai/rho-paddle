package paddle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/integration"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestCheckoutRecoveryPostgresCorrelatedMismatchRemainsUnknown(t *testing.T) {
	for _, test := range []struct {
		name     string
		currency string
		quantity int64
	}{
		{name: "currency", currency: "EUR", quantity: 1},
		{name: "quantity", currency: "USD", quantity: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPaymentFixture(t, "checkout-mismatch-"+test.name, false)
			candidateID := "txn_zyxwvutsrqponmlkjihgfedcba"
			var createCalls, listCalls int
			var customData map[string]string
			var candidateCreatedAt time.Time
			client, err := paddle.New(paddle.Config{
				Merchant:    "merchant-test",
				Environment: paddle.Sandbox,
				APIKey:      testAPIKey,
				HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					switch {
					case req.Method == http.MethodPost && req.URL.Path == "/transactions":
						createCalls++
						var body struct {
							CustomerID     string            `json:"customer_id"`
							CollectionMode string            `json:"collection_mode"`
							Items          []map[string]any  `json:"items"`
							CustomData     map[string]string `json:"custom_data"`
						}
						decodeRequest(t, req, &body)
						customData = body.CustomData
						return nil, context.DeadlineExceeded
					case req.Method == http.MethodGet && req.URL.Path == "/transactions":
						listCalls++
						candidateCreatedAt, _ = time.Parse(time.RFC3339Nano, req.URL.Query().Get("created_at[GT]"))
						candidateCreatedAt = candidateCreatedAt.Add(time.Minute)
						return jsonResponse(http.StatusOK, recoveryMismatchListResponse(candidateID, customData, candidateCreatedAt, test.currency, test.quantity, f)), nil
					default:
						return nil, fmt.Errorf("unexpected mismatch request %s %s", req.Method, req.URL.String())
					}
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			service, plan, claim := makeRecoveryClaim(t, f, client, "checkout-mismatch-worker")
			if _, err := service.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
				t.Fatalf("dispatch error=%v, want unresolved timeout", err)
			}
			if _, err := service.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "checkout-mismatch-resumer"); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
				t.Fatalf("recovery error=%v, want unresolved mismatch", err)
			}
			delivery := assertCheckoutUnknown(t, f, client, plan.Message.ID)
			if delivery.LastResult == nil || !strings.Contains(strings.ToLower(delivery.LastResult.Evidence), "conflict") {
				t.Fatalf("durable mismatch evidence=%+v, want conflict", delivery.LastResult)
			}
			assertNoPaymentEffects(t, &f)
			if createCalls != 1 || listCalls != 1 {
				t.Fatalf("provider calls create=%d list=%d, want one create and one lookup", createCalls, listCalls)
			}
		})
	}
}

func TestCheckoutRecoveryPostgresTwoExactCandidatesRemainAmbiguous(t *testing.T) {
	f := newPaymentFixture(t, "checkout-ambiguous", false)
	firstID := "txn_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	secondID := "txn_zzzzzzzzzzzzzzzzzzzzzzzzzz"
	var createCalls, listCalls int
	var customData map[string]string
	var candidateCreatedAt time.Time
	client, err := paddle.New(paddle.Config{
		Merchant:    "merchant-test",
		Environment: paddle.Sandbox,
		APIKey:      testAPIKey,
		HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodPost && req.URL.Path == "/transactions":
				createCalls++
				var body struct {
					CustomerID     string            `json:"customer_id"`
					CollectionMode string            `json:"collection_mode"`
					Items          []map[string]any  `json:"items"`
					CustomData     map[string]string `json:"custom_data"`
				}
				decodeRequest(t, req, &body)
				customData = body.CustomData
				return nil, context.DeadlineExceeded
			case req.Method == http.MethodGet && req.URL.Path == "/transactions":
				listCalls++
				if listCalls == 1 {
					candidateCreatedAt, _ = time.Parse(time.RFC3339Nano, req.URL.Query().Get("created_at[GT]"))
					candidateCreatedAt = candidateCreatedAt.Add(time.Minute)
					return jsonResponse(http.StatusOK, recoveryListResponse(firstID, true, customData, candidateCreatedAt, f)), nil
				}
				if req.URL.Query().Get("after") != firstID {
					t.Fatalf("second lookup after=%q, want %q", req.URL.Query().Get("after"), firstID)
				}
				return jsonResponse(http.StatusOK, recoveryListResponse(secondID, false, customData, candidateCreatedAt, f)), nil
			default:
				return nil, fmt.Errorf("unexpected ambiguity request %s %s", req.Method, req.URL.String())
			}
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, plan, claim := makeRecoveryClaim(t, f, client, "checkout-ambiguous-worker")
	if _, err := service.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
		t.Fatalf("dispatch error=%v, want unresolved timeout", err)
	}
	if _, err := service.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "checkout-ambiguous-resumer"); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
		t.Fatalf("recovery error=%v, want unresolved ambiguity", err)
	}
	delivery := assertCheckoutUnknown(t, f, client, plan.Message.ID)
	if delivery.LastResult == nil || !strings.Contains(delivery.LastResult.Evidence, firstID) || !strings.Contains(delivery.LastResult.Evidence, secondID) {
		t.Fatalf("ambiguous evidence=%+v, want both candidates", delivery.LastResult)
	}
	assertNoPaymentEffects(t, &f)
	if createCalls != 1 || listCalls != 2 {
		t.Fatalf("provider calls create=%d list=%d, want one create and two pages", createCalls, listCalls)
	}
}

func TestCheckoutRecoveryPostgresEmptyScanCanRetrySameWindow(t *testing.T) {
	f := newPaymentFixture(t, "checkout-empty-retry", false)
	candidateID := "txn_zyxwvutsrqponmlkjihgfedcba"
	var createCalls, listCalls int
	var customData map[string]string
	var candidateCreatedAt time.Time
	client, err := paddle.New(paddle.Config{
		Merchant:    "merchant-test",
		Environment: paddle.Sandbox,
		APIKey:      testAPIKey,
		HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodPost && req.URL.Path == "/transactions":
				createCalls++
				var body struct {
					CustomerID     string            `json:"customer_id"`
					CollectionMode string            `json:"collection_mode"`
					Items          []map[string]any  `json:"items"`
					CustomData     map[string]string `json:"custom_data"`
				}
				decodeRequest(t, req, &body)
				customData = body.CustomData
				return nil, context.DeadlineExceeded
			case req.Method == http.MethodGet && req.URL.Path == "/transactions":
				listCalls++
				candidateCreatedAt, _ = time.Parse(time.RFC3339Nano, req.URL.Query().Get("created_at[GT]"))
				candidateCreatedAt = candidateCreatedAt.Add(time.Minute)
				if listCalls == 1 {
					return jsonResponse(http.StatusOK, recoveryListResponse("", false, customData, candidateCreatedAt, f)), nil
				}
				return jsonResponse(http.StatusOK, recoveryListResponse(candidateID, false, customData, candidateCreatedAt, f)), nil
			default:
				return nil, fmt.Errorf("unexpected retry request %s %s", req.Method, req.URL.String())
			}
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, plan, claim := makeRecoveryClaim(t, f, client, "checkout-empty-retry-worker")
	if _, err := service.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
		t.Fatalf("dispatch error=%v, want unresolved timeout", err)
	}
	if _, err := service.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "checkout-empty-retry-first"); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
		t.Fatalf("first recovery error=%v, want unresolved empty scan", err)
	}
	assertCheckoutUnknown(t, f, client, plan.Message.ID)
	assertNoPaymentEffects(t, &f)
	got, err := service.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "checkout-empty-retry-second")
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != candidateID || got.CheckoutURL != "https://checkout.example.test/recovered" {
		t.Fatalf("retried transaction=%+v", got)
	}
	assertCheckoutCompleted(t, f, client, plan.Message.ID, candidateID)
	assertNoPaymentEffects(t, &f)
	if createCalls != 1 || listCalls != 2 {
		t.Fatalf("provider calls create=%d list=%d, want one create and two lookups", createCalls, listCalls)
	}
}

func makeRecoveryClaim(t *testing.T, f paymentFixture, client *paddle.Client, worker string) (*paddle.CheckoutService, paddle.CheckoutPlan, integration.Claim) {
	t.Helper()
	quoteLineID := f.binding.Lines[0].Allocations[0].QuoteLineID
	service, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanCheckout(t.Context(), f.intent.IntentInput, paddle.CustomerReference{Account: f.account, Reference: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID}}, []paddle.CheckoutDispatchLine{{QuoteLineID: quoteLineID, Price: billing.Reference{Scope: client.Scope(), ID: f.binding.Lines[0].ProviderPriceID}, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, worker, f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	return service, plan, claim
}

func recoveryMismatchListResponse(id string, customData map[string]string, createdAt time.Time, currency string, quantity int64, f paymentFixture) string {
	object := recoveryTransactionObject(id, customData, createdAt, f)
	var value map[string]any
	if err := json.Unmarshal([]byte(object), &value); err != nil {
		panic(err)
	}
	value["currency_code"] = currency
	details := value["details"].(map[string]any)
	items := details["line_items"].([]any)
	items[0].(map[string]any)["quantity"] = quantity
	value["details"] = details
	raw, _ := json.Marshal(value)
	return fmt.Sprintf(`{"data":[%s],"meta":{"pagination":{"has_more":false,"next":""}}}`, raw)
}
