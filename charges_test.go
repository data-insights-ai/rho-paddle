package paddle_test

import (
	"net/http"
	"testing"

	"github.com/data-insights-ai/rho-billing/provider"

	paddle "github.com/data-insights-ai/rho-paddle"
)

const (
	subscriptionProduct = "GET /subscriptions/sub_1"
	customPrices        = "GET /prices?order_by=id%5BDESC%5D&per_page=200&product_id=pro_soc&type=custom"
	nextTransaction     = "GET /subscriptions/sub_1?include=next_transaction"
	billedTransactions  = "GET /transactions?order_by=created_at%5BDESC%5D&per_page=200&subscription_id=sub_1"
)

var keyedPrice = `{"data":[{"id":"pri_other","custom_data":null},{"id":"pri_over","custom_data":{"charge_key":"org-1:2026-10"}}],"meta":{"pagination":{"has_more":false}}}`

func TestAChargeAtACustomPriceCarriesItsKey(t *testing.T) {
	c, f := client(t, map[string]string{
		subscriptionProduct:                `{"data":{"items":[{"price":{"id":"pri_pro","product_id":"pro_soc"}}]}}`,
		"POST /subscriptions/sub_1/charge": `{"data":{"id":"sub_1"}}`,
	})
	err := c.Charge(t.Context(), provider.ChargeRequest{Subscription: "sub_1", Key: "org-1:2026-10", Custom: &provider.CustomItem{
		Name: "Overage", Amount: 250, Currency: "EUR", Quantity: 1, Reference: "overage:2026-10",
	}})
	if err != nil {
		t.Fatal(err)
	}
	price := f.bodies[1]["items"].([]any)[0].(map[string]any)["price"].(map[string]any)
	data := price["custom_data"].(map[string]any)
	if data[paddle.ChargeKey] != "org-1:2026-10" || data[paddle.ReferenceKey] != "overage:2026-10" {
		t.Errorf("custom data %v", data)
	}
}

func TestFindChargeScheduledForTheNextRenewal(t *testing.T) {
	c, _ := client(t, map[string]string{
		subscriptionProduct: `{"data":{"items":[{"price":{"id":"pri_pro","product_id":"pro_soc"}}]}}`,
		customPrices:        keyedPrice,
		nextTransaction:     `{"data":{"next_transaction":{"details":{"line_items":[{"price_id":"pri_pro"},{"price_id":"pri_over"}]}}}}`,
	})
	got, err := c.FindCharge(t.Context(), "sub_1", "org-1:2026-10")
	if err != nil || !got.Found || got.Payment != "" {
		t.Fatalf("%+v %v; want found, not billed yet", got, err)
	}
}

func TestFindChargeBilled(t *testing.T) {
	c, _ := client(t, map[string]string{
		subscriptionProduct: `{"data":{"items":[{"price":{"id":"pri_pro","product_id":"pro_soc"}}]}}`,
		customPrices:        keyedPrice,
		nextTransaction:     `{"data":{"next_transaction":{"details":{"line_items":[{"price_id":"pri_pro"}]}}}}`,
		billedTransactions: `{"data":[{"id":"txn_new","status":"completed","details":{"line_items":[{"price_id":"pri_pro"}]}},
			{"id":"txn_old","status":"completed","details":{"line_items":[{"price_id":"pri_pro"},{"price_id":"pri_over"}]}}],"meta":{"pagination":{"has_more":false}}}`,
	})
	got, err := c.FindCharge(t.Context(), "sub_1", "org-1:2026-10")
	if err != nil || !got.Found || got.Payment != "txn_old" {
		t.Fatalf("%+v %v; want billed on txn_old", got, err)
	}
}

func TestFindChargeNotThere(t *testing.T) {
	c, _ := client(t, map[string]string{
		subscriptionProduct: `{"data":{"items":[{"price":{"id":"pri_pro","product_id":"pro_soc"}}]}}`,
		customPrices:        `{"data":[{"id":"pri_other","custom_data":null}],"meta":{"pagination":{"has_more":false}}}`,
	})
	got, err := c.FindCharge(t.Context(), "sub_1", "org-1:2026-10")
	if err != nil || got.Found {
		t.Fatalf("%+v %v; want not found", got, err)
	}
}

func TestGetTransactionNamesItsLines(t *testing.T) {
	c, _ := client(t, map[string]string{
		"GET /transactions/txn_1": `{"data":{"id":"txn_1","status":"completed","customer_id":"ctm_1","custom_data":{"reference":"org-1"},
			"details":{"line_items":[{"id":"txnitm_1","price_id":"pri_pack","quantity":2,"totals":{"total":"11900"}}]}}}`,
	})
	tx, err := c.GetTransaction(t.Context(), "txn_1")
	if err != nil || len(tx.Lines) != 1 {
		t.Fatalf("%+v %v", tx, err)
	}
	if l := tx.Lines[0]; l.ID != "txnitm_1" || l.PriceID != "pri_pack" || l.Quantity != 2 || l.Total != 11900 {
		t.Errorf("line %+v", l)
	}
}

func TestCreateCustomerSendsTheLocale(t *testing.T) {
	c, f := client(t, map[string]string{"POST /customers": `{"data":{"id":"ctm_1"}}`})
	if _, err := c.CreateCustomer(t.Context(), provider.CustomerRequest{Email: "pay@acme.example", Reference: "org-1", Locale: "de"}); err != nil {
		t.Fatal(err)
	}
	if f.bodies[0]["locale"] != "de" {
		t.Errorf("request %v", f.bodies[0])
	}
}

func TestTheAccountsExistingCustomerFollowsItsLocale(t *testing.T) {
	const id = "ctm_01hv8wt8nffez4p2t6typn4a5j"
	for name, c := range map[string]struct{ existing, patch string }{
		"another language": {`{"data":{"id":"` + id + `","locale":"en","custom_data":{"reference":"org-1"}}}`, `/customers/` + id + ` {"locale":"de"}`},
		"the same":         {`{"data":{"id":"` + id + `","locale":"de","custom_data":{"reference":"org-1"}}}`, ""},
	} {
		var patched string
		client, err := paddle.New(paddle.Config{Environment: paddle.Sandbox, APIKey: "pdl_sdbx_test", WebhookSecrets: []string{"whsec"},
			HTTPClient: &http.Client{Transport: claimable{conflicting{"customer email conflicts with customer of ID " + id, c.existing}, &patched}}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := client.CreateCustomer(t.Context(), provider.CustomerRequest{Email: "buyer@example.com", Reference: "org-1", Locale: "de"})
		if got != id || err != nil || patched != c.patch {
			t.Errorf("%s: customer %q, %v, patch %q", name, got, err, patched)
		}
	}
}
