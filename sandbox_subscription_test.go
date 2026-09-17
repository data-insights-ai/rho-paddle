package paddle_test

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func sandboxClient(t *testing.T) *paddle.Client {
	t.Helper()
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
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: key, HTTPClient: &http.Client{Timeout: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestSandboxRecurringInvoiceCreatesActiveSubscription(t *testing.T) {
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
	setup := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	run := fmt.Sprintf("rho-sub-%d", time.Now().UnixNano())
	product := sandboxEntity(t, setup, key, http.MethodPost, "/products", map[string]any{"name": run, "tax_category": "saas"})
	t.Cleanup(func() {
		sandboxEntity(t, setup, key, http.MethodPatch, "/products/"+product, map[string]string{"status": "archived"})
	})
	price := sandboxEntity(t, setup, key, http.MethodPost, "/prices", map[string]any{
		"product_id": product, "description": run, "tax_mode": "internal",
		"unit_price":    map[string]string{"amount": "100", "currency_code": "USD"},
		"billing_cycle": map[string]any{"interval": "month", "frequency": 1},
	})
	t.Cleanup(func() {
		sandboxEntity(t, setup, key, http.MethodPatch, "/prices/"+price, map[string]string{"status": "archived"})
	})
	customer := newSandboxCustomer(t)
	customerID, address := customer.CustomerID, customer.AddressID
	txnID := sandboxEntity(t, setup, key, http.MethodPost, "/transactions", map[string]any{
		"customer_id": customerID, "address_id": address, "collection_mode": "manual", "currency_code": "USD", "status": "billed",
		"items":           []map[string]any{{"price_id": price, "quantity": 1}},
		"billing_details": map[string]any{"enable_checkout": false, "payment_terms": map[string]any{"interval": "day", "frequency": 14}},
	})
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: key, HTTPClient: &http.Client{Timeout: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	var txn paddle.Transaction
	deadline := time.Now().Add(15 * time.Second)
	for {
		txn, err = client.Transaction(t.Context(), billing.Reference{Scope: client.Scope(), ID: txnID})
		if err != nil {
			t.Fatal(err)
		}
		if txn.Subscription.ID != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if txn.Status != "billed" || !strings.HasPrefix(txn.Subscription.ID, "sub_") {
		t.Fatalf("transaction=%s status=%s subscription=%s", txn.Reference.ID, txn.Status, txn.Subscription.ID)
	}
	sub, err := client.Subscription(t.Context(), txn.Subscription)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != "active" || sub.CollectionMode != "manual" || sub.Customer.ID != customerID {
		t.Fatalf("subscription=%#v", sub)
	}
	t.Logf("real sandbox: billed recurring invoice transaction=%s subscription=%s status=%s collection=%s; not a simulated webhook", txn.Reference.ID, sub.Reference.ID, sub.Status, sub.CollectionMode)
}

func TestSandboxUpdateSubscriptionQuantityProration(t *testing.T) {
	fixture := newSandboxCustomer(t).withRecurringPrice(t).withSubscription(t)
	client := sandboxClient(t)
	scope := client.Scope()
	sub, err := client.UpdateSubscription(t.Context(), paddle.SubscriptionUpdate{
		Subscription: billing.Reference{Scope: scope, ID: fixture.SubscriptionID},
		Proration:    "prorated_immediately",
		Items:        []paddle.CheckoutItem{{Price: billing.Reference{Scope: scope, ID: fixture.PriceID}, Quantity: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != "active" || len(sub.Items) != 1 || sub.Items[0].Quantity != 2 {
		t.Fatalf("subscription=%#v", sub)
	}
	t.Logf("real sandbox: quantity/proration subscription=%s quantity=%d collection=%s status=%s", sub.Reference.ID, sub.Items[0].Quantity, sub.CollectionMode, sub.Status)
}

func TestSandboxPauseResumeCancelLifecycle(t *testing.T) {
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
	setup := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	run := fmt.Sprintf("rho-life-%d", time.Now().UnixNano())
	product := sandboxEntity(t, setup, key, http.MethodPost, "/products", map[string]any{"name": run, "tax_category": "saas"})
	t.Cleanup(func() {
		sandboxEntity(t, setup, key, http.MethodPatch, "/products/"+product, map[string]string{"status": "archived"})
	})
	price := sandboxEntity(t, setup, key, http.MethodPost, "/prices", map[string]any{
		"product_id": product, "description": run, "tax_mode": "internal",
		"unit_price":    map[string]string{"amount": "100", "currency_code": "USD"},
		"billing_cycle": map[string]any{"interval": "month", "frequency": 1},
	})
	t.Cleanup(func() {
		sandboxEntity(t, setup, key, http.MethodPatch, "/prices/"+price, map[string]string{"status": "archived"})
	})
	customer := newSandboxCustomer(t)
	customerID, address := customer.CustomerID, customer.AddressID
	txnID := sandboxEntity(t, setup, key, http.MethodPost, "/transactions", map[string]any{
		"customer_id": customerID, "address_id": address, "collection_mode": "manual", "currency_code": "USD", "status": "billed",
		"items":           []map[string]any{{"price_id": price, "quantity": 1}},
		"billing_details": map[string]any{"enable_checkout": false, "payment_terms": map[string]any{"interval": "day", "frequency": 14}},
	})
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: key, HTTPClient: &http.Client{Timeout: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	var txn paddle.Transaction
	deadline := time.Now().Add(15 * time.Second)
	for {
		txn, err = client.Transaction(t.Context(), billing.Reference{Scope: client.Scope(), ID: txnID})
		if err != nil {
			t.Fatal(err)
		}
		if txn.Subscription.ID != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.HasPrefix(txn.Subscription.ID, "sub_") {
		t.Fatalf("transaction=%s subscription=%q", txn.Reference.ID, txn.Subscription.ID)
	}
	// The subscription came from a transaction this test created moments ago,
	// so cancelling it at the end cannot touch anything else in the account.
	ref := txn.Subscription
	paused, err := client.PauseSubscription(t.Context(), paddle.PauseInput{Subscription: ref, When: paddle.EffectiveImmediately})
	if err != nil || paused.Status != paddle.SubscriptionPaused {
		t.Fatalf("pause=%#v err=%v", paused, err)
	}
	resumed, err := client.ResumeSubscription(t.Context(), paddle.ResumeInput{Subscription: ref, When: paddle.EffectiveImmediately})
	if err != nil || resumed.Status != paddle.SubscriptionActive {
		t.Fatalf("resume=%#v err=%v", resumed, err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		resumed, err = client.Subscription(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if resumed.Status == paddle.SubscriptionActive && resumed.ScheduledChange.Action == "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if resumed.Status != paddle.SubscriptionActive {
		t.Fatalf("resume settled=%#v", resumed)
	}
	var canceled paddle.Subscription
	deadline = time.Now().Add(15 * time.Second)
	for {
		canceled, err = client.CancelSubscription(t.Context(), paddle.CancelInput{Subscription: ref, When: paddle.EffectiveImmediately})
		if err == nil && canceled.Status == paddle.SubscriptionCanceled {
			break
		}
		var api *paddle.APIError
		if !errors.As(err, &api) || api.StatusCode != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("cancel=%#v err=%v", canceled, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("real sandbox: pause/resume/cancel subscription=%s pause=%s resume=%s cancel=%s invoice_txn=%s", ref.ID, paused.Status, resumed.Status, canceled.Status, txnID)
}

func TestSandboxAutomaticTopUpCapabilityStaysFailClosed(t *testing.T) {
	err := billing.RequireSupported(paddle.AutomaticTopUpCapability())
	capability, ok := errors.AsType[*billing.CapabilityError](err)
	if !ok || capability.Support != billing.SupportUnsupported || capability.Reason != "automatic_top_up" {
		t.Fatalf("automatic top-up=%v", err)
	}
}
