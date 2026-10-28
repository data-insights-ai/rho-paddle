package paddle_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/provider"

	paddle "github.com/data-insights-ai/rho-paddle"
)

// Shaped like the sandbox's adjustment.updated (identifiers made up).
const adjustmentEvent = `{"event_id":"evt_adj","event_type":"adjustment.updated","occurred_at":"2026-10-05T06:30:01.227162Z","notification_id":"ntf_1",
"data":{"id":"adj_1","action":"refund","type":"partial","transaction_id":"txn_1","subscription_id":null,"customer_id":"ctm_1",
"reason":"error","currency_code":"EUR","status":"approved",
"items":[{"id":"adjitm_1","item_id":"txnitm_2","type":"partial","amount":"1190","proration":null,"totals":{"subtotal":"1000","tax":"190","total":"1190"}}],
"totals":{"currency_code":"EUR","earnings":"900","fee":"100","retained_fee":"100","subtotal":"1000","tax":"190","total":"1190"},
"payout_totals":null,"tax_rates_used":[],"created_at":"2026-10-05T06:19:35.238102Z","updated_at":"2026-10-05T06:30:01.227162Z"}}`

func TestAdjustmentsBecomeEvents(t *testing.T) {
	e, err := paddle.Decode([]byte(adjustmentEvent))
	if err != nil || e.Kind != provider.PaymentAdjusted || e.Adjustment == nil {
		t.Fatalf("%+v %v", e, err)
	}
	a := e.Adjustment
	if a.ID != "adj_1" || a.Payment != "txn_1" || a.Action != provider.Refund || a.Status != provider.AdjustmentApproved ||
		a.Total != 1190 || a.Currency != "EUR" || len(a.Items) != 1 || a.Items[0].Line != "txnitm_2" || a.Items[0].Full || a.Items[0].Total != 1190 {
		t.Errorf("adjustment %+v", a)
	}
	for _, kind := range []string{"adjustment.created"} {
		body := strings.Replace(adjustmentEvent, "adjustment.updated", kind, 1)
		if e, err := paddle.Decode([]byte(body)); err != nil || e.Kind != provider.PaymentAdjusted {
			t.Errorf("%s: %+v %v", kind, e, err)
		}
	}
}

// Shaped like a sandbox transaction whose collection failed.
const failedPayment = `{"event_id":"evt_fail","event_type":"transaction.payment_failed","occurred_at":"2026-10-20T10:00:00Z",
"data":{"id":"txn_2","status":"past_due","customer_id":"ctm_1","subscription_id":"sub_1","currency_code":"EUR","billed_at":"2026-10-20T09:59:00Z",
"custom_data":{"reference":"org-1"},"items":[{"quantity":1,"price":{"id":"pri_c"}}],"details":{"totals":{"grand_total":"23800"}},
"payments":[{"payment_attempt_id":"pa_1","status":"error","error_code":"declined","created_at":"2026-10-20T09:59:30Z"}]}}`

func TestFailedAndCanceledPaymentsBecomeEvents(t *testing.T) {
	e, err := paddle.Decode([]byte(failedPayment))
	if err != nil || e.Kind != provider.PaymentFailed || e.Payment == nil {
		t.Fatalf("%+v %v", e, err)
	}
	if p := e.Payment; p.ID != "txn_2" || p.Subscription != "sub_1" || p.Failure != "declined" || p.Reference != "org-1" {
		t.Errorf("failed payment %+v", p)
	}
	canceled := strings.NewReplacer("transaction.payment_failed", "transaction.canceled", `"past_due"`, `"canceled"`).Replace(failedPayment)
	e, err = paddle.Decode([]byte(canceled))
	if err != nil || e.Kind != provider.PaymentCanceled || e.Payment == nil || e.Payment.ID != "txn_2" {
		t.Fatalf("canceled: %+v %v", e, err)
	}
}

func TestWebhooksDeliverAdjustments(t *testing.T) {
	c, _ := client(t, nil)
	var got []provider.Event
	h := c.Webhooks(func(_ context.Context, e provider.Event) error {
		got = append(got, e)
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/webhooks/paddle", strings.NewReader(adjustmentEvent))
	req.Header.Set("Paddle-Signature", sign(adjustmentEvent, time.Now()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || len(got) != 1 || got[0].Kind != provider.PaymentAdjusted {
		t.Fatalf("status %d, delivered %+v", rec.Code, got)
	}
}
