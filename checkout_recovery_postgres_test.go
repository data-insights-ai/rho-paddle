package paddle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestCheckoutRecoveryPostgresResolvesLostCreateFromCorrelatedList(t *testing.T) {
	f := newPaymentFixture(t, "checkout-recovery", false)
	quoteLineID := f.binding.Lines[0].Allocations[0].QuoteLineID
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
				body := recoveryListResponse(candidateID, false, customData, candidateCreatedAt, f)
				return jsonResponse(http.StatusOK, body), nil
			default:
				return nil, fmt.Errorf("unexpected recovery request %s %s", req.Method, req.URL.String())
			}
		})},
	})
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
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "checkout-recovery-worker", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := service.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
		t.Fatalf("dispatch error=%v, want unresolved timeout", err)
	}
	assertCheckoutUnknown(t, f, client, plan.Message.ID)
	assertNoPaymentEffects(t, &f)
	if _, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).CollectionBinding(t.Context(), f.account, client.Scope(), candidateID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("binding before recovery err=%v, want not found", err)
	}

	got, err := service.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "checkout-recovery-resumer")
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != candidateID || got.CheckoutURL != "https://checkout.example.test/recovered" {
		t.Fatalf("recovered transaction=%+v", got)
	}
	if createCalls != 1 || listCalls != 1 {
		t.Fatalf("provider calls create=%d list=%d, want 1 create and 1 list", createCalls, listCalls)
	}
	assertCheckoutCompleted(t, f, client, plan.Message.ID, candidateID)
	binding, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).CollectionBinding(t.Context(), f.account, client.Scope(), candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.IntentID != f.intent.ID || binding.CustomerID != f.binding.CustomerID || len(binding.Lines) != 1 || binding.Lines[0].Allocations[0].QuoteLineID != quoteLineID {
		t.Fatalf("binding=%+v", binding)
	}
	intent, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).Intent(t.Context(), f.account, f.intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Command != purchase.CommandReconciled {
		t.Fatalf("intent after recovery=%+v, want reconciled candidate", intent)
	}
	assertNoPaymentEffects(t, &f)
}

func TestCheckoutRecoveryPostgresResumesCorrelatedCandidateAcrossPages(t *testing.T) {
	f := newPaymentFixture(t, "checkout-recovery-pages", false)
	quoteLineID := f.binding.Lines[0].Allocations[0].QuoteLineID
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
				switch listCalls {
				case 1:
					candidateCreatedAt, _ = time.Parse(time.RFC3339Nano, req.URL.Query().Get("created_at[GT]"))
					candidateCreatedAt = candidateCreatedAt.Add(time.Minute)
					return jsonResponse(http.StatusOK, recoveryListResponse(candidateID, true, customData, candidateCreatedAt, f)), nil
				case 2:
					return nil, context.DeadlineExceeded
				default:
					if req.URL.Query().Get("after") != candidateID {
						t.Fatalf("resumed lookup after=%q, want %q", req.URL.Query().Get("after"), candidateID)
					}
					return jsonResponse(http.StatusOK, recoveryListResponse("", false, customData, candidateCreatedAt, f)), nil
				}
			case req.Method == http.MethodGet && req.URL.Path == "/transactions/"+candidateID:
				return jsonResponse(http.StatusOK, recoveryTransactionResponse(candidateID, customData, candidateCreatedAt, f)), nil
			default:
				return nil, fmt.Errorf("unexpected recovery request %s %s", req.Method, req.URL.String())
			}
		})},
	})
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
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "checkout-recovery-pages-worker", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := service.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
		t.Fatalf("dispatch error=%v, want unresolved timeout", err)
	}
	assertCheckoutUnknown(t, f, client, plan.Message.ID)

	if _, err := service.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "checkout-recovery-pages-worker"); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
		t.Fatalf("first recovery error=%v, want unresolved second-page failure", err)
	}
	checkpointed := assertCheckoutUnknown(t, f, client, plan.Message.ID)
	if checkpointed.LastResult == nil || !strings.Contains(checkpointed.LastResult.Evidence, candidateID) {
		t.Fatalf("checkpoint=%+v, want retained first-page candidate", checkpointed.LastResult)
	}

	fresh, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fresh.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "checkout-recovery-pages-fresh")
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != candidateID || got.CheckoutURL != "https://checkout.example.test/recovered" {
		t.Fatalf("resumed transaction=%+v", got)
	}
	if createCalls != 1 || listCalls < 3 {
		t.Fatalf("provider calls create=%d list=%d, want one create and resumed pages", createCalls, listCalls)
	}
	assertCheckoutCompleted(t, f, client, plan.Message.ID, candidateID)
	if _, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).CollectionBinding(t.Context(), f.account, client.Scope(), candidateID); err != nil {
		t.Fatalf("binding after resumed recovery err=%v", err)
	}
	intent, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).Intent(t.Context(), f.account, f.intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Command != purchase.CommandReconciled {
		t.Fatalf("intent after resumed recovery=%+v", intent)
	}
	assertNoPaymentEffects(t, &f)
}

func recoveryTransactionResponse(id string, customData map[string]string, createdAt time.Time, f paymentFixture) string {
	line := f.binding.Lines[0]
	return fmt.Sprintf(`{"data":{"id":%q,"customer_id":%q,"status":"ready","currency_code":"USD","origin":"api","collection_mode":"automatic","created_at":%q,"custom_data":%s,"details":{"line_items":[{"id":%q,"price_id":%q,"quantity":1}]},"checkout":{"url":"https://checkout.example.test/recovered"}}}`, id, f.binding.CustomerID, createdAt.Format(time.RFC3339Nano), quotedCustomData(customData), line.ProviderLineID, line.ProviderPriceID)
}

func recoveryListResponse(id string, hasMore bool, customData map[string]string, createdAt time.Time, f paymentFixture) string {
	data := "[]"
	next := ""
	if id != "" {
		data = "[" + recoveryTransactionObject(id, customData, createdAt, f) + "]"
		next = "https://sandbox-api.paddle.com/transactions?after=" + id
	}
	return fmt.Sprintf(`{"data":%s,"meta":{"pagination":{"has_more":%t,"next":%q}}}`, data, hasMore, next)
}

func recoveryTransactionObject(id string, customData map[string]string, createdAt time.Time, f paymentFixture) string {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(recoveryTransactionResponse(id, customData, createdAt, f)), &envelope); err != nil {
		panic(err)
	}
	return string(envelope.Data)
}

func quotedCustomData(values map[string]string) string {
	if values == nil {
		return "{}"
	}
	raw, _ := json.Marshal(values)
	return string(raw)
}

func assertCheckoutUnknown(t *testing.T, f paymentFixture, client *paddle.Client, messageID string) integration.Delivery {
	t.Helper()
	delivery, err := f.store.Queue().Outbox(t.Context(), f.account, client.Scope(), messageID)
	if err != nil {
		t.Fatal(err)
	}
	if string(delivery.State) != string(integration.OutboxUnknown) || delivery.ProviderReference != "" {
		t.Fatalf("delivery=%+v, want unknown without provider reference", delivery)
	}
	return delivery
}

func assertCheckoutCompleted(t *testing.T, f paymentFixture, client *paddle.Client, messageID, candidateID string) {
	t.Helper()
	delivery, err := f.store.Queue().Outbox(t.Context(), f.account, client.Scope(), messageID)
	if err != nil {
		t.Fatal(err)
	}
	if string(delivery.State) != string(integration.OutboxCompleted) || delivery.ProviderReference != candidateID {
		t.Fatalf("delivery=%+v, want completed candidate %q", delivery, candidateID)
	}
}
