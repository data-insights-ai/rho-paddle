package paddle_test

import (
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"testing"

	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestTransactionsUsesPublishedPaginationAndLineItemShape(t *testing.T) {
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("collection_mode") != "automatic" {
			t.Fatal("automatic collection filter missing")
		}
		return jsonResponse(http.StatusOK, `{"data":[{
			"id":"`+testTxnID+`","customer_id":"`+testCustomerID+`",
			"status":"ready","currency_code":"USD","origin":"api","collection_mode":"automatic",
			"created_at":"2026-09-16T08:00:00Z","custom_data":{"host_flags":{"enabled":true},"count":4},
			"items":[{"price":{"id":"`+testPriceID+`"},"quantity":2},{"price":{"id":"pri_22222222222222222222222222"},"quantity":1}],
			"details":{"line_items":[
				{"id":"txnitm_22222222222222222222222222","price_id":"pri_22222222222222222222222222","quantity":1},
				{"id":"txnitm_11111111111111111111111111","price_id":"`+testPriceID+`","quantity":2}
			]}
		}],"meta":{"pagination":{"has_more":false,"next":"https://api.paddle.com/transactions?after=`+testTxnID+`&per_page=30&customer_id=`+testCustomerID+`"}}}`), nil
	})
	page, err := client.Transactions(t.Context(), paddle.TransactionLookup{Customer: billing.Reference{Scope: client.Scope(), ID: testCustomerID}})
	if err != nil {
		t.Fatal(err)
	}
	if page.HasMore || page.NextAfter != "" || len(page.Items) != 1 || len(page.Items[0].Items) != 2 {
		t.Fatalf("page=%+v", page)
	}
	lines := page.Items[0].Items
	if lines[0].Reference.ID != "txnitm_22222222222222222222222222" || lines[1].Price.ID != testPriceID || lines[1].Quantity != 2 {
		t.Fatalf("authoritative line identities=%+v", lines)
	}
}

func TestTransactionsRejectsMissingPaginationEvidence(t *testing.T) {
	for _, body := range []string{
		`{"data":[]}`, `{"data":[],"meta":{}}`,
		`{"data":[],"meta":{"pagination":{"next":""}}}`,
		`{"data":[],"meta":{"pagination":{"has_more":true,"next":"https://api.paddle.com/transactions?after=txn_11111111111111111111111111&after=txn_22222222222222222222222222"}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) { return jsonResponse(http.StatusOK, body), nil })
			_, err := client.Transactions(t.Context(), paddle.TransactionLookup{Customer: billing.Reference{Scope: client.Scope(), ID: testCustomerID}})
			if !errors.Is(err, paddle.ErrResponse) {
				t.Fatalf("missing/ambiguous pagination error=%v", err)
			}
		})
	}
}

func TestTransactionsOriginAnyAndTotals(t *testing.T) {
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("origin") != "" || req.URL.Query().Get("collection_mode") != "" {
			t.Fatalf("OriginAny must not filter: %s", req.URL.RawQuery)
		}
		return jsonResponse(http.StatusOK, `{"data":[{"id":"`+testTxnID+`","customer_id":"`+testCustomerID+`","status":"completed","currency_code":"EUR","origin":"subscription_recurring","collection_mode":"automatic","created_at":"2026-09-16T08:00:00Z","billed_at":"2026-09-16T08:00:01Z","invoice_number":"INV-1","details":{"totals":{"grand_total":"5929","tax":"1029"},"line_items":[]}}],"meta":{"pagination":{"has_more":false,"next":""}}}`), nil
	})
	page, err := client.Transactions(t.Context(), paddle.TransactionLookup{Customer: billing.Reference{Scope: client.Scope(), ID: testCustomerID}, Origin: paddle.OriginAny})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Origin != "subscription_recurring" || page.Items[0].Total != 5929 || page.Items[0].Tax != 1029 || page.Items[0].InvoiceNumber != "INV-1" || page.Items[0].BilledAt.IsZero() {
		t.Fatalf("items=%#v", page.Items)
	}
}

func TestInvoiceURL(t *testing.T) {
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodGet, "https://sandbox-api.paddle.com/transactions/"+testTxnID+"/invoice?disposition=inline")
		return jsonResponse(http.StatusOK, `{"data":{"url":"https://invoices.example.test/inv.pdf"}}`), nil
	})
	got, err := client.InvoiceURL(t.Context(), billing.Reference{Scope: client.Scope(), ID: testTxnID})
	if err != nil || got != "https://invoices.example.test/inv.pdf" {
		t.Fatalf("url=%q err=%v", got, err)
	}
	client = newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"url":"http://insecure.example.test/inv.pdf"}}`), nil
	})
	if _, err := client.InvoiceURL(t.Context(), billing.Reference{Scope: client.Scope(), ID: testTxnID}); err == nil {
		t.Fatal("non-https invoice link accepted")
	}
	if _, err := client.InvoiceURL(t.Context(), billing.Reference{Scope: client.Scope(), ID: "nope"}); err == nil {
		t.Fatal("bad reference accepted")
	}
}
