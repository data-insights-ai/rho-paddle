package paddle_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/provider"
	"github.com/data-insights-ai/rho-billing/subscription"

	paddle "github.com/data-insights-ai/rho-paddle"
)

// fakePaddle answers like Paddle's API and records what it was asked.
type fakePaddle struct {
	t        *testing.T
	requests []string
	bodies   []map[string]any
	answers  map[string]string
}

func (f *fakePaddle) RoundTrip(r *http.Request) (*http.Response, error) {
	key := r.Method + " " + r.URL.RequestURI()
	f.requests = append(f.requests, key)
	if r.Body != nil {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.bodies = append(f.bodies, body)
	}
	if r.Header.Get("Authorization") != "Bearer pdl_sdbx_test" {
		f.t.Errorf("%s without the API key", key)
	}
	answer, ok := f.answers[key]
	if !ok {
		f.t.Errorf("unexpected request %s", key)
		answer = `{"error":{"code":"not_found"}}`
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(answer)), Request: r}, nil
}

func client(t *testing.T, answers map[string]string) (*paddle.Client, *fakePaddle) {
	f := &fakePaddle{t: t, answers: answers}
	c, err := paddle.New(paddle.Config{
		Environment: paddle.Sandbox, APIKey: "pdl_sdbx_test", WebhookSecrets: []string{"whsec"},
		HTTPClient: &http.Client{Transport: f},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func TestCheckoutCarriesTheReference(t *testing.T) {
	c, f := client(t, map[string]string{
		"POST /transactions": `{"data":{"id":"txn_1","checkout":{"url":"https://pay.example/checkout?_ptxn=txn_1"}}}`,
	})
	out, err := c.Checkout(t.Context(), provider.CheckoutRequest{
		Items: []subscription.Item{{PriceID: "pri_pro", Quantity: 10}}, Reference: "org-1", Customer: "ctm_1",
	})
	if err != nil || out.ID != "txn_1" || !strings.Contains(out.URL, "txn_1") {
		t.Fatalf("checkout: %+v %v", out, err)
	}
	body := f.bodies[0]
	if body["customer_id"] != "ctm_1" || body["custom_data"].(map[string]any)["reference"] != "org-1" {
		t.Errorf("request: %v", body)
	}
}

func TestCatalogWalksEveryPage(t *testing.T) {
	c, _ := client(t, map[string]string{
		"GET /products?include=prices&status=active,archived&per_page=200": `{"data":[{"id":"pro_1","name":"AI SOC Professional","status":"active",
			"custom_data":{"reference":"soc-professional@1"},"prices":[{"id":"pri_m","description":"Monthly","status":"active",
			"unit_price":{"amount":"4900","currency_code":"EUR"},"billing_cycle":{"interval":"month","frequency":1}}]}],
			"meta":{"pagination":{"has_more":true,"next":"https://sandbox-api.paddle.com/products?after=pro_1&include=prices"}}}`,
		"GET /products?after=pro_1&include=prices": `{"data":[{"id":"pro_2","name":"Credits","status":"archived","prices":[
			{"id":"pri_c","status":"active","custom_data":{"reference":"credits:100000"},"unit_price":{"amount":"10000","currency_code":"EUR"}}]}],
			"meta":{"pagination":{"has_more":false}}}`,
	})
	products, err := c.Catalog(t.Context())
	if err != nil || len(products) != 2 {
		t.Fatalf("catalog: %+v %v", products, err)
	}
	pro, credits := products[0], products[1]
	if pro.Reference != "soc-professional@1" || pro.Prices[0].Amount != 4900 || pro.Prices[0].Interval != "month" {
		t.Errorf("subscription product: %+v", pro)
	}
	if !credits.Archived || credits.Prices[0].Interval != "" || credits.Prices[0].Reference != "credits:100000" {
		t.Errorf("one-time product: %+v", credits)
	}
}

func TestChargeAndPortal(t *testing.T) {
	c, f := client(t, map[string]string{
		"POST /subscriptions/sub_1/charge":      `{"data":{"id":"sub_1"}}`,
		"POST /customers/ctm_1/portal-sessions": `{"data":{"urls":{"general":{"overview":"https://customer-portal.paddle.com/x"}}}}`,
	})
	if err := c.Charge(t.Context(), provider.ChargeRequest{Subscription: "sub_1", PriceID: "pri_over", Quantity: 3}); err != nil {
		t.Fatal(err)
	}
	if f.bodies[0]["effective_from"] != "next_billing_period" {
		t.Errorf("charge: %v", f.bodies[0])
	}
	link, err := c.CustomerPortal(t.Context(), "ctm_1")
	if err != nil || link != "https://customer-portal.paddle.com/x" {
		t.Errorf("portal: %q %v", link, err)
	}
}

func sign(body string, at time.Time) string {
	ts := fmt.Sprint(at.Unix())
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write([]byte(ts + ":" + body))
	return "ts=" + ts + ";h1=" + hex.EncodeToString(mac.Sum(nil))
}

const subscriptionEvent = `{"event_id":"evt_1","event_type":"subscription.updated","occurred_at":"2026-10-20T10:00:00Z",
	"data":{"id":"sub_1","status":"past_due","customer_id":"ctm_1","custom_data":{"reference":"org-1"},
	"items":[{"quantity":10,"price":{"id":"pri_pro"}}],
	"current_billing_period":{"starts_at":"2026-10-01T00:00:00Z","ends_at":"2026-11-01T00:00:00Z"},
	"scheduled_change":{"action":"cancel","effective_at":"2026-11-01T00:00:00Z"}}}`

func TestWebhooksDeliverVerifiedEvents(t *testing.T) {
	c, _ := client(t, nil)
	var got []provider.Event
	h := c.Webhooks(func(_ context.Context, e provider.Event) error {
		got = append(got, e)
		return nil
	})
	post := func(body, sig string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/paddle", strings.NewReader(body))
		req.Header.Set("Paddle-Signature", sig)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(subscriptionEvent, sign(subscriptionEvent, time.Now())); code != 200 || len(got) != 1 {
		t.Fatalf("signed event: %d, %d delivered", code, len(got))
	}
	s := got[0].Subscription
	if s.Status != subscription.PastDue || s.Reference != "org-1" || s.Items[0].Quantity != 10 ||
		s.CancelAt.IsZero() || s.Revision.EventID != "evt_1" || s.PeriodEnd.Month() != time.November {
		t.Errorf("subscription: %+v", s)
	}
	if code := post(subscriptionEvent, sign(subscriptionEvent, time.Now().Add(-time.Hour))); code != 401 {
		t.Errorf("an old signature: %d", code)
	}
	if code := post(subscriptionEvent, sign(strings.Replace(subscriptionEvent, "past_due", "active", 1), time.Now())); code != 401 {
		t.Errorf("a changed body: %d", code)
	}
	ignored := `{"event_id":"evt_2","event_type":"customer.created","occurred_at":"2026-10-20T10:00:00Z","data":{}}`
	if code := post(ignored, sign(ignored, time.Now())); code != 200 || len(got) != 1 {
		t.Errorf("an ignored kind: %d", code)
	}
}

func TestAFailedHandlerMakesPaddleRetry(t *testing.T) {
	c, _ := client(t, nil)
	h := c.Webhooks(func(context.Context, provider.Event) error { return errors.New("database down") })
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(subscriptionEvent))
	req.Header.Set("Paddle-Signature", sign(subscriptionEvent, time.Now()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("status %d", rec.Code)
	}
}

func TestCompletedTransactionsBecomePayments(t *testing.T) {
	body := `{"event_id":"evt_9","event_type":"transaction.completed","occurred_at":"2026-10-20T10:00:00Z",
		"data":{"id":"txn_9","customer_id":"ctm_1","subscription_id":null,"currency_code":"EUR","billed_at":"2026-10-20T09:59:00Z",
		"custom_data":{"reference":"org-1"},"items":[{"quantity":2,"price":{"id":"pri_c"}}],"details":{"totals":{"grand_total":"23800"}}}}`
	e, err := paddle.Decode([]byte(body))
	if err != nil || e.Kind != provider.PaymentCompleted {
		t.Fatal(e, err)
	}
	p := e.Payment
	if p.Total != 23800 || p.Reference != "org-1" || p.Subscription != "" || p.Items[0].Quantity != 2 {
		t.Errorf("payment: %+v", p)
	}
}

func TestRefusalsAreAPIErrors(t *testing.T) {
	c, err := paddle.New(paddle.Config{
		Environment: paddle.Sandbox, APIKey: "pdl_sdbx_test", WebhookSecrets: []string{"whsec"},
		HTTPClient: &http.Client{Transport: refusing{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CustomerPortal(t.Context(), "ctm_01aaaaaaaaaaaaaaaaaaaaaaaaaa")
	var refused *paddle.APIError
	if !errors.As(err, &refused) || refused.StatusCode != http.StatusNotFound || refused.Code != "not_found" ||
		refused.RequestID != "req_1" || refused.Detail != "Customer not found." {
		t.Fatalf("err = %+v", err)
	}
}

type refusing struct{}

func (refusing) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{"Content-Type": {"application/json"}}, Request: r,
		Body: io.NopCloser(strings.NewReader(`{"error":{"code":"not_found","detail":"Customer not found."},"meta":{"request_id":"req_1"}}`))}, nil
}

func TestCreateCustomerKeepsTheAccountsReference(t *testing.T) {
	c, f := client(t, map[string]string{
		"POST /customers": `{"data":{"id":"ctm_01hv8wt8nffez4p2t6typn4a5j","email":"pay@acme.example"}}`,
	})
	id, err := c.CreateCustomer(t.Context(), provider.CustomerRequest{Email: "pay@acme.example", Name: "Acme", Reference: "org-1"})
	if err != nil || id != "ctm_01hv8wt8nffez4p2t6typn4a5j" {
		t.Fatalf("customer %q, %v", id, err)
	}
	if b := f.bodies[0]; b["email"] != "pay@acme.example" || b["name"] != "Acme" || b["custom_data"].(map[string]any)["reference"] != "org-1" {
		t.Errorf("request: %v", b)
	}
}

// conflicting refuses the address as taken and answers the customer that
// has it.
type conflicting struct{ detail, existing string }

func (c conflicting) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Request: r,
			Body: io.NopCloser(strings.NewReader(c.existing))}, nil
	}
	return &http.Response{StatusCode: http.StatusConflict, Header: http.Header{"Content-Type": {"application/json"}}, Request: r,
		Body: io.NopCloser(strings.NewReader(`{"error":{"code":"customer_already_exists","detail":"` + c.detail + `"},"meta":{"request_id":"req_2"}}`))}, nil
}

func TestATakenAddressIsNeverSomeoneElsesCustomer(t *testing.T) {
	const id = "ctm_01hv8wt8nffez4p2t6typn4a5j"
	taken := "customer email conflicts with customer of ID " + id
	for name, c := range map[string]struct {
		conflict conflicting
		want     string
	}{
		"this account's":       {conflicting{taken, `{"data":{"id":"` + id + `","custom_data":{"reference":"org-1"}}}`}, id},
		"another account's":    {conflicting{taken, `{"data":{"id":"` + id + `","custom_data":{"reference":"org-2"}}}`}, ""},
		"a prefix of it":       {conflicting{taken, `{"data":{"id":"` + id + `","custom_data":{"reference":"org-1x"}}}`}, ""},
		"without a reference":  {conflicting{taken, `{"data":{"id":"` + id + `"}}`}, ""},
		"unnamed in the error": {conflicting{"customer email conflicts", ""}, ""},
	} {
		client, err := paddle.New(paddle.Config{Environment: paddle.Sandbox, APIKey: "pdl_sdbx_test", WebhookSecrets: []string{"whsec"},
			HTTPClient: &http.Client{Transport: c.conflict}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := client.CreateCustomer(t.Context(), provider.CustomerRequest{Email: "victim@example.com", Reference: "org-1"})
		if got != c.want || (c.want == "") != errors.Is(err, paddle.ErrCustomerExists) {
			t.Errorf("%s: customer %q, %v", name, got, err)
		}
	}
}

func TestGetTransactionNamesItsCustomerAndSubscription(t *testing.T) {
	c, _ := client(t, map[string]string{
		"GET /transactions/txn_1": `{"data":{"id":"txn_1","status":"past_due","customer_id":"ctm_1","subscription_id":"sub_1","custom_data":{"reference":"org-1"}}}`,
	})
	got, err := c.GetTransaction(t.Context(), "txn_1")
	want := provider.Transaction{ID: "txn_1", Status: "past_due", Customer: "ctm_1", Subscription: "sub_1", Reference: "org-1"}
	if err != nil || got != want {
		t.Errorf("transaction %+v, %v", got, err)
	}
}

type claimable struct {
	conflicting
	patched *string
}

func (c claimable) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPatch {
		body, _ := io.ReadAll(r.Body)
		*c.patched = r.URL.Path + " " + string(body)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Request: r,
			Body: io.NopCloser(strings.NewReader(`{"data":{}}`))}, nil
	}
	return c.conflicting.RoundTrip(r)
}

func TestAProvenAddressClaimsACustomerNobodyReferences(t *testing.T) {
	const id = "ctm_01hv8wt8nffez4p2t6typn4a5j"
	taken := "customer email conflicts with customer of ID " + id
	for name, c := range map[string]struct {
		existing string
		want     string
		patch    string
	}{
		"without a reference":   {`{"data":{"id":"` + id + `","custom_data":{"project":"other"}}}`, id, `/customers/` + id + ` {"custom_data":{"project":"other","reference":"org-1"}}`},
		"without custom data":   {`{"data":{"id":"` + id + `"}}`, id, `/customers/` + id + ` {"custom_data":{"reference":"org-1"}}`},
		"this account's":        {`{"data":{"id":"` + id + `","custom_data":{"reference":"org-1"}}}`, id, ""},
		"another account's":     {`{"data":{"id":"` + id + `","custom_data":{"reference":"org-2"}}}`, "", ""},
		"another id in the GET": {`{"data":{"id":"ctm_01hv8wt8nffez4p2t6typn4a5k"}}`, "", ""},
	} {
		var patched string
		client, err := paddle.New(paddle.Config{Environment: paddle.Sandbox, APIKey: "pdl_sdbx_test", WebhookSecrets: []string{"whsec"},
			HTTPClient: &http.Client{Transport: claimable{conflicting{taken, c.existing}, &patched}}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := client.ClaimCustomer(t.Context(), provider.CustomerRequest{Email: "buyer@example.com", Reference: "org-1"})
		if got != c.want || (c.want == "") != errors.Is(err, paddle.ErrCustomerExists) || patched != c.patch {
			t.Errorf("%s: customer %q, %v, patch %q", name, got, err, patched)
		}
	}
}
