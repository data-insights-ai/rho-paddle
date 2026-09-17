package paddle_test

import (
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestAdjustmentProcessorPostgresTaxOnlyRefundRetainsTaxAndCreditBalance(t *testing.T) {
	f := newWorkflowMoneyFixture(t)
	paid := workflowPaymentBody(workflowPayment{
		EventID: "evt_aaaaaaaaaaaaaaaaaaaaaaaaaa", EventType: "transaction.completed", OccurredAt: webhookNow,
		Status: "completed", AccountCustomer: f.binding.CustomerID, Total: "480", Tax: "80",
		Lines: []workflowLine{
			{ID: "txnitm_zyxwvutsrqponmlkjihgfedcba", PriceID: "pri_zyxwvutsrqponmlkjihgfedcba", Quantity: 1, Total: "240", Tax: "40"},
			{ID: "txnitm_abcdefghijklmnopqrstuvwxyz", PriceID: "pri_abcdefghijklmnopqrstuvwxyz", Quantity: 2, Total: "240", Tax: "40"},
		},
		Attempts: []workflowAttempt{{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Status: "captured", Amount: "480", CreatedAt: webhookNow.Add(-2 * time.Second), CapturedAt: webhookNow.Add(-time.Second)}},
	})
	processWorkflowBody(t, &f, paid, "tax-payment-worker")
	before, err := credit.New(f.store.Credits(), func() time.Time { return webhookNow.Add(time.Minute) }).Balance(t.Context(), f.account, "credits", "")
	if err != nil || before.Available != 300 {
		t.Fatalf("before refund balance=%+v err=%v, want 300", before, err)
	}

	body := paddleAdjustmentBody(paddleAdjustmentInput{
		EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.updated", OccurredAt: webhookNow.Add(3 * time.Second),
		AdjustmentID: "adj_abcdefghijklmnopqrstuvwxyz", Status: "approved", Type: "partial", ItemType: "tax",
		TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID, ItemID: "txnitm_zyxwvutsrqponmlkjihgfedcba",
		Total: "40", Tax: "40", CreatedAt: webhookNow.Add(time.Second), UpdatedAt: webhookNow.Add(2 * time.Second),
	})
	processor := newAdjustmentProcessor(t, f)
	processAdjustmentBody(t, &f, processor, body, "tax-adjustment-worker")

	var record purchase.AdjustmentRecord
	if err := f.store.Purchases().WithinAccount(t.Context(), f.account, func(tx purchase.Tx) error {
		var err error
		record, err = tx.ProviderAdjustment(t.Context(), f.intent.Scope, "adj_abcdefghijklmnopqrstuvwxyz")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !record.Result.Applied || len(record.Input.Lines) != 1 || record.Input.Lines[0].Gross != 40 || record.Input.Lines[0].Tax != 40 {
		t.Fatalf("adjustment=%+v, want applied tax-only gross=40 tax=40", record)
	}
	var state purchase.AdjustmentState
	if err := f.store.Purchases().WithinAccount(t.Context(), f.account, func(tx purchase.Tx) error {
		var err error
		state, err = tx.AdjustmentState(t.Context(), f.intent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(state.Lines) != 1 || state.Lines[0].RefundedGross != 40 || state.Lines[0].RefundedTax != 40 {
		t.Fatalf("adjustment state=%+v, want tax-bearing totals retained", state)
	}
	after, err := credit.New(f.store.Credits(), func() time.Time { return webhookNow.Add(time.Minute) }).Balance(t.Context(), f.account, "credits", "")
	if err != nil || after.Available != before.Available {
		t.Fatalf("after refund balance=%+v err=%v, want unchanged full-only credit balance", after, err)
	}
	if _, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "tax-adjustment-replay", time.Now().UTC(), time.Minute); err != nil || ok {
		t.Fatalf("unexpected pending replay claim ok=%v err=%v", ok, err)
	}
}
