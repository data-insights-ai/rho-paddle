package paddle_test

import (
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"sync/atomic"
	"testing"

	paddle "github.com/data-insights-ai/rho-paddle"
)

const testAdjID = "adj_0123456789abcdefghijklmnop"
const testAdjItemID = "adjitm_0123456789abcdefghijklmnop"
const testTxnItemID = "txnitm_0123456789abcdefghijklmnop"

func TestCreateAdjustmentRefundWireMapping(t *testing.T) {
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodPost, "https://api.paddle.com/adjustments")
		var body struct {
			Action        string `json:"action"`
			Type          string `json:"type"`
			TransactionID string `json:"transaction_id"`
			Reason        string `json:"reason"`
			Items         []struct {
				ItemID string `json:"item_id"`
				Type   string `json:"type"`
				Amount string `json:"amount"`
			} `json:"items"`
		}
		decodeRequest(t, req, &body)
		if body.Action != "refund" || body.Type != "partial" || body.TransactionID != testTxnID || body.Reason != testOperation || len(body.Items) != 1 || body.Items[0].Amount != "50" {
			t.Fatalf("body=%#v", body)
		}
		return jsonResponse(http.StatusCreated, adjustmentJSON(testAdjID, "refund", "pending_approval", "partial", testTxnID, testCustomerID, testOperation, "50", "0", "50")), nil
	})
	got, err := client.CreateAdjustment(t.Context(), paddle.AdjustmentCreate{
		Action: "refund", Type: "partial", Transaction: billing.Reference{Scope: client.Scope(), ID: testTxnID}, Reason: testOperation,
		Items: []paddle.AdjustmentCreateItem{{TransactionItemID: testTxnItemID, Type: "partial", Amount: "50"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != testAdjID || got.Status != "pending_approval" || got.Gross != 50 || got.Reason != testOperation {
		t.Fatalf("adjustment=%#v", got)
	}
}

func TestCreateAdjustmentFullRefundOmitsItems(t *testing.T) {
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodPost, "https://sandbox-api.paddle.com/adjustments")
		var body struct {
			Action        string `json:"action"`
			Type          string `json:"type"`
			TransactionID string `json:"transaction_id"`
			Reason        string `json:"reason"`
			Items         []struct {
				ItemID string `json:"item_id"`
			} `json:"items"`
		}
		decodeRequest(t, req, &body)
		if body.Action != "refund" || body.Type != "full" || body.TransactionID != testTxnID || body.Reason != "error" || len(body.Items) != 0 {
			t.Fatalf("body=%#v", body)
		}
		return jsonResponse(http.StatusCreated, adjustmentJSON(testAdjID, "refund", "approved", "full", testTxnID, testCustomerID, "error", "100", "0", "100")), nil
	})
	got, err := client.CreateAdjustment(t.Context(), paddle.AdjustmentCreate{
		Action: "refund", Type: "full", Transaction: billing.Reference{Scope: client.Scope(), ID: testTxnID}, Reason: "error",
		Items: []paddle.AdjustmentCreateItem{{TransactionItemID: testTxnItemID, Type: "full"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != testAdjID || got.Type != "full" || got.Action != "refund" {
		t.Fatalf("adjustment=%#v", got)
	}
}

func TestCreateAdjustmentCreditFailsClosedBeforeMutation(t *testing.T) {
	var calls atomic.Int32
	client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not create credit")
	})
	_, err := client.CreateAdjustment(t.Context(), paddle.AdjustmentCreate{
		Action: "credit", Type: "full", Transaction: billing.Reference{Scope: client.Scope(), ID: testTxnID}, Reason: testOperation,
		Items: []paddle.AdjustmentCreateItem{{TransactionItemID: testTxnItemID, Type: "full"}},
	})
	if capabilityReason(err) != "provider_credit_adjustment_unresolved" || calls.Load() != 0 {
		t.Fatalf("credit error=%v calls=%d", err, calls.Load())
	}
}

func TestAdjustmentGetByID(t *testing.T) {
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodGet, "https://api.paddle.com/adjustments/"+testAdjID)
		return jsonResponse(http.StatusOK, adjustmentJSON(testAdjID, "refund", "approved", "full", testTxnID, testCustomerID, testOperation, "100", "0", "100")), nil
	})
	got, err := client.Adjustment(t.Context(), billing.Reference{Scope: client.Scope(), ID: testAdjID})
	if err != nil || got.Reference.ID != testAdjID || got.Transaction.ID != testTxnID || got.Status != "approved" {
		t.Fatalf("adjustment=%#v err=%v", got, err)
	}
}

func TestAdjustmentsLookupPaginationAndFilters(t *testing.T) {
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		want := "https://sandbox-api.paddle.com/adjustments?action=refund&after=adj_0123456789abcdefghijklmnoa&customer_id=" + testCustomerID + "&order_by=id%5BASC%5D&per_page=50&status=approved&transaction_id=" + testTxnID
		if req.Method != http.MethodGet || req.URL.String() != want {
			t.Fatalf("request=%s %s", req.Method, req.URL)
		}
		return jsonResponse(http.StatusOK, `{"data":[`+innerAdjustmentJSON(testAdjID, "refund", "approved", "full", testTxnID, testCustomerID, testOperation, "100", "0", "100")+`],"meta":{"pagination":{"has_more":false,"next":"https://sandbox-api.paddle.com/adjustments?after=`+testAdjID+`"}}}`), nil
	})
	page, err := client.Adjustments(t.Context(), paddle.AdjustmentLookup{
		Transaction: billing.Reference{Scope: client.Scope(), ID: testTxnID},
		Customer:    billing.Reference{Scope: client.Scope(), ID: testCustomerID},
		Action:      "refund", Status: "approved", After: "adj_0123456789abcdefghijklmnoa",
	})
	if err != nil || len(page.Items) != 1 || page.HasMore || page.Items[0].Reference.ID != testAdjID {
		t.Fatalf("page=%#v err=%v", page, err)
	}
}

func adjustmentJSON(id, action, status, kind, txn, customer, reason, subtotal, tax, total string) string {
	return `{"data":` + innerAdjustmentJSON(id, action, status, kind, txn, customer, reason, subtotal, tax, total) + `}`
}

func innerAdjustmentJSON(id, action, status, kind, txn, customer, reason, subtotal, tax, total string) string {
	return listedAdjustmentJSON(id, action, status, kind, txn, customer, reason, testTxnItemID, subtotal, tax, total)
}

func listedAdjustmentJSON(id, action, status, kind, txn, customer, reason, itemID, subtotal, tax, total string) string {
	return `{"id":"` + id + `","action":"` + action + `","status":"` + status + `","type":"` + kind + `","transaction_id":"` + txn + `","customer_id":"` + customer + `","currency_code":"USD","reason":"` + reason + `","created_at":"2026-09-16T08:00:00Z","updated_at":"2026-09-16T08:00:01Z","totals":{"subtotal":"` + subtotal + `","tax":"` + tax + `","total":"` + total + `","currency_code":"USD"},"items":[{"id":"` + testAdjItemID + `","item_id":"` + itemID + `","type":"` + kind + `","totals":{"subtotal":"` + subtotal + `","tax":"` + tax + `","total":"` + total + `"}}]}`
}
