package paddle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	billing "github.com/data-insights-ai/rho-billing"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

// This test performs real, unpaid sandbox API mutations only when explicitly
// enabled. Credentials are read from a private host file, never from fixtures.
func TestSandboxCheckoutLostResponseRecovery(t *testing.T) {
	if os.Getenv("PADDLE_SANDBOX_TEST") != "1" {
		t.Skip("set PADDLE_SANDBOX_TEST=1 and PADDLE_SANDBOX_KEY_FILE for real sandbox evidence")
	}
	keyBytes, err := os.ReadFile(os.Getenv("PADDLE_SANDBOX_KEY_FILE"))
	if err != nil {
		t.Fatal("sandbox key file is unavailable")
	}
	key := strings.TrimSpace(string(keyBytes))
	if !strings.HasPrefix(key, "pdl_sdbx_apikey_") {
		t.Fatal("expected a sandbox credential")
	}
	f := newPaymentFixture(t, "real-sandbox-checkout", false)
	run := fmt.Sprintf("rho-billing-%d", time.Now().UnixNano())
	setup := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	product := sandboxEntity(t, setup, key, http.MethodPost, "/products", map[string]any{"name": run, "tax_category": "saas", "custom_data": map[string]string{"test_run": run}})
	defer sandboxEntity(t, setup, key, http.MethodPatch, "/products/"+product, map[string]string{"status": "archived"})
	price := sandboxEntity(t, setup, key, http.MethodPost, "/prices", map[string]any{"product_id": product, "description": run, "unit_price": map[string]string{"amount": "125", "currency_code": "USD"}, "tax_mode": "internal"})
	defer sandboxEntity(t, setup, key, http.MethodPatch, "/prices/"+price, map[string]string{"status": "archived"})
	var posts, lost atomic.Int64
	transport := clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(req)
		if req.Method == http.MethodPost && req.URL.Path == "/transactions" {
			posts.Add(1)
			if err == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<20))
				_ = response.Body.Close()
				lost.Add(1)
				return nil, context.DeadlineExceeded
			}
		}
		return response, err
	})
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: key, HTTPClient: &http.Client{Transport: transport, Timeout: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	customer, err := client.CreateCustomer(t.Context(), paddle.CustomerInput{Account: f.account, Operation: run, Email: run + "@example.com", Name: "Rho Billing Sandbox Test"})
	if err != nil {
		t.Fatal(err)
	}
	defer sandboxEntity(t, setup, key, http.MethodPatch, "/customers/"+customer.Reference.ID, map[string]string{"status": "archived"})
	purchases := purchase.New(f.store.Purchases(), func() time.Time { return f.clock })
	oldQuote, err := purchases.Quote(t.Context(), f.account, f.intent.QuoteID)
	if err != nil {
		t.Fatal(err)
	}
	localPrice, err := purchases.PublishPrice(t.Context(), purchase.Price{Account: f.account, Revision: purchase.Revision{ID: "sandbox-price", Version: 1}, Offer: oldQuote.Lines[0].Offer.Revision, Currency: "USD", UnitAmount: 125, TaxTreatment: purchase.TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := purchases.CreateQuote(t.Context(), purchase.QuoteInput{Account: f.account, ID: "sandbox-quote", ValidUntil: f.clock.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "a", Price: localPrice.Revision, Quantity: 1}, {ID: "b", Price: localPrice.Revision, Quantity: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	f.intent, err = purchases.CreateIntent(t.Context(), purchase.IntentInput{Account: f.account, ID: "sandbox-intent", Operation: run, QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: client.Scope(), Actor: "sandbox-test", Reason: "lost-response", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{CheckoutPageURL: os.Getenv("PADDLE_SANDBOX_CHECKOUT_PAGE_URL"), Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	ref := billing.Reference{Scope: client.Scope(), ID: price}
	plan, err := checkout.PlanCheckout(t.Context(), f.intent.IntentInput, customer, []paddle.CheckoutDispatchLine{{QuoteLineID: "a", Price: ref, Quantity: 1}, {QuoteLineID: "b", Price: ref, Quantity: 2}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), plan.Message.Direction, "sandbox-worker", time.Now().Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if tx, err := checkout.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrCheckoutUnresolved) || tx.Reference.ID != "" {
		t.Fatalf("expected discarded response, got id=%q error=%v", tx.Reference.ID, err)
	}
	if lost.Load() != 1 {
		t.Fatal("provider did not accept exactly one intentionally discarded create")
	}
	assertCheckoutUnknown(t, f, client, plan.Message.ID)
	tx, err := checkout.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "sandbox-resumer")
	if err != nil {
		t.Fatal(err)
	}
	assertCheckoutCompleted(t, f, client, plan.Message.ID, tx.Reference.ID)
	if posts.Load() != 1 || len(tx.Items) != 1 || tx.Items[0].Quantity != 3 {
		t.Fatalf("posts=%d item_count=%d", posts.Load(), len(tx.Items))
	}
	binding, err := purchases.CollectionBinding(t.Context(), f.account, client.Scope(), tx.Reference.ID)
	if err != nil || len(binding.Lines) != 1 || len(binding.Lines[0].Allocations) != 2 {
		t.Fatalf("grouped binding count=%d err=%v", len(binding.Lines), err)
	}
	assertNoPaymentEffects(t, &f)
	t.Logf("real sandbox: one lost create recovered; transaction=%s status=%s grouped_quantity=3; no payment or funding", tx.Reference.ID, tx.Status)
}

func TestSandboxCheckoutAPIAcceptanceIsNotCapturedPayment(t *testing.T) {
	if os.Getenv("PADDLE_SANDBOX_TEST") != "1" {
		t.Skip("set PADDLE_SANDBOX_TEST=1 and PADDLE_SANDBOX_KEY_FILE for real sandbox evidence")
	}
	keyBytes, err := os.ReadFile(os.Getenv("PADDLE_SANDBOX_KEY_FILE"))
	if err != nil {
		t.Fatal("sandbox key file is unavailable")
	}
	key := strings.TrimSpace(string(keyBytes))
	if !strings.HasPrefix(key, "pdl_sdbx_apikey_") {
		t.Fatal("expected a sandbox credential")
	}
	f := newPaymentFixture(t, "real-sandbox-unpaid", false)
	run := fmt.Sprintf("rho-billing-unpaid-%d", time.Now().UnixNano())
	setup := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	product := sandboxEntity(t, setup, key, http.MethodPost, "/products", map[string]any{"name": run, "tax_category": "saas", "custom_data": map[string]string{"test_run": run}})
	defer sandboxEntity(t, setup, key, http.MethodPatch, "/products/"+product, map[string]string{"status": "archived"})
	price := sandboxEntity(t, setup, key, http.MethodPost, "/prices", map[string]any{"product_id": product, "description": run, "unit_price": map[string]string{"amount": "125", "currency_code": "USD"}, "tax_mode": "internal"})
	defer sandboxEntity(t, setup, key, http.MethodPatch, "/prices/"+price, map[string]string{"status": "archived"})
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: key, HTTPClient: &http.Client{Timeout: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	customer, err := client.CreateCustomer(t.Context(), paddle.CustomerInput{Account: f.account, Operation: run, Email: run + "@example.com", Name: "Rho Billing Sandbox Unpaid"})
	if err != nil {
		t.Fatal(err)
	}
	defer sandboxEntity(t, setup, key, http.MethodPatch, "/customers/"+customer.Reference.ID, map[string]string{"status": "archived"})
	purchases := purchase.New(f.store.Purchases(), func() time.Time { return f.clock })
	oldQuote, err := purchases.Quote(t.Context(), f.account, f.intent.QuoteID)
	if err != nil {
		t.Fatal(err)
	}
	localPrice, err := purchases.PublishPrice(t.Context(), purchase.Price{Account: f.account, Revision: purchase.Revision{ID: "sandbox-unpaid-price", Version: 1}, Offer: oldQuote.Lines[0].Offer.Revision, Currency: "USD", UnitAmount: 125, TaxTreatment: purchase.TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := purchases.CreateQuote(t.Context(), purchase.QuoteInput{Account: f.account, ID: "sandbox-unpaid-quote", ValidUntil: f.clock.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "a", Price: localPrice.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	f.intent, err = purchases.CreateIntent(t.Context(), purchase.IntentInput{Account: f.account, ID: "sandbox-unpaid-intent", Operation: run, QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: client.Scope(), Actor: "sandbox-test", Reason: "unpaid-acceptance", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{CheckoutPageURL: os.Getenv("PADDLE_SANDBOX_CHECKOUT_PAGE_URL"), Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	ref := billing.Reference{Scope: client.Scope(), ID: price}
	plan, err := checkout.PlanCheckout(t.Context(), f.intent.IntentInput, customer, []paddle.CheckoutDispatchLine{{QuoteLineID: "a", Price: ref, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), plan.Message.Direction, "sandbox-unpaid-worker", time.Now().Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	tx, err := checkout.Dispatch(t.Context(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status != "draft" && tx.Status != "ready" {
		t.Fatalf("status=%s, want draft or ready", tx.Status)
	}
	got, err := client.Transaction(t.Context(), tx.Reference)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "paid" || got.Status == "completed" {
		t.Fatalf("API create captured payment status=%s", got.Status)
	}
	assertCheckoutCompleted(t, f, client, plan.Message.ID, tx.Reference.ID)
	assertNoPaymentEffects(t, &f)
	t.Logf("real sandbox: API acceptance without capture; transaction=%s status=%s; no payment or funding", tx.Reference.ID, got.Status)
}

func sandboxEntity(t *testing.T, client *http.Client, key, method, path string, body any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal("invalid sandbox setup request")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://sandbox-api.paddle.com"+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal("invalid sandbox setup URL")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Paddle-Version", "1")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("sandbox setup transport failed")
	}
	defer response.Body.Close()
	var envelope struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope) != nil {
		t.Fatal("invalid sandbox setup response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("sandbox setup %s %s: status=%d code=%s", method, path, response.StatusCode, envelope.Error.Code)
	}
	return envelope.Data.ID
}
