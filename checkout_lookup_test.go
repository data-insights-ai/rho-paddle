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
