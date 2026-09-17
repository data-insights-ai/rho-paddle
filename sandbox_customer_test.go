package paddle_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	paddle "github.com/data-insights-ai/rho-paddle"
)

// Subscribing twice with the same email is the normal case, not the edge case:
// an upgrade, a resubscribe, a retry after a declined card. CreateCustomer is
// correct exactly once and then fails forever, so a checkout flow built on it
// breaks for every returning customer.
func TestSandboxLinkCustomerIsRepeatable(t *testing.T) {
	client := sandboxClient(t)
	ctx := t.Context()

	suffix := strings.ReplaceAll(time.Now().UTC().Format("20060102T150405.000000000"), ".", "")
	account := "rho-link-" + suffix
	email := "rho-link-" + suffix + "@example.com"

	first, err := client.LinkCustomer(ctx, paddle.CustomerInput{
		Account: billing.AccountID(account), Operation: "link-" + suffix, Email: email,
	})
	if err != nil {
		t.Fatalf("first link: %v", err)
	}
	if !strings.HasPrefix(first.Reference.ID, "ctm_") {
		t.Fatalf("first link returned %q", first.Reference.ID)
	}

	second, err := client.LinkCustomer(ctx, paddle.CustomerInput{
		Account: billing.AccountID(account), Operation: "link-" + suffix + "-again", Email: email,
	})
	if err != nil {
		t.Fatalf("second link failed, so a returning customer cannot subscribe: %v", err)
	}
	if second.Reference != first.Reference {
		t.Fatalf("second link returned a different customer: %v then %v",
			first.Reference, second.Reference)
	}

	// An email is not proof of ownership. Another account presenting the same
	// address must not inherit this customer's payment methods and history.
	if _, err := client.LinkCustomer(ctx, paddle.CustomerInput{
		Account: billing.AccountID(account + "-other"), Operation: "link-" + suffix + "-foreign", Email: email,
	}); !errors.Is(err, paddle.ErrCustomerForeign) {
		t.Fatalf("a foreign account linked to this customer: err = %v", err)
	}
}
