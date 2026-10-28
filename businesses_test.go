package paddle_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/data-insights-ai/rho-billing/provider"

	paddle "github.com/data-insights-ai/rho-paddle"
)

const (
	searchHopper = "GET /customers/ctm_1/businesses?search=Hopper+GmbH&status=active"
	transaction  = `{"data":{"id":"txn_1","checkout":{"url":"https://pay.example/checkout?_ptxn=txn_1"}}}`
)

func businessCheckout(b provider.Business) provider.CheckoutRequest {
	return provider.CheckoutRequest{Customer: "ctm_1", Reference: "org-1", Business: &b}
}

func TestACheckoutForANewBusinessCreatesIt(t *testing.T) {
	c, f := client(t, map[string]string{
		searchHopper:                       `{"data":[{"id":"biz_other","name":"Hopper Holding"}]}`,
		"POST /customers/ctm_1/businesses": `{"data":{"id":"biz_1","name":"Hopper GmbH"}}`,
		"POST /transactions":               transaction,
	})
	if _, err := c.Checkout(t.Context(), businessCheckout(provider.Business{Name: " Hopper GmbH ", TaxID: "ATU12345678"})); err != nil {
		t.Fatal(err)
	}
	if created := f.bodies[1]; created["name"] != "Hopper GmbH" || created["tax_identifier"] != "ATU12345678" {
		t.Errorf("created %v", created)
	}
	if txn := f.bodies[2]; txn["business_id"] != "biz_1" || txn["customer_id"] != "ctm_1" {
		t.Errorf("transaction %v", txn)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "ATU12345678") {
			t.Errorf("the tax number is in a URL: %s", r)
		}
	}
}

func TestACheckoutReusesTheCustomersBusiness(t *testing.T) {
	for name, tc := range map[string]struct {
		taxID, known string
		patched      bool
	}{
		"same number":     {"ATU12345678", "ATU12345678", false},
		"new number":      {"ATU87654321", "ATU12345678", true},
		"gains a number":  {"ATU12345678", "", true},
		"keeps its known": {"", "ATU12345678", false},
	} {
		answers := map[string]string{
			searchHopper:         `{"data":[{"id":"biz_1","name":"hopper gmbh","tax_identifier":"` + tc.known + `"}]}`,
			"POST /transactions": transaction,
		}
		if tc.patched {
			answers["PATCH /customers/ctm_1/businesses/biz_1"] = `{"data":{"id":"biz_1"}}`
		}
		c, f := client(t, answers)
		if _, err := c.Checkout(t.Context(), businessCheckout(provider.Business{Name: "Hopper GmbH", TaxID: tc.taxID})); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		last := f.bodies[len(f.bodies)-1]
		if last["business_id"] != "biz_1" || len(f.requests) != map[bool]int{true: 3, false: 2}[tc.patched] {
			t.Errorf("%s: requests %v, transaction %v", name, f.requests, last)
		}
		if tc.patched && f.bodies[1]["tax_identifier"] != tc.taxID {
			t.Errorf("%s: patched %v", name, f.bodies[1])
		}
	}
}

func TestACheckoutForABusinessIsCheckedFirst(t *testing.T) {
	for name, tc := range map[string]struct {
		r    provider.CheckoutRequest
		want error
	}{
		"no customer": {provider.CheckoutRequest{Business: &provider.Business{Name: "Hopper GmbH"}}, paddle.ErrBusinessNeedsCustomer},
		"no name":     {businessCheckout(provider.Business{TaxID: "ATU12345678"}), provider.ErrInvalidBusiness},
	} {
		c, f := client(t, nil)
		if _, err := c.Checkout(t.Context(), tc.r); !errors.Is(err, tc.want) || len(f.requests) != 0 {
			t.Errorf("%s: %v, requests %v", name, err, f.requests)
		}
	}
}

func TestACheckoutWithoutABusinessIsUnchanged(t *testing.T) {
	c, f := client(t, map[string]string{"POST /transactions": transaction})
	if _, err := c.Checkout(t.Context(), provider.CheckoutRequest{Customer: "ctm_1", Reference: "org-1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.bodies[0]["business_id"]; ok || len(f.requests) != 1 {
		t.Errorf("requests %v, body %v", f.requests, f.bodies[0])
	}
}

func TestACheckoutFindsTheCustomersBusinessBeyondTheFirstPage(t *testing.T) {
	c, f := client(t, map[string]string{
		searchHopper: `{"data":[{"id":"biz_other","name":"Hopper Holding"}],
			"meta":{"pagination":{"has_more":true,"next":"https://sandbox-api.paddle.com/customers/ctm_1/businesses?after=biz_other&search=Hopper+GmbH&status=active"}}}`,
		"GET /customers/ctm_1/businesses?after=biz_other&search=Hopper+GmbH&status=active": `{"data":[{"id":"biz_1","name":"Hopper GmbH"}],"meta":{"pagination":{"has_more":false}}}`,
		"POST /transactions": transaction,
	})
	if _, err := c.Checkout(t.Context(), businessCheckout(provider.Business{Name: "Hopper GmbH"})); err != nil {
		t.Fatal(err)
	}
	if txn := f.bodies[len(f.bodies)-1]; txn["business_id"] != "biz_1" {
		t.Errorf("transaction %v; want the business on page 2", txn)
	}
}
