package paddle_test

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestSandboxCustomerAndPortalSessionAreEphemeral(t *testing.T) {
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
	run := "rho-billing-portal-" + time.Now().UTC().Format("20060102T150405")
	customer, err := client.CreateCustomer(t.Context(), paddle.CustomerInput{Account: "sandbox-portal", Operation: run, Email: run + "@example.com", Name: "Rho Billing Portal"})
	if err != nil {
		t.Fatal(err)
	}
	setup := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer sandboxEntity(t, setup, key, http.MethodPatch, "/customers/"+customer.Reference.ID, map[string]string{"status": "archived"})
	if !strings.HasPrefix(customer.Reference.ID, "ctm_") || customer.Reference.Scope.Environment != "sandbox" {
		t.Fatalf("customer=%+v", customer.Reference)
	}
	session, err := client.CreatePortalSession(t.Context(), customer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if session.OverviewURL == "" || !strings.HasPrefix(session.OverviewURL, "https://") || session.Customer.Reference.ID != customer.Reference.ID {
		t.Fatalf("portal session missing https overview or customer mismatch")
	}
	t.Logf("real sandbox: customer=%s portal_overview_https=true subscriptions=%d; URL not persisted", customer.Reference.ID, len(session.Subscriptions))
}
