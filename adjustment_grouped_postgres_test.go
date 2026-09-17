package paddle_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestAdjustmentProcessorPostgresGroupedFullOriginalExpandsFunding(t *testing.T) {
	f := paidGroupedAdjustmentFixture(t, "adjustment-grouped-full")
	processor := newAdjustmentProcessor(t, f)
	adjustmentID := "adj_00000000000000000000000021"
	body := groupedAdjustmentBody(
		"evt_00000000000000000000000021", adjustmentID, f, "344", "31", "375",
	)
	processAdjustmentBody(t, &f, processor, body, "grouped-full-worker")

	var record purchase.AdjustmentRecord
	err := f.store.Purchases().WithinAccount(t.Context(), f.account, func(tx purchase.Tx) error {
		var err error
		record, err = tx.ProviderAdjustment(t.Context(), f.intent.Scope, adjustmentID)
		return err
	})
	if err != nil || !record.Result.Applied || len(record.Input.Lines) != 2 {
		t.Fatalf("grouped adjustment=%+v err=%v", record, err)
	}
	if record.Input.Lines[0] != (purchase.PaidLine{LineID: "a", Gross: 125, Tax: 10}) ||
		record.Input.Lines[1] != (purchase.PaidLine{LineID: "b", Gross: 250, Tax: 21}) {
		t.Fatalf("grouped lines=%+v", record.Input.Lines)
	}
	assertAdjustmentInboxState(t, &f, "evt_00000000000000000000000021", "processed")
	assertPaddleCreditBalance(t, f, 0)
}

func TestAdjustmentProcessorPostgresGroupedBoundPartialUsesOutboundAllocation(t *testing.T) {
	f := paidGroupedAdjustmentFixture(t, "adjustment-grouped-bound")
	operation := "grouped-bound-refund"
	adjustmentID := "adj_00000000000000000000000023"
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusCreated, `{"data":`+listedAdjustmentJSON(adjustmentID, "refund", "approved", "partial", f.binding.TransactionID, f.binding.CustomerID, operation, f.binding.Lines[0].ProviderLineID, "115", "10", "125")+`}`), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	refunds, err := paddle.NewRefundService(paddle.RefundServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refunds.PlanRefund(t.Context(), paddle.RefundDispatch{
		Account: f.account, IntentID: f.intent.ID, Operation: operation, TransactionID: f.binding.TransactionID,
		QuoteFingerprint: f.intent.QuoteFingerprint, Customer: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID},
		Lines: []paddle.RefundDispatchLine{{QuoteLineID: "a", Gross: 125, Tax: 10}},
		Items: []paddle.RefundDispatchItem{{ProviderLineID: f.binding.Lines[0].ProviderLineID, Type: "partial", Amount: "125"}},
	}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "grouped-bound-dispatch", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := refunds.Dispatch(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	processor := newAdjustmentProcessor(t, f)
	body := []byte(fmt.Sprintf(`{"event_id":"evt_00000000000000000000000023","event_type":"adjustment.updated","occurred_at":%q,"data":{"id":%q,"action":"refund","status":"approved","type":"partial","transaction_id":%q,"customer_id":%q,"currency_code":"USD","reason":%q,"created_at":%q,"updated_at":%q,"totals":{"subtotal":"115","tax":"10","total":"125","currency_code":"USD"},"items":[{"id":"adjitm_00000000000000000000000023","type":"partial","item_id":%q,"totals":{"subtotal":"115","tax":"10","total":"125"}}]}}`,
		webhookNow.Format(time.RFC3339Nano), adjustmentID, f.binding.TransactionID, f.binding.CustomerID, operation, webhookNow.Add(-2*time.Second).Format(time.RFC3339Nano), webhookNow.Add(-time.Second).Format(time.RFC3339Nano), f.binding.Lines[0].ProviderLineID))
	processAdjustmentBody(t, &f, processor, body, "grouped-bound-worker")
	var record purchase.AdjustmentRecord
	err = f.store.Purchases().WithinAccount(t.Context(), f.account, func(tx purchase.Tx) error {
		var readErr error
		record, readErr = tx.ProviderAdjustment(t.Context(), f.intent.Scope, adjustmentID)
		return readErr
	})
	if err != nil || !record.Result.Applied || len(record.Input.Lines) != 1 || record.Input.Lines[0] != (purchase.PaidLine{LineID: "a", Gross: 125, Tax: 10}) {
		t.Fatalf("bound grouped adjustment=%+v err=%v", record, err)
	}
}

func TestAdjustmentProcessorPostgresGroupedRemainingFullStaysUnresolved(t *testing.T) {
	f := paidGroupedAdjustmentFixture(t, "adjustment-grouped-remaining")
	processor := newAdjustmentProcessor(t, f)
	eventID := "evt_00000000000000000000000022"
	adjustmentID := "adj_00000000000000000000000022"
	body := groupedAdjustmentBody(eventID, adjustmentID, f, "100", "10", "110")
	claim := receiveAndClaimAdjustment(t, &f, body, "grouped-remaining-worker")
	err := processor.Process(t.Context(), claim)
	capability, ok := errors.AsType[*billing.CapabilityError](err)
	if !ok || capability.Support != billing.SupportUnresolved || capability.Reason != "grouped_adjustment_allocation_unresolved" {
		t.Fatalf("remaining grouped error=%v", err)
	}
	assertAdjustmentInboxState(t, &f, eventID, "processing")
	assertAdjustmentAbsent(t, &f, adjustmentID)
	assertPaddleCreditBalance(t, f, 300)
}

func paidGroupedAdjustmentFixture(t *testing.T, suffix string) paymentFixture {
	t.Helper()
	f := newPaymentFixture(t, suffix, false)
	service := purchase.New(f.store.Purchases(), func() time.Time { return f.clock })
	original, err := service.Quote(t.Context(), f.account, f.intent.QuoteID)
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(t.Context(), purchase.Price{
		Account: f.account, Revision: purchase.Revision{ID: "grouped-price-" + suffix, Version: 1},
		Offer: original.Lines[0].Offer.Revision, Currency: "USD", UnitAmount: 125,
		TaxTreatment: purchase.TaxInclusive, PublishedAt: f.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(t.Context(), purchase.QuoteInput{
		Account: f.account, ID: "grouped-quote-" + suffix, ValidUntil: f.clock.Add(time.Hour),
		Lines: []purchase.QuoteLineInput{{ID: "a", Price: price.Revision, Quantity: 1}, {ID: "b", Price: price.Revision, Quantity: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.intent, err = service.CreateIntent(t.Context(), purchase.IntentInput{
		Account: f.account, ID: "grouped-intent-" + suffix, Operation: "grouped-operation-" + suffix,
		QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: f.intent.Scope,
		Actor: "host", Reason: "grouped adjustment test", ExpiresAt: quote.ValidUntil,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.binding.IntentID = f.intent.ID
	f.binding.QuoteFingerprint = quote.Fingerprint()
	f.binding.Lines = []purchase.CollectionLine{{
		ProviderLineID: "txnitm_abcdefghijklmnopqrstuvwxyz", ProviderPriceID: "pri_abcdefghijklmnopqrstuvwxyz", Quantity: 3,
		Allocations: []purchase.CollectionAllocation{{QuoteLineID: "b", Quantity: 2}, {QuoteLineID: "a", Quantity: 1}},
	}}
	dispatchMergedCheckout(t, f)
	body := workflowPaymentBody(workflowPayment{
		EventID: "evt_00000000000000000000000020", EventType: "transaction.completed", OccurredAt: webhookNow,
		Status: "completed", AccountCustomer: f.binding.CustomerID, Total: "375", Tax: "31",
		Lines:    []workflowLine{{ID: f.binding.Lines[0].ProviderLineID, PriceID: f.binding.Lines[0].ProviderPriceID, Quantity: 3, Total: "375", Tax: "31"}},
		Attempts: []workflowAttempt{{ID: "20202020-2020-4020-8020-202020202020", Status: "captured", Amount: "375", CreatedAt: f.clock, CapturedAt: f.clock.Add(time.Second)}},
	})
	processWorkflowBody(t, &f, body, "grouped-adjustment-payment-worker")
	return f
}

func groupedAdjustmentBody(eventID, adjustmentID string, f paymentFixture, subtotal, tax, total string) []byte {
	createdAt := webhookNow.Add(-2 * time.Second)
	updatedAt := webhookNow.Add(-time.Second)
	return []byte(fmt.Sprintf(`{"event_id":%q,"event_type":"adjustment.updated","occurred_at":%q,"data":{"id":%q,"action":"refund","status":"approved","type":"full","transaction_id":%q,"customer_id":%q,"currency_code":"USD","created_at":%q,"updated_at":%q,"totals":{"subtotal":%q,"tax":%q,"total":%q,"currency_code":"USD"},"items":[{"id":"adjitm_00000000000000000000000021","type":"full","item_id":%q,"totals":{"subtotal":%q,"tax":%q,"total":%q}}]}}`,
		eventID, webhookNow.Format(time.RFC3339Nano), adjustmentID, f.binding.TransactionID, f.binding.CustomerID,
		createdAt.Format(time.RFC3339Nano), updatedAt.Format(time.RFC3339Nano), subtotal, tax, total,
		f.binding.Lines[0].ProviderLineID, subtotal, tax, total))
}

var _ = integration.Inbound
var _ billing.AccountID
