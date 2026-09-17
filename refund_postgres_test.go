package paddle_test

import (
	"context"
	"errors"
	"fmt"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestRefundDispatchPostgresAppliesBoundPartialAndUnknownCreate(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "refund-dispatch")
	adjustmentID := "adj_abcdefghijklmnopqrstuvwxyz"
	operation := "refund-operation-dispatch"
	var posts int
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/adjustments" {
			t.Fatalf("unexpected %s %s", req.Method, req.URL)
		}
		posts++
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
		if body.Action != "refund" || body.TransactionID != f.binding.TransactionID || body.Reason != operation || body.Items[0].Amount != "40" {
			t.Fatalf("create body=%#v", body)
		}
		return jsonResponse(http.StatusCreated, `{"data":`+listedAdjustmentJSON(adjustmentID, "refund", "approved", "partial", f.binding.TransactionID, f.binding.CustomerID, operation, f.binding.Lines[0].ProviderLineID, "40", "0", "40")+`}`), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := paddle.NewRefundService(paddle.RefundServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanRefund(t.Context(), paddle.RefundDispatch{
		Account: f.account, IntentID: f.intent.ID, Operation: operation, TransactionID: f.binding.TransactionID,
		QuoteFingerprint: f.intent.QuoteFingerprint, Customer: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID},
		Lines: []paddle.RefundDispatchLine{{QuoteLineID: "line", Gross: 40, Tax: 0}},
		Items: []paddle.RefundDispatchItem{{ProviderLineID: f.binding.Lines[0].ProviderLineID, Type: "partial", Amount: "40"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "refund-worker", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	got, err := service.Dispatch(t.Context(), claim)
	if err != nil || got.Reference.ID != adjustmentID || posts != 1 {
		t.Fatalf("dispatch=%+v err=%v posts=%d", got, err, posts)
	}
	if _, err := service.Dispatch(t.Context(), claim); err == nil || posts != 1 {
		t.Fatalf("resent refund err=%v posts=%d", err, posts)
	}

	processor, err := paddle.NewAdjustmentProcessor(paddle.AdjustmentProcessorConfig{Merchant: "merchant-test", Environment: paddle.Sandbox, Repository: f.store.Queue(), Now: func() time.Time { return webhookNow.Add(time.Minute) }, PolicyVersion: "refund-v1", CreditPolicy: purchase.CreditRefundProportional})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(fmt.Sprintf(`{"event_id":"evt_abcdefghijklmnopqrstuvwxy1","event_type":"adjustment.updated","occurred_at":%q,"data":{"id":%q,"action":"refund","status":"approved","type":"partial","transaction_id":%q,"customer_id":%q,"currency_code":"USD","reason":%q,"created_at":%q,"updated_at":%q,"totals":{"subtotal":"40","tax":"0","total":"40","currency_code":"USD"},"items":[{"id":"adjitm_abcdefghijklmnopqrstuvwxyz","type":"partial","item_id":%q,"totals":{"subtotal":"40","tax":"0","total":"40"}}]}}`,
		webhookNow.Format(time.RFC3339Nano), adjustmentID, f.binding.TransactionID, f.binding.CustomerID, operation, webhookNow.Add(-2*time.Second).Format(time.RFC3339Nano), webhookNow.Add(-time.Second).Format(time.RFC3339Nano), f.binding.Lines[0].ProviderLineID))
	processAdjustmentBody(t, &f, processor, body, "refund-inbound-worker")
	assertPaddleCreditBalance(t, f, 60)
	delivery, err := f.store.Queue().Outbox(t.Context(), f.account, client.Scope(), plan.Message.ID)
	if err != nil || string(delivery.State) != string(integration.OutboxCompleted) || delivery.ProviderReference != adjustmentID {
		t.Fatalf("outbox=%+v err=%v", delivery, err)
	}
}

func TestRefundDispatchPostgresTimeoutStaysUnknownWithoutResend(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "refund-timeout")
	var posts int
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(*http.Request) (*http.Response, error) {
		posts++
		return nil, context.DeadlineExceeded
	})}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := paddle.NewRefundService(paddle.RefundServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanRefund(t.Context(), paddle.RefundDispatch{
		Account: f.account, IntentID: f.intent.ID, Operation: "refund-timeout-operation", TransactionID: f.binding.TransactionID,
		QuoteFingerprint: f.intent.QuoteFingerprint, Customer: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID},
		Lines: []paddle.RefundDispatchLine{{QuoteLineID: "line", Gross: 100, Tax: 0}},
		Items: []paddle.RefundDispatchItem{{ProviderLineID: f.binding.Lines[0].ProviderLineID, Type: "full"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "refund-timeout-worker", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := service.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrRefundUnresolved) || posts != 1 {
		t.Fatalf("timeout err=%v posts=%d", err, posts)
	}
	delivery, err := f.store.Queue().Outbox(t.Context(), f.account, client.Scope(), plan.Message.ID)
	if err != nil || string(delivery.State) != string(integration.OutboxUnknown) {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
	if _, err := service.Dispatch(t.Context(), claim); err == nil || posts != 1 {
		t.Fatalf("blind retry err=%v posts=%d", err, posts)
	}
	assertPaddleCreditBalance(t, f, 100)
}

func TestRefundRecoveryPostgresResolvesLostCreateByReason(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "refund-recover")
	adjustmentID := "adj_zyxwvutsrqponmlkjihgfedcba"
	operation := "refund-recover-operation"
	var posts int
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			posts++
			return nil, context.DeadlineExceeded
		}
		if req.URL.Path != "/adjustments" {
			t.Fatalf("lookup path=%s", req.URL)
		}
		return jsonResponse(http.StatusOK, fmt.Sprintf(`{"data":[%s],"meta":{"pagination":{"has_more":false,"next":"https://sandbox-api.paddle.com/adjustments?after=%s"}}}`,
			listedAdjustmentJSON(adjustmentID, "refund", "approved", "full", f.binding.TransactionID, f.binding.CustomerID, operation, f.binding.Lines[0].ProviderLineID, "100", "0", "100"), adjustmentID)), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := paddle.NewRefundService(paddle.RefundServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanRefund(t.Context(), paddle.RefundDispatch{
		Account: f.account, IntentID: f.intent.ID, Operation: operation, TransactionID: f.binding.TransactionID,
		QuoteFingerprint: f.intent.QuoteFingerprint, Customer: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID},
		Lines: []paddle.RefundDispatchLine{{QuoteLineID: "line", Gross: 100, Tax: 0}},
		Items: []paddle.RefundDispatchItem{{ProviderLineID: f.binding.Lines[0].ProviderLineID, Type: "full"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "refund-lost-worker", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := service.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrRefundUnresolved) {
		t.Fatalf("lost create err=%v", err)
	}
	got, err := service.RecoverRefund(t.Context(), f.account, plan.Message.ID, "refund-resumer")
	if err != nil || got.Reference.ID != adjustmentID || posts != 1 {
		t.Fatalf("recovered=%+v err=%v posts=%d", got, err, posts)
	}
	delivery, err := f.store.Queue().Outbox(t.Context(), f.account, client.Scope(), plan.Message.ID)
	if err != nil || string(delivery.State) != string(integration.OutboxCompleted) {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
}
