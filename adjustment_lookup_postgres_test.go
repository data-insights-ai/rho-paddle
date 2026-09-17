package paddle_test

import (
	"fmt"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestAdjustmentProcessorPostgresReverseObjectLooksUpOriginal(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "adjustment-reverse-lookup")
	originalID := "adj_abcdefghijklmnopqrstuvwxyz"
	createdAt := webhookNow.Add(-2 * time.Second)
	var listed int
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/adjustments" {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		}
		listed++
		query := req.URL.Query()
		if query.Get("transaction_id") != f.binding.TransactionID || query.Get("action") != "chargeback" || query.Get("status") != "reversed" {
			t.Fatalf("lookup query=%s", req.URL.RawQuery)
		}
		return jsonResponse(http.StatusOK, fmt.Sprintf(`{"data":[%s],"meta":{"pagination":{"has_more":false,"next":"https://sandbox-api.paddle.com/adjustments?after=%s"}}}`,
			listedAdjustmentJSON(originalID, "chargeback", "reversed", "full", f.binding.TransactionID, f.binding.CustomerID, "chargeback", f.binding.Lines[0].ProviderLineID, "100", "0", "100"), originalID)), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	processor, err := paddle.NewAdjustmentProcessor(paddle.AdjustmentProcessorConfig{Merchant: "merchant-test", Environment: paddle.Sandbox, Repository: f.store.Queue(), Client: client, Now: func() time.Time { return webhookNow.Add(time.Minute) }, PolicyVersion: "reverse-lookup-v1", CreditPolicy: purchase.CreditRefundFullOnly})
	if err != nil {
		t.Fatal(err)
	}
	body := paddleDisputeAdjustmentBody(disputeAdjustmentWireInput{
		EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.created", Action: "chargeback_reverse", Status: "approved",
		AdjustmentID: "adj_zyxwvutsrqponmlkjihgfedcba", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID,
		ItemID: f.binding.Lines[0].ProviderLineID, CreatedAt: createdAt, UpdatedAt: webhookNow, OccurredAt: webhookNow,
	})
	processAdjustmentBody(t, &f, processor, body, "reverse-lookup-worker")
	if listed != 1 {
		t.Fatalf("list calls=%d", listed)
	}
	assertPaddleCreditBalance(t, f, 0)
	assertPaddleRecoveryTotal(t, f, 100)
	dispute, err := purchase.New(f.store.Purchases(), func() time.Time { return webhookNow.Add(time.Minute) }).Dispute(t.Context(), f.account, f.intent.Scope, originalID)
	if err != nil || dispute.Status != purchase.DisputeWon {
		t.Fatalf("looked-up dispute=%+v err=%v", dispute, err)
	}
	assertAdjustmentAbsent(t, &f, "adj_zyxwvutsrqponmlkjihgfedcba")
}

func TestPortalSessionIsEphemeralHandoff(t *testing.T) {
	client := newClient(t, paddle.Live, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodPost, "https://api.paddle.com/customers/"+testCustomerID+"/portal-sessions")
		return jsonResponse(http.StatusCreated, `{"data":{"customer_id":"`+testCustomerID+`","urls":{"general":{"overview":"https://portal.example.test/overview"},"subscriptions":[]}}}`), nil
	})
	got, err := client.CreatePortalSession(t.Context(), paddle.CustomerReference{Account: testAccount, Reference: billing.Reference{Scope: client.Scope(), ID: testCustomerID}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.OverviewURL != "https://portal.example.test/overview" || got.Customer.Reference.ID != testCustomerID {
		t.Fatalf("portal=%#v", got)
	}
}
