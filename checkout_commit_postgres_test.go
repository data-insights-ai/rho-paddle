package paddle_test

import (
	"context"
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

func TestCheckoutDispatchPostgresDeferredBindingCommitRecoversByKnownID(t *testing.T) {
	f := newPaymentFixture(t, "checkout-commit", false)
	quoteLineID := f.binding.Lines[0].Allocations[0].QuoteLineID
	var createCalls, getCalls, listCalls int
	var customData map[string]string
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
				line := f.binding.Lines[0]
				return jsonResponse(http.StatusCreated, fmt.Sprintf(`{"data":{"id":%q,"customer_id":%q,"status":"ready","currency_code":"USD","origin":"api","collection_mode":"automatic","created_at":%q,"custom_data":{"billing_account_id":%q,"billing_operation_id":%q,"billing_checkout_schema":%q,"billing_intent_id":%q,"billing_quote_fingerprint":%q,"billing_request_fingerprint":%q},"details":{"line_items":[{"id":%q,"price_id":%q,"quantity":1}]},"checkout":{"url":"https://checkout.example.test/commit"}}}`, f.binding.TransactionID, f.binding.CustomerID, f.clock.Format(time.RFC3339Nano), customData["billing_account_id"], customData["billing_operation_id"], customData["billing_checkout_schema"], customData["billing_intent_id"], customData["billing_quote_fingerprint"], customData["billing_request_fingerprint"], line.ProviderLineID, line.ProviderPriceID)), nil
			case req.Method == http.MethodGet && req.URL.Path == "/transactions/"+f.binding.TransactionID:
				getCalls++
				line := f.binding.Lines[0]
				return jsonResponse(http.StatusOK, fmt.Sprintf(`{"data":{"id":%q,"customer_id":%q,"status":"ready","currency_code":"USD","origin":"api","collection_mode":"automatic","created_at":%q,"custom_data":{"billing_account_id":%q,"billing_operation_id":%q,"billing_checkout_schema":%q,"billing_intent_id":%q,"billing_quote_fingerprint":%q,"billing_request_fingerprint":%q},"details":{"line_items":[{"id":%q,"price_id":%q,"quantity":1}]},"checkout":{"url":"https://checkout.example.test/commit"}}}`, f.binding.TransactionID, f.binding.CustomerID, f.clock.Format(time.RFC3339Nano), customData["billing_account_id"], customData["billing_operation_id"], customData["billing_checkout_schema"], customData["billing_intent_id"], customData["billing_quote_fingerprint"], customData["billing_request_fingerprint"], line.ProviderLineID, line.ProviderPriceID)), nil
			case req.Method == http.MethodGet && req.URL.Path == "/transactions":
				listCalls++
				return nil, fmt.Errorf("unexpected list recovery request")
			default:
				return nil, fmt.Errorf("unexpected commit recovery request %s %s", req.Method, req.URL.String())
			}
		})},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _ = f.db.ExecContext(t.Context(), `DROP TRIGGER IF EXISTS checkout_binding_commit_failure ON billing_purchase_collection_bindings; DROP FUNCTION IF EXISTS checkout_binding_commit_failure()`)
	if _, err := f.db.ExecContext(t.Context(), `CREATE FUNCTION checkout_binding_commit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'checkout binding commit failure'; END $$; CREATE CONSTRAINT TRIGGER checkout_binding_commit_failure AFTER INSERT ON billing_purchase_collection_bindings DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION checkout_binding_commit_failure()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = f.db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS checkout_binding_commit_failure ON billing_purchase_collection_bindings; DROP FUNCTION IF EXISTS checkout_binding_commit_failure()`)
	}()
	service, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanCheckout(t.Context(), f.intent.IntentInput, paddle.CustomerReference{Account: f.account, Reference: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID}}, []paddle.CheckoutDispatchLine{{QuoteLineID: quoteLineID, Price: billing.Reference{Scope: client.Scope(), ID: f.binding.Lines[0].ProviderPriceID}, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "checkout-commit-worker", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	got, err := service.Dispatch(t.Context(), claim)
	if !errors.Is(err, paddle.ErrCheckoutUnresolved) || !strings.Contains(err.Error(), "checkout binding commit failure") {
		t.Fatalf("dispatch err=%v, want unresolved deferred commit failure", err)
	}
	if got.Reference.ID != "" || got.CheckoutURL != "" || len(got.Items) != 0 {
		t.Fatalf("dispatch returned transaction=%+v, want zero result", got)
	}
	if createCalls != 1 || getCalls != 0 || listCalls != 0 {
		t.Fatalf("provider calls create=%d get=%d list=%d, want one create only", createCalls, getCalls, listCalls)
	}
	delivery, err := f.store.Queue().Outbox(t.Context(), f.account, client.Scope(), plan.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(delivery.State) != string(integration.OutboxUnknown) || delivery.ProviderReference != f.binding.TransactionID || delivery.LastResult == nil || delivery.LastResult.ProviderReference != f.binding.TransactionID {
		t.Fatalf("delivery=%+v, want unknown with known provider ID", delivery)
	}
	assertNoPaymentEffects(t, &f)
	if _, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).CollectionBinding(t.Context(), f.account, client.Scope(), f.binding.TransactionID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("binding after failed commit err=%v, want not found", err)
	}
	intent, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).Intent(t.Context(), f.account, f.intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Command != purchase.CommandUnknown {
		t.Fatalf("intent after failed commit=%+v, want unknown", intent)
	}

	if _, err := f.db.ExecContext(t.Context(), `DROP TRIGGER checkout_binding_commit_failure ON billing_purchase_collection_bindings; DROP FUNCTION checkout_binding_commit_failure()`); err != nil {
		t.Fatal(err)
	}
	fresh, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	got, err = fresh.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "checkout-commit-resumer")
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != f.binding.TransactionID || got.CheckoutURL != "https://checkout.example.test/commit" {
		t.Fatalf("recovered transaction=%+v", got)
	}
	if createCalls != 1 || getCalls != 1 || listCalls != 0 {
		t.Fatalf("provider calls after recovery create=%d get=%d list=%d, want direct GET only", createCalls, getCalls, listCalls)
	}
	assertCheckoutCompleted(t, f, client, plan.Message.ID, f.binding.TransactionID)
	if _, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).CollectionBinding(t.Context(), f.account, client.Scope(), f.binding.TransactionID); err != nil {
		t.Fatalf("binding after recovery err=%v", err)
	}
	assertNoPaymentEffects(t, &f)
}
