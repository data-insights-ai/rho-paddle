package paddle_test

import (
	"context"
	"errors"
	"fmt"
	billing "github.com/data-insights-ai/rho-billing"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestAdjustmentProcessorPostgresChargebackRecoveryAndFrozenPolicyReplay(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "adjustment-chargeback-recovery")
	processor := adjustmentProcessorWithPolicy(t, f, "chargeback-policy-v1", purchase.CreditRefundFullOnly)
	createdAt := webhookNow.Add(-2 * time.Second)
	adjustmentID := "adj_abcdefghijklmnopqrstuvwxyz"
	approved := paddleDisputeAdjustmentBody(disputeAdjustmentWireInput{
		EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.created", Action: "chargeback", Status: "approved",
		AdjustmentID: adjustmentID, TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID,
		ItemID: f.binding.Lines[0].ProviderLineID, CreatedAt: createdAt, UpdatedAt: webhookNow.Add(-time.Second), OccurredAt: webhookNow,
	})
	processAdjustmentBody(t, &f, processor, approved, "chargeback-approved-worker")
	assertPaddleCreditBalance(t, f, 0)

	reversed := paddleDisputeAdjustmentBody(disputeAdjustmentWireInput{
		EventID: "evt_mnopqrstuvwxyzabcdefghijkl", EventType: "adjustment.updated", Action: "chargeback", Status: "reversed",
		AdjustmentID: adjustmentID, TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID,
		ItemID: f.binding.Lines[0].ProviderLineID, CreatedAt: createdAt, UpdatedAt: webhookNow, OccurredAt: webhookNow,
	})
	processAdjustmentBody(t, &f, processor, reversed, "chargeback-reversed-worker")
	assertPaddleCreditBalance(t, f, 0)
	assertPaddleRecoveryTotal(t, f, 100)
	dispute, err := purchase.New(f.store.Purchases(), func() time.Time { return webhookNow.Add(time.Minute) }).Dispute(t.Context(), f.account, f.intent.Scope, adjustmentID)
	if err != nil || dispute.Status != purchase.DisputeWon || !dispute.StatusOccurredAt.Equal(webhookNow) {
		t.Fatalf("reversed dispute=%+v err=%v", dispute, err)
	}

	changedPolicy := adjustmentProcessorWithPolicy(t, f, "chargeback-policy-v2", purchase.CreditRefundProportional)
	replayedStatus := paddleDisputeAdjustmentBody(disputeAdjustmentWireInput{
		EventID: "evt_aaaaaaaaaaaaaaaaaaaaaaaaaa", EventType: "adjustment.updated", Action: "chargeback", Status: "reversed",
		AdjustmentID: adjustmentID, TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID,
		ItemID: f.binding.Lines[0].ProviderLineID, CreatedAt: createdAt, UpdatedAt: webhookNow, OccurredAt: webhookNow,
	})
	processAdjustmentBody(t, &f, changedPolicy, replayedStatus, "chargeback-policy-replay-worker")
	assertPaddleCreditBalance(t, f, 0)
	assertPaddleRecoveryTotal(t, f, 100)
	record, err := purchase.New(f.store.Purchases(), func() time.Time { return webhookNow.Add(time.Minute) }).Adjustment(t.Context(), f.account, paddleAdjustmentOperationID(t, f, adjustmentID))
	if err != nil || record.Input.PolicyVersion != "chargeback-policy-v1" || record.Input.CreditPolicy != purchase.CreditRefundFullOnly || !record.Input.OccurredAt.Equal(createdAt) {
		t.Fatalf("frozen adjustment=%+v err=%v", record, err)
	}
}

func TestAdjustmentProcessorPostgresOriginalReversedBackfillsDebitAndRecovery(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "adjustment-chargeback-backfill")
	processor := adjustmentProcessorWithPolicy(t, f, "chargeback-backfill-policy", purchase.CreditRefundFullOnly)
	body := paddleDisputeAdjustmentBody(disputeAdjustmentWireInput{
		EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.updated", Action: "chargeback", Status: "reversed",
		AdjustmentID: "adj_zyxwvutsrqponmlkjihgfedcba", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID,
		ItemID: f.binding.Lines[0].ProviderLineID, CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow, OccurredAt: webhookNow,
	})
	processAdjustmentBody(t, &f, processor, body, "chargeback-backfill-worker")
	assertPaddleCreditBalance(t, f, 0)
	assertPaddleRecoveryTotal(t, f, 100)
	dispute, err := purchase.New(f.store.Purchases(), func() time.Time { return webhookNow.Add(time.Minute) }).Dispute(t.Context(), f.account, f.intent.Scope, "adj_zyxwvutsrqponmlkjihgfedcba")
	if err != nil || dispute.Status != purchase.DisputeWon || !dispute.StatusOccurredAt.Equal(webhookNow) {
		t.Fatalf("backfilled dispute=%+v err=%v", dispute, err)
	}
}

func TestAdjustmentProcessorPostgresChargebackWarningApprovedThenReversed(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "adjustment-chargeback-warning")
	processor := adjustmentProcessorWithPolicy(t, f, "chargeback-warning-policy", purchase.CreditRefundFullOnly)
	adjustmentID := "adj_abcdefghijklmnopqrstuvwxyz"
	createdAt := webhookNow.Add(-2 * time.Second)
	approved := paddleDisputeAdjustmentBody(disputeAdjustmentWireInput{
		EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.created", Action: "chargeback_warning", Status: "approved",
		AdjustmentID: adjustmentID, TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID,
		ItemID: f.binding.Lines[0].ProviderLineID, CreatedAt: createdAt, UpdatedAt: webhookNow.Add(-time.Second), OccurredAt: webhookNow,
	})
	processAdjustmentBody(t, &f, processor, approved, "chargeback-warning-approved-worker")
	assertPaddleCreditBalance(t, f, 0)
	dispute, err := purchase.New(f.store.Purchases(), func() time.Time { return webhookNow.Add(time.Minute) }).Dispute(t.Context(), f.account, f.intent.Scope, adjustmentID)
	if err != nil || dispute.Status != purchase.DisputeWarning || !dispute.StatusOccurredAt.Equal(createdAt) {
		t.Fatalf("warning dispute=%+v err=%v", dispute, err)
	}

	reversed := paddleDisputeAdjustmentBody(disputeAdjustmentWireInput{
		EventID: "evt_mnopqrstuvwxyzabcdefghijkl", EventType: "adjustment.updated", Action: "chargeback_warning", Status: "reversed",
		AdjustmentID: adjustmentID, TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID,
		ItemID: f.binding.Lines[0].ProviderLineID, CreatedAt: createdAt, UpdatedAt: webhookNow, OccurredAt: webhookNow,
	})
	processAdjustmentBody(t, &f, processor, reversed, "chargeback-warning-reversed-worker")
	dispute, err = purchase.New(f.store.Purchases(), func() time.Time { return webhookNow.Add(time.Minute) }).Dispute(t.Context(), f.account, f.intent.Scope, adjustmentID)
	if err != nil || dispute.Status != purchase.DisputeClosed || !dispute.StatusOccurredAt.Equal(webhookNow) {
		t.Fatalf("closed warning dispute=%+v err=%v", dispute, err)
	}
	assertPaddleRecoveryTotal(t, f, 100)
	assertPaddleCreditBalance(t, f, 0)
}

func TestAdjustmentProcessorPostgresDisputeFailureRollsBackDebitEffectsAndInbox(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "adjustment-dispute-rollback")
	processor := adjustmentProcessorWithPolicy(t, f, "chargeback-rollback-policy", purchase.CreditRefundFullOnly)
	if _, err := f.db.ExecContext(t.Context(), `CREATE FUNCTION reject_paddle_dispute_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected dispute insert failure'; END $$; CREATE TRIGGER reject_paddle_dispute_insert BEFORE INSERT ON billing_purchase_disputes FOR EACH ROW EXECUTE FUNCTION reject_paddle_dispute_insert()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = f.db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS reject_paddle_dispute_insert ON billing_purchase_disputes; DROP FUNCTION IF EXISTS reject_paddle_dispute_insert()`)
	}()
	adjustmentID := "adj_zyxwvutsrqponmlkjihgfedcba"
	body := paddleDisputeAdjustmentBody(disputeAdjustmentWireInput{
		EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.created", Action: "chargeback", Status: "approved",
		AdjustmentID: adjustmentID, TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID,
		ItemID: f.binding.Lines[0].ProviderLineID, CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow, OccurredAt: webhookNow,
	})
	if _, err := integration.ReceiveWebhook(t.Context(), f.store.Queue(), f.verifier, body, signedWebhookHeaders(body), webhookScope()); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "chargeback-rollback-worker", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := processor.Process(t.Context(), claim); err == nil {
		t.Fatal("dispute insert failure was accepted")
	}
	assertPaddleCreditBalance(t, f, 100)
	assertPaddleRecoveryTotal(t, f, 0)
	var inboxState string
	if err := f.db.QueryRowContext(t.Context(), `SELECT state FROM billing_inbox WHERE account_id=$1 AND message_id=$2`, f.account, claim.Message.ID).Scan(&inboxState); err != nil || inboxState != "processing" {
		t.Fatalf("inbox state=%q err=%v, want processing", inboxState, err)
	}
	var adjustments, disputes int
	if err := f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM billing_purchase_adjustments WHERE account_id=$1 AND provider_adjustment_id=$2`, f.account, adjustmentID).Scan(&adjustments); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM billing_purchase_disputes WHERE account_id=$1 AND dispute_id=$2`, f.account, adjustmentID).Scan(&disputes); err != nil {
		t.Fatal(err)
	}
	if adjustments != 0 || disputes != 0 {
		t.Fatalf("rollback retained adjustments=%d disputes=%d", adjustments, disputes)
	}
}

func TestAdjustmentProcessorPostgresUnknownReverseObjectRemainsUnresolved(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "adjustment-reverse-unknown")
	processor := adjustmentProcessorWithPolicy(t, f, "chargeback-reverse-policy", purchase.CreditRefundFullOnly)
	body := paddleDisputeAdjustmentBody(disputeAdjustmentWireInput{
		EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.created", Action: "chargeback_reverse", Status: "approved",
		AdjustmentID: "adj_zyxwvutsrqponmlkjihgfedcba", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID,
		ItemID: f.binding.Lines[0].ProviderLineID, CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow, OccurredAt: webhookNow,
	})
	if _, err := integration.ReceiveWebhook(t.Context(), f.store.Queue(), f.verifier, body, signedWebhookHeaders(body), webhookScope()); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "chargeback-reverse-unknown-worker", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	err = processor.Process(t.Context(), claim)
	capability, ok := errors.AsType[*billing.CapabilityError](err)
	if !ok || capability.Capability.Support != billing.SupportUnresolved || capability.Capability.Reason != "original_adjustment_lookup_required" {
		t.Fatalf("reverse error=%v", err)
	}
	var state string
	if err := f.db.QueryRowContext(t.Context(), `SELECT state FROM billing_inbox WHERE account_id=$1 AND message_id=$2`, f.account, claim.Message.ID).Scan(&state); err != nil || state != "processing" {
		t.Fatalf("inbox state=%q err=%v, want processing", state, err)
	}
	assertPaddleRecoveryTotal(t, f, 0)
	assertPaddleCreditBalance(t, f, 100)
	var adjustments int
	if err := f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM billing_purchase_adjustments WHERE account_id=$1 AND provider_adjustment_id=$2`, f.account, "adj_zyxwvutsrqponmlkjihgfedcba").Scan(&adjustments); err != nil || adjustments != 0 {
		t.Fatalf("guessed reverse adjustment count=%d err=%v", adjustments, err)
	}
}

type disputeAdjustmentWireInput struct {
	EventID, EventType, Action, Status, AdjustmentID, TransactionID, CustomerID, ItemID string
	OccurredAt, CreatedAt, UpdatedAt                                                    time.Time
}

func paddleDisputeAdjustmentBody(in disputeAdjustmentWireInput) []byte {
	return []byte(fmt.Sprintf(`{"event_id":%q,"event_type":%q,"occurred_at":%q,"data":{"id":%q,"action":%q,"status":%q,"type":"full","transaction_id":%q,"customer_id":%q,"currency_code":"USD","created_at":%q,"updated_at":%q,"totals":{"subtotal":"100","tax":"0","total":"100","currency_code":"USD"},"items":[{"id":"adjitm_abcdefghijklmnopqrstuvwxyz","type":"full","item_id":%q,"totals":{"subtotal":"100","tax":"0","total":"100","currency_code":"USD"}}]}}`, in.EventID, in.EventType, in.OccurredAt.Format(time.RFC3339Nano), in.AdjustmentID, in.Action, in.Status, in.TransactionID, in.CustomerID, in.CreatedAt.Format(time.RFC3339Nano), in.UpdatedAt.Format(time.RFC3339Nano), in.ItemID))
}

func paidPaddleAdjustmentFixture(t *testing.T, suffix string) paymentFixture {
	t.Helper()
	f := newPaymentFixture(t, suffix, true)
	if err := f.process(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", "USD", f.binding.CustomerID, f.binding.Lines[0].ProviderLineID, f.binding.Lines[0].ProviderPriceID); err != nil {
		t.Fatal(err)
	}
	return f
}

func adjustmentProcessorWithPolicy(t *testing.T, f paymentFixture, version string, policy purchase.CreditRefundPolicy) *paddle.AdjustmentProcessor {
	t.Helper()
	processor, err := paddle.NewAdjustmentProcessor(paddle.AdjustmentProcessorConfig{Merchant: "merchant-test", Environment: paddle.Sandbox, Repository: f.store.Queue(), Now: func() time.Time { return webhookNow.Add(time.Minute) }, PolicyVersion: version, CreditPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func assertPaddleCreditBalance(t *testing.T, f paymentFixture, available int64) {
	t.Helper()
	balance, err := credit.New(f.store.Credits(), func() time.Time { return webhookNow.Add(time.Minute) }).Balance(t.Context(), f.account, "credits", "")
	if err != nil || balance.Available != available {
		t.Fatalf("balance=%+v err=%v, want available=%d", balance, err, available)
	}
}

func assertPaddleRecoveryTotal(t *testing.T, f paymentFixture, gross int64) {
	t.Helper()
	err := f.store.Purchases().WithinAccount(t.Context(), f.account, func(tx purchase.Tx) error {
		lines, err := tx.ChargebackRecoveries(t.Context(), f.intent.ID)
		if err == nil && ((gross == 0 && len(lines) != 0) || (gross != 0 && (len(lines) != 1 || lines[0].Gross != gross || lines[0].Tax != 0))) {
			t.Fatalf("recovery totals=%+v, want gross=%d", lines, gross)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func paddleAdjustmentOperationID(t *testing.T, f paymentFixture, providerID string) string {
	t.Helper()
	var id string
	if err := f.db.QueryRowContext(t.Context(), `SELECT adjustment_id FROM billing_purchase_adjustments WHERE account_id=$1 AND provider_adjustment_id=$2`, f.account, providerID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
