package paddle_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	paddle "github.com/data-insights-ai/rho-paddle"
)

const (
	testAPIKey     = "synthetic-paddle-api-key"
	testMerchant   = "merchant-a"
	testAccount    = billing.AccountID("account-a")
	testOperation  = "operation-a"
	testCustomerID = "ctm_0123456789abcdefghijklmnop"
	testOtherCTMID = "ctm_abcdefghijklmnop0123456789"
	testPriceID    = "pri_0123456789abcdefghijklmnop"
	testTxnID      = "txn_0123456789abcdefghijklmnop"
	testSubID      = "sub_0123456789abcdefghijklmnop"
)

type clientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f clientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestNewHasNoIOAndPinsScope(t *testing.T) {
	var calls atomic.Int32
	httpClient := &http.Client{Transport: clientRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected transport call")
	})}
	client, err := paddle.New(paddle.Config{Merchant: testMerchant, Environment: paddle.Live, APIKey: testAPIKey, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("New() made %d transport calls", calls.Load())
	}
	want := billing.Scope{Provider: "paddle", Merchant: testMerchant, Environment: "live"}
	if client.Scope() != want {
		t.Fatalf("Scope() = %#v, want %#v", client.Scope(), want)
	}

	for _, config := range []paddle.Config{
		{Merchant: "", Environment: paddle.Live, APIKey: testAPIKey},
		{Merchant: testMerchant, Environment: "staging", APIKey: testAPIKey},
		{Merchant: testMerchant, Environment: paddle.Live, APIKey: ""},
	} {
		if _, err := paddle.New(config); !errors.Is(err, paddle.ErrInvalid) {
			t.Fatalf("New(%#v) error = %v, want ErrInvalid", config, err)
		}
	}
}

func TestCreateCustomerWireMapping(t *testing.T) {
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodPost, "https://api.paddle.com/customers")
		var body struct {
			Email      string            `json:"email"`
			Name       string            `json:"name"`
			CustomData map[string]string `json:"custom_data"`
		}
		decodeRequest(t, req, &body)
		if body.Email != "owner@example.test" || body.Name != "Ada" || body.CustomData["billing_account_id"] != string(testAccount) || body.CustomData["billing_operation_id"] != testOperation {
			t.Fatalf("request body = %#v", body)
		}
		return jsonResponse(http.StatusCreated, `{"data":{"id":"`+testCustomerID+`"}}`), nil
	})

	got, err := client.CreateCustomer(t.Context(), paddle.CustomerInput{Account: testAccount, Operation: testOperation, Email: "owner@example.test", Name: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Account != testAccount || got.Reference.ID != testCustomerID || got.Reference.Scope != client.Scope() {
		t.Fatalf("CreateCustomer() = %#v", got)
	}
}

func TestCreateCheckoutWireMapping(t *testing.T) {
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodPost, "https://api.paddle.com/transactions")
		var body struct {
			CustomerID     string `json:"customer_id"`
			CollectionMode string `json:"collection_mode"`
			Items          []struct {
				PriceID  string `json:"price_id"`
				Quantity int64  `json:"quantity"`
			} `json:"items"`
			CustomData map[string]string `json:"custom_data"`
		}
		decodeRequest(t, req, &body)
		if body.CustomerID != testCustomerID || body.CollectionMode != "automatic" || len(body.Items) != 1 || body.Items[0].PriceID != testPriceID || body.Items[0].Quantity != 3 || body.CustomData["billing_operation_id"] != testOperation {
			t.Fatalf("request body = %#v", body)
		}
		return transactionResponse(testTxnID, testCustomerID), nil
	})

	got, err := client.CreateCheckout(t.Context(), paddle.CheckoutInput{
		Customer:  customer(client.Scope()),
		Operation: testOperation,
		Items:     []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != testTxnID || got.Customer.ID != testCustomerID || got.Status != "ready" || got.Currency != "USD" || got.CheckoutURL != "https://checkout.example.test/session" {
		t.Fatalf("CreateCheckout() = %#v", got)
	}
}

func TestCreateCheckoutCorrelationRoundTrip(t *testing.T) {
	quoteFP := strings.Repeat("a", 64)
	requestFP := strings.Repeat("b", 64)
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodPost, "https://api.paddle.com/transactions")
		var body struct {
			CustomerID     string `json:"customer_id"`
			CollectionMode string `json:"collection_mode"`
			Items          []struct {
				PriceID  string `json:"price_id"`
				Quantity int64  `json:"quantity"`
			} `json:"items"`
			CustomData map[string]string `json:"custom_data"`
		}
		decodeRequest(t, req, &body)
		if body.CustomData["billing_checkout_schema"] != "rho.checkout.v1" || body.CustomData["billing_intent_id"] != "intent-a" || body.CustomData["billing_quote_fingerprint"] != quoteFP || body.CustomData["billing_request_fingerprint"] != requestFP {
			t.Fatalf("custom data=%#v", body.CustomData)
		}
		return jsonResponse(http.StatusCreated, `{"data":{"id":"`+testTxnID+`","customer_id":"`+testCustomerID+`","status":"ready","currency_code":"USD","origin":"api","collection_mode":"automatic","created_at":"2026-09-16T08:00:00Z","custom_data":{"billing_account_id":"`+string(testAccount)+`","billing_operation_id":"`+testOperation+`","billing_checkout_schema":"rho.checkout.v1","billing_intent_id":"intent-a","billing_quote_fingerprint":"`+quoteFP+`","billing_request_fingerprint":"`+requestFP+`"},"details":{"line_items":[{"id":"txnitm_0123456789abcdefghijklmnop","price_id":"`+testPriceID+`","quantity":2}]}}}`), nil
	})
	correlation := &paddle.CheckoutCorrelation{Account: testAccount, Operation: testOperation, IntentID: "intent-a", QuoteFingerprint: quoteFP, RequestFingerprint: requestFP, Schema: "rho.checkout.v1"}
	got, err := client.CreateCheckout(t.Context(), paddle.CheckoutInput{Customer: customer(client.Scope()), Operation: testOperation, Correlation: correlation, Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Correlation != *correlation || len(got.Items) != 1 || got.Items[0].Quantity != 2 || got.Origin != "api" || got.CollectionMode != "automatic" {
		t.Fatalf("transaction=%#v", got)
	}
}

func TestTransactionsLookupPaginationAndFilters(t *testing.T) {
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		want := "https://api.paddle.com/transactions?after=txn_0123456789abcdefghijklmnop&collection_mode=automatic&created_at%5BGT%5D=2026-09-16T08%3A00%3A00Z&created_at%5BLT%5D=2026-09-16T09%3A00%3A00Z&customer_id=" + testCustomerID + "&order_by=id%5BASC%5D&origin=api&per_page=2&status=ready"
		assertRequest(t, req, http.MethodGet, want)
		return jsonResponse(http.StatusOK, `{"data":[{"id":"txn_0123456789abcdefghijklmnox","customer_id":"`+testCustomerID+`","status":"ready","currency_code":"USD","origin":"api","collection_mode":"automatic","created_at":"2026-09-16T08:30:00Z","details":{"line_items":[{"id":"txnitm_0123456789abcdefghijklmnop","price_id":"`+testPriceID+`","quantity":1}]}}],"meta":{"pagination":{"has_more":true,"next":"https://api.paddle.com/transactions?after=txn_0123456789abcdefghijklmnox&per_page=2"}}}`), nil
	})
	page, err := client.Transactions(t.Context(), paddle.TransactionLookup{Customer: billing.Reference{Scope: client.Scope(), ID: testCustomerID}, CreatedAfter: time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC), CreatedBefore: time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC), Status: "ready", After: testTxnID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !page.HasMore || page.NextAfter != "txn_0123456789abcdefghijklmnox" {
		t.Fatalf("page=%#v", page)
	}
}

func TestTransactionsLookupRejectsMalformedPaginationAndScope(t *testing.T) {
	wrong := billing.Scope{Provider: "paddle", Merchant: "other", Environment: "live"}
	client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":[],"meta":{"pagination":{"has_more":true,"next":"https://evil.example/transactions?after=txn_x"}}}`), nil
	})
	if _, err := client.Transactions(t.Context(), paddle.TransactionLookup{Customer: billing.Reference{Scope: wrong, ID: testCustomerID}}); !errors.Is(err, paddle.ErrInvalid) {
		t.Fatalf("scope error=%v", err)
	}
	if _, err := client.Transactions(t.Context(), paddle.TransactionLookup{Customer: billing.Reference{Scope: client.Scope(), ID: testCustomerID}}); !errors.Is(err, paddle.ErrResponse) {
		t.Fatalf("pagination error=%v", err)
	}
	foreign := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":[{"id":"`+testTxnID+`","customer_id":"`+testOtherCTMID+`","status":"ready","currency_code":"USD","origin":"api","collection_mode":"automatic","created_at":"2026-09-16T08:30:00Z"}],"meta":{"pagination":{"has_more":false}}}`), nil
	})
	if _, err := foreign.Transactions(t.Context(), paddle.TransactionLookup{Customer: billing.Reference{Scope: foreign.Scope(), ID: testCustomerID}}); !errors.Is(err, paddle.ErrResponse) {
		t.Fatalf("foreign customer error=%v", err)
	}
}

func TestCheckoutCorrelationRequiresAllFrozenFields(t *testing.T) {
	var calls atomic.Int32
	client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return transactionResponse(testTxnID, testCustomerID), nil
	})
	correlation := &paddle.CheckoutCorrelation{Account: testAccount, Operation: testOperation, IntentID: "intent-a"}
	_, err := client.CreateCheckout(t.Context(), paddle.CheckoutInput{Customer: customer(client.Scope()), Operation: testOperation, Correlation: correlation, Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}}})
	if !errors.Is(err, paddle.ErrInvalid) || calls.Load() != 0 {
		t.Fatalf("error=%v calls=%d, want local ErrInvalid", err, calls.Load())
	}
}

func TestTransactionWireMapping(t *testing.T) {
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodGet, "https://api.paddle.com/transactions/"+testTxnID)
		return transactionResponse(testTxnID, testCustomerID), nil
	})
	ref := billing.Reference{Scope: client.Scope(), ID: testTxnID}
	got, err := client.Transaction(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference != ref || got.Customer.ID != testCustomerID || got.Status != "ready" {
		t.Fatalf("Transaction() = %#v", got)
	}
}

func TestCreatePortalSessionWireMapping(t *testing.T) {
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodPost, "https://api.paddle.com/customers/"+testCustomerID+"/portal-sessions")
		var body struct {
			Subscriptions []string `json:"subscription_ids"`
		}
		decodeRequest(t, req, &body)
		if len(body.Subscriptions) != 1 || body.Subscriptions[0] != testSubID {
			t.Fatalf("request body = %#v", body)
		}
		return jsonResponse(http.StatusCreated, `{"data":{"customer_id":"`+testCustomerID+`","urls":{"general":{"overview":"https://portal.example.test/overview"},"subscriptions":[{"id":"`+testSubID+`","cancel_subscription":"https://portal.example.test/cancel","update_subscription_payment_method":"https://portal.example.test/payment"}]}}}`), nil
	})

	got, err := client.CreatePortalSession(t.Context(), customer(client.Scope()), []billing.Reference{{Scope: client.Scope(), ID: testSubID}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Customer.Reference.ID != testCustomerID || got.OverviewURL != "https://portal.example.test/overview" || len(got.Subscriptions) != 1 || got.Subscriptions[0].Reference.ID != testSubID {
		t.Fatalf("CreatePortalSession() = %#v", got)
	}
}

func TestCrossScopeInputsNeverReachTransport(t *testing.T) {
	var calls atomic.Int32
	client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(http.StatusOK, `{"data":{}}`), nil
	})
	wrong := billing.Scope{Provider: "paddle", Merchant: "merchant-b", Environment: "live"}
	tests := []struct {
		name string
		call func() error
	}{
		{"checkout customer", func() error {
			_, err := client.CreateCheckout(t.Context(), paddle.CheckoutInput{Customer: customer(wrong), Operation: testOperation, Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}}})
			return err
		}},
		{"checkout price", func() error {
			_, err := client.CreateCheckout(t.Context(), paddle.CheckoutInput{Customer: customer(client.Scope()), Operation: testOperation, Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: wrong, ID: testPriceID}, Quantity: 1}}})
			return err
		}},
		{"transaction", func() error {
			_, err := client.Transaction(t.Context(), billing.Reference{Scope: wrong, ID: testTxnID})
			return err
		}},
		{"portal customer", func() error { _, err := client.CreatePortalSession(t.Context(), customer(wrong), nil); return err }},
		{"portal subscription", func() error {
			_, err := client.CreatePortalSession(t.Context(), customer(client.Scope()), []billing.Reference{{Scope: wrong, ID: testSubID}})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, paddle.ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid inputs made %d transport calls", calls.Load())
	}
}

func TestAcceptedMutationResponseMismatchIsUncertain(t *testing.T) {
	tests := []struct {
		name string
		body string
		call func(*paddle.Client) error
	}{
		{"customer malformed ID", `{"data":{"id":"ctm_bad"}}`, func(client *paddle.Client) error {
			_, err := client.CreateCustomer(t.Context(), paddle.CustomerInput{Account: testAccount, Operation: testOperation, Email: "owner@example.test"})
			return err
		}},
		{"checkout wrong customer", transactionJSON(testTxnID, testOtherCTMID), func(client *paddle.Client) error {
			_, err := client.CreateCheckout(t.Context(), paddle.CheckoutInput{Customer: customer(client.Scope()), Operation: testOperation, Items: []paddle.CheckoutItem{{Price: billing.Reference{Scope: client.Scope(), ID: testPriceID}, Quantity: 1}}})
			return err
		}},
		{"portal wrong customer", `{"data":{"customer_id":"` + testOtherCTMID + `","urls":{"general":{"overview":"https://portal.example.test/overview"},"subscriptions":[]}}}`, func(client *paddle.Client) error {
			_, err := client.CreatePortalSession(t.Context(), customer(client.Scope()), nil)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) { return jsonResponse(http.StatusOK, tt.body), nil })
			err := tt.call(client)
			if !errors.Is(err, paddle.ErrResponse) || !errors.Is(err, paddle.ErrUncertain) {
				t.Fatalf("error = %v, want ErrResponse and ErrUncertain", err)
			}
		})
	}
}

func TestMutationCancellationAfterSubmissionIsUncertainAndNotRetried(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel()
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	_, err := client.CreateCustomer(ctx, paddle.CustomerInput{Account: testAccount, Operation: testOperation, Email: "owner@example.test"})
	if !errors.Is(err, paddle.ErrUncertain) || !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("error = %v, calls = %d", err, calls.Load())
	}
}

func TestGETTransportErrorsAreSafeAndDefinitive(t *testing.T) {
	const sensitive = "sensitive-response-or-network-detail"
	tests := []struct {
		name string
		fn   clientRoundTripFunc
	}{
		{"transport", func(*http.Request) (*http.Response, error) {
			return nil, errors.New(sensitive + " https://authenticated.example.test/path")
		}},
		{"provider response", func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusInternalServerError, `{"error":{"code":"internal_error","detail":"`+sensitive+` https://authenticated.example.test/path"}}`), nil
		}},
		{"reflected credential", func(*http.Request) (*http.Response, error) {
			response := jsonResponse(http.StatusBadRequest, `{"error":{"code":"`+testAPIKey+`"},"meta":{"request_id":"`+testAPIKey+`"}}`)
			response.Header.Set("Paddle-Request-Id", testAPIKey)
			return response, nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClient(t, paddle.Live, tt.fn)
			_, err := client.Transaction(t.Context(), billing.Reference{Scope: client.Scope(), ID: testTxnID})
			if err == nil || errors.Is(err, paddle.ErrUncertain) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), sensitive) || strings.Contains(err.Error(), "authenticated") || strings.Contains(err.Error(), testAPIKey) {
				t.Fatalf("unsafe error = %v", err)
			}
		})
	}
}

func newClient(t *testing.T, environment paddle.Environment, fn clientRoundTripFunc) *paddle.Client {
	t.Helper()
	client, err := paddle.New(paddle.Config{Merchant: testMerchant, Environment: environment, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: fn}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func customer(scope billing.Scope) paddle.CustomerReference {
	return paddle.CustomerReference{Account: testAccount, Reference: billing.Reference{Scope: scope, ID: testCustomerID}}
}

func assertRequest(t *testing.T, req *http.Request, method, rawURL string) {
	t.Helper()
	if req.Method != method || req.URL.String() != rawURL {
		t.Fatalf("request = %s %s, want %s %s", req.Method, req.URL, method, rawURL)
	}
	if req.Header.Get("Authorization") != "Bearer "+testAPIKey || req.Header.Get("Paddle-Version") != "1" || req.Header.Get("Accept") != "application/json" || req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request headers = %#v", req.Header)
	}
}

func decodeRequest(t *testing.T, req *http.Request, output any) {
	t.Helper()
	defer req.Body.Close()
	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		t.Fatal(err)
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func transactionResponse(id, customerID string) *http.Response {
	return jsonResponse(http.StatusOK, transactionJSON(id, customerID))
}

func transactionJSON(id, customerID string) string {
	return `{"data":{"id":"` + id + `","customer_id":"` + customerID + `","status":"ready","currency_code":"USD","origin":"api","collection_mode":"automatic","created_at":"2026-09-16T08:00:00Z","details":{"line_items":[{"id":"txnitm_0123456789abcdefghijklmnop","price_id":"pri_0123456789abcdefghijklmnop","quantity":1}]},"checkout":{"url":"https://checkout.example.test/session"}}}`
}
