package paddle_test

import (
	"errors"
	"fmt"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestSandboxRefundOnUnpaidInvoiceIsRejected(t *testing.T) {
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
	run := fmt.Sprintf("rho-rf-%d", time.Now().UnixNano())
	product := sandboxEntity(t, setup, key, http.MethodPost, "/products", map[string]any{"name": run, "tax_category": "saas"})
	t.Cleanup(func() {
		sandboxEntity(t, setup, key, http.MethodPatch, "/products/"+product, map[string]string{"status": "archived"})
	})
	price := sandboxEntity(t, setup, key, http.MethodPost, "/prices", map[string]any{
		"product_id": product, "description": run, "tax_mode": "internal",
		"unit_price": map[string]string{"amount": "100", "currency_code": "USD"},
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
	got, err := client.Transaction(t.Context(), billing.Reference{Scope: client.Scope(), ID: txnID})
	if err != nil || len(got.Items) == 0 {
		t.Fatalf("transaction=%+v err=%v", got, err)
	}
	_, err = client.CreateAdjustment(t.Context(), paddle.AdjustmentCreate{
		Action: "refund", Type: "full", Transaction: billing.Reference{Scope: client.Scope(), ID: txnID}, Reason: "sandbox-unpaid-invoice",
		Items: []paddle.AdjustmentCreateItem{{TransactionItemID: got.Items[0].Reference.ID, Type: "full"}},
	})
	if err == nil {
		t.Fatal("refund of unpaid billed invoice succeeded; that is not captured payment")
	}
	var api *paddle.APIError
	if !errors.As(err, &api) || api.StatusCode < 400 {
		t.Fatalf("err=%v", err)
	}
	t.Logf("real sandbox: unpaid billed invoice %s rejected refund status=%d code=%s", txnID, api.StatusCode, api.Code)
}
