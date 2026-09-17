package paddle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// sandboxFixture is a disposable set of provider objects a test owns outright.
//
// Nothing here is shared, pre-existing or named in configuration. A suite that
// leans on objects someone created by hand cannot be run by anyone else, and it
// puts the account owner's data one bad assertion away from being mutated. Each
// test creates what it needs and archives it afterwards.
type sandboxFixture struct {
	Key            string
	Run            string
	CustomerID     string
	AddressID      string
	ProductID      string
	PriceID        string
	TransactionID  string
	SubscriptionID string
}

// sandboxKey reads the sandbox credential, skipping unless tests are opted in.
func sandboxKey(t *testing.T) string {
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
	return key
}

// sandboxSetupClient is the raw HTTP client used to build fixtures. Fixture
// setup deliberately does not go through the adapter: a test must not depend on
// the code it is testing to establish its own preconditions.
func sandboxSetupClient() *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// newSandboxCustomer creates an ephemeral customer with a billing address.
func newSandboxCustomer(t *testing.T) sandboxFixture {
	t.Helper()
	key := sandboxKey(t)
	setup := sandboxSetupClient()
	run := fmt.Sprintf("rho-%d", time.Now().UnixNano())

	customer := sandboxEntity(t, setup, key, http.MethodPost, "/customers", map[string]string{
		"email": run + "@example.com", "name": run,
	})
	t.Cleanup(func() {
		sandboxEntity(t, setup, key, http.MethodPatch, "/customers/"+customer, map[string]string{"status": "archived"})
	})
	address := sandboxEntity(t, setup, key, http.MethodPost, "/customers/"+customer+"/addresses", map[string]string{
		"country_code": "US", "postal_code": "10021", "city": "New York",
		"region": "NY", "first_line": "4050 Jefferson Plaza",
	})
	return sandboxFixture{Key: key, Run: run, CustomerID: customer, AddressID: address}
}

// withRecurringPrice adds an archived-on-cleanup product and monthly price.
func (f sandboxFixture) withRecurringPrice(t *testing.T) sandboxFixture {
	t.Helper()
	setup := sandboxSetupClient()
	product := sandboxEntity(t, setup, f.Key, http.MethodPost, "/products", map[string]any{
		"name": f.Run, "tax_category": "saas",
	})
	t.Cleanup(func() {
		sandboxEntity(t, setup, f.Key, http.MethodPatch, "/products/"+product, map[string]string{"status": "archived"})
	})
	price := sandboxEntity(t, setup, f.Key, http.MethodPost, "/prices", map[string]any{
		"product_id": product, "description": f.Run, "tax_mode": "internal",
		"unit_price":    map[string]string{"amount": "100", "currency_code": "USD"},
		"billing_cycle": map[string]any{"interval": "month", "frequency": 1},
	})
	t.Cleanup(func() {
		sandboxEntity(t, setup, f.Key, http.MethodPatch, "/prices/"+price, map[string]string{"status": "archived"})
	})
	f.ProductID, f.PriceID = product, price
	return f
}

// withSubscription bills a manual-collection transaction, which creates an
// active subscription.
//
// This is the one route to a real subscription that needs no browser: an
// automatic transaction requires a card, and capturing one requires Paddle.js.
// A manual invoice is billed server-side and the subscription it creates is
// genuine provider state, not a simulation.
func (f sandboxFixture) withSubscription(t *testing.T) sandboxFixture {
	t.Helper()
	if f.PriceID == "" {
		t.Fatal("withSubscription needs a price")
	}
	setup := sandboxSetupClient()
	txn := sandboxEntity(t, setup, f.Key, http.MethodPost, "/transactions", map[string]any{
		"customer_id": f.CustomerID, "address_id": f.AddressID,
		"collection_mode": "manual", "currency_code": "USD", "status": "billed",
		"items": []map[string]any{{"price_id": f.PriceID, "quantity": 1}},
		"billing_details": map[string]any{
			"enable_checkout": false,
			"payment_terms":   map[string]any{"interval": "day", "frequency": 14},
		},
	})
	f.TransactionID = txn
	f.SubscriptionID = sandboxAwaitSubscription(t, f.Key, txn)
	return f
}

// sandboxAwaitSubscription waits for the provider to attach a subscription to a
// billed recurring transaction. The attachment is asynchronous, so reading it
// once and concluding "no subscription" is a race, not a result.
func sandboxAwaitSubscription(t *testing.T, key, txnID string) string {
	t.Helper()
	setup := sandboxSetupClient()
	deadline := time.Now().Add(20 * time.Second)
	for {
		id := sandboxField(t, setup, key, "/transactions/"+txnID, "subscription_id")
		if id != "" {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("transaction %s never gained a subscription", txnID)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// sandboxField reads one top-level string field of a provider object.
func sandboxField(t *testing.T, client *http.Client, key, path, field string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://sandbox-api.paddle.com"+path, nil)
	if err != nil {
		t.Fatal("invalid sandbox read URL")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Paddle-Version", "1")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("sandbox read transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope) != nil {
		t.Fatal("invalid sandbox read response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("sandbox read %s: status=%d", path, response.StatusCode)
	}
	value, _ := envelope.Data[field].(string)
	return value
}
