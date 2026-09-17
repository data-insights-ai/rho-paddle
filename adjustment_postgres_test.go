package paddle_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestAdjustmentProcessorPostgresApprovedRefundRevokesCredit(t *testing.T) {
	f := newPaymentFixture(t, "adjustment-approved", true)
	if err := f.process(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	body := paddleAdjustmentBody(paddleAdjustmentInput{EventID: "evt_0123456789abcdefghijklmnop", EventType: "adjustment.updated", OccurredAt: webhookNow, AdjustmentID: "adj_abcdefghijklmnopqrstuvwxyz", Status: "approved", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID, Total: "100", Tax: "0", CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow.Add(-time.Second)})
	processor := newAdjustmentProcessor(t, f)
	processAdjustmentBody(t, &f, processor, body, "adjustment-approved-worker")
	assertAdjustmentApplied(t, &f, "adj_abcdefghijklmnopqrstuvwxyz")
	balance, err := credit.New(f.store.Credits(), func() time.Time { return webhookNow.Add(time.Minute) }).Balance(t.Context(), f.account, "credits", "")
	if err != nil || balance.Available != 0 {
		t.Fatalf("credit balance=%+v err=%v, want fully revoked", balance, err)
	}
}

func TestAdjustmentProcessorPostgresPendingThenApprovedReplay(t *testing.T) {
	f := newPaymentFixture(t, "adjustment-pending", true)
	if err := f.process(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	processor := newAdjustmentProcessor(t, f)
	pending := paddleAdjustmentBody(paddleAdjustmentInput{EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.created", OccurredAt: webhookNow, AdjustmentID: "adj_zyxwvutsrqponmlkjihgfedcba", Status: "pending_approval", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID, Total: "100", Tax: "0", CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow.Add(-time.Second)})
	processAdjustmentBody(t, &f, processor, pending, "adjustment-pending-worker")
	assertAdjustmentAbsent(t, &f, "adj_zyxwvutsrqponmlkjihgfedcba")
	approved := paddleAdjustmentBody(paddleAdjustmentInput{EventID: "evt_0123456789abcdefghijklmnop", EventType: "adjustment.updated", OccurredAt: webhookNow, AdjustmentID: "adj_zyxwvutsrqponmlkjihgfedcba", Status: "approved", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID, Total: "100", Tax: "0", CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow.Add(-time.Second)})
	processAdjustmentBody(t, &f, processor, approved, "adjustment-pending-approved-worker")
	assertAdjustmentApplied(t, &f, "adj_zyxwvutsrqponmlkjihgfedcba")
	balance, err := credit.New(f.store.Credits(), func() time.Time { return webhookNow.Add(time.Minute) }).Balance(t.Context(), f.account, "credits", "")
	if err != nil || balance.Available != 0 {
		t.Fatalf("credit balance=%+v err=%v, want fully revoked", balance, err)
	}
	if _, err := integration.ReceiveWebhook(t.Context(), f.store.Queue(), f.verifier, approved, signedWebhookHeaders(approved), webhookScope()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "adjustment-pending-replay-worker", time.Now().UTC(), time.Minute); err != nil || ok {
		t.Fatalf("replay claim ok=%v err=%v, want no duplicate claim", ok, err)
	}
	balance, err = credit.New(f.store.Credits(), func() time.Time { return webhookNow.Add(time.Minute) }).Balance(t.Context(), f.account, "credits", "")
	if err != nil || balance.Available != 0 {
		t.Fatalf("replayed credit balance=%+v err=%v, want unchanged", balance, err)
	}
}

func TestAdjustmentProcessorPostgresPartialRefundsAccumulateWithoutDuplicateEffects(t *testing.T) {
	f := newPaymentFixture(t, "adjustment-partial-cumulative", true)
	if err := f.process(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	processor := newAdjustmentProcessor(t, f)
	first := paddleAdjustmentBody(paddleAdjustmentInput{EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.updated", OccurredAt: webhookNow, AdjustmentID: "adj_abcdefghijklmnopqrstuvwxyz", Status: "approved", Type: "partial", ItemType: "partial", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID, Total: "40", Tax: "0", CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow.Add(-time.Second)})
	processAdjustmentBody(t, &f, processor, first, "adjustment-partial-first")
	second := paddleAdjustmentBody(paddleAdjustmentInput{EventID: "evt_0123456789abcdefghijklmnop", EventType: "adjustment.updated", OccurredAt: webhookNow, AdjustmentID: "adj_zyxwvutsrqponmlkjihgfedcba", Status: "approved", Type: "partial", ItemType: "full", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID, Total: "60", Tax: "0", CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow.Add(-time.Second)})
	processAdjustmentBody(t, &f, processor, second, "adjustment-partial-second")
	var state purchase.AdjustmentState
	if err := f.store.Purchases().WithinAccount(t.Context(), f.account, func(tx purchase.Tx) error {
		var err error
		state, err = tx.AdjustmentState(t.Context(), f.intent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(state.Lines) != 1 || state.Lines[0].RefundedGross != 100 || state.Lines[0].RefundedTax != 0 {
		t.Fatalf("adjustment state=%+v, want cumulative refunded gross 100", state)
	}
	balance, err := credit.New(f.store.Credits(), func() time.Time { return webhookNow.Add(time.Minute) }).Balance(t.Context(), f.account, "credits", "")
	if err != nil || balance.Available != 0 {
		t.Fatalf("credit balance=%+v err=%v, want one final revocation", balance, err)
	}
	if _, err := integration.ReceiveWebhook(t.Context(), f.store.Queue(), f.verifier, second, signedWebhookHeaders(second), webhookScope()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "adjustment-partial-replay", time.Now().UTC(), time.Minute); err != nil || ok {
		t.Fatalf("replay claim ok=%v err=%v, want no duplicate claim", ok, err)
	}
}

func TestAdjustmentProcessorPostgresRejectedRefundFinishesInboxWithoutDebit(t *testing.T) {
	f := newPaymentFixture(t, "adjustment-rejected", true)
	if err := f.process(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	body := paddleAdjustmentBody(paddleAdjustmentInput{EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.updated", OccurredAt: webhookNow, AdjustmentID: "adj_abcdefghijklmnopqrstuvwxyz", Status: "rejected", Type: "full", ItemType: "full", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID, Total: "100", Tax: "0", CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow.Add(-time.Second)})
	processor := newAdjustmentProcessor(t, f)
	processAdjustmentBody(t, &f, processor, body, "adjustment-rejected-worker")
	assertPaymentEffects(t, &f)
	assertAdjustmentAbsent(t, &f, "adj_abcdefghijklmnopqrstuvwxyz")
	var state string
	if err := f.db.QueryRowContext(t.Context(), `SELECT state FROM billing_inbox WHERE account_id=$1 AND message_id=$2`, f.account, "evt_zyxwvutsrqponmlkjihgfedcba").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "processed" {
		t.Fatalf("inbox state=%q, want processed", state)
	}
}

func TestAdjustmentProcessorPostgresMissingFundingRetriesAfterPayment(t *testing.T) {
	f := newPaymentFixture(t, "adjustment-missing-funding", true)
	processor := newAdjustmentProcessor(t, f)
	body := paddleAdjustmentBody(paddleAdjustmentInput{EventID: "evt_abcdefghijklmnopqrstuvwxyz", EventType: "adjustment.updated", OccurredAt: webhookNow, AdjustmentID: "adj_zyxwvutsrqponmlkjihgfedcba", Status: "approved", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID, Total: "100", Tax: "0", CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow.Add(-time.Second)})
	if _, err := integration.ReceiveWebhook(t.Context(), f.store.Queue(), f.verifier, body, signedWebhookHeaders(body), webhookScope()); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "adjustment-missing-funding-worker", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := processor.Process(t.Context(), claim); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("missing funding error=%v, want not found", err)
	}
	assertNoPaymentEffects(t, &f)
	if err := f.receiveOnly(t, "evt_aaaaaaaaaaaaaaaaaaaaaaaaaa", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	paymentClaim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "payment-after-missing", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("payment claim ok=%v err=%v", ok, err)
	}
	if err := f.processor.Process(t.Context(), paymentClaim); err != nil {
		t.Fatal(err)
	}
	expireInboxClaim(t, f.db, "evt_abcdefghijklmnopqrstuvwxyz")
	retryClaim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "adjustment-missing-funding-retry", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("retry claim ok=%v err=%v", ok, err)
	}
	if err := processor.Process(t.Context(), retryClaim); err != nil {
		t.Fatal(err)
	}
	assertAdjustmentApplied(t, &f, "adj_zyxwvutsrqponmlkjihgfedcba")
}

func TestAdjustmentProcessorPostgresDeferredCommitRollsBackAndRetries(t *testing.T) {
	f := newPaymentFixture(t, "adjustment-commit", true)
	if err := f.process(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	body := paddleAdjustmentBody(paddleAdjustmentInput{EventID: "evt_zyxwvutsrqponmlkjihgfedcba", EventType: "adjustment.updated", OccurredAt: webhookNow, AdjustmentID: "adj_zyxwvutsrqponmlkjihgfedcba", Status: "approved", TransactionID: f.binding.TransactionID, CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID, Total: "100", Tax: "0", CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow.Add(-time.Second)})
	if _, err := f.db.ExecContext(t.Context(), `CREATE FUNCTION paddle_adjustment_commit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'paddle adjustment commit failure'; END $$; CREATE CONSTRAINT TRIGGER paddle_adjustment_commit_failure AFTER INSERT ON billing_purchase_adjustments DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION paddle_adjustment_commit_failure()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = f.db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS paddle_adjustment_commit_failure ON billing_purchase_adjustments; DROP FUNCTION IF EXISTS paddle_adjustment_commit_failure()`)
	}()
	processor := newAdjustmentProcessor(t, f)
	if _, err := integration.ReceiveWebhook(t.Context(), f.store.Queue(), f.verifier, body, signedWebhookHeaders(body), webhookScope()); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "adjustment-commit-worker", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := processor.Process(t.Context(), claim); err == nil || !strings.Contains(err.Error(), "paddle adjustment commit failure") {
		t.Fatalf("commit failure error=%v, want injected failure", err)
	}
	assertAdjustmentAbsent(t, &f, "adj_zyxwvutsrqponmlkjihgfedcba")
	assertPaymentEffects(t, &f)
	if _, err := f.db.ExecContext(t.Context(), `DROP TRIGGER paddle_adjustment_commit_failure ON billing_purchase_adjustments; DROP FUNCTION paddle_adjustment_commit_failure()`); err != nil {
		t.Fatal(err)
	}
	expireInboxClaim(t, f.db, "evt_zyxwvutsrqponmlkjihgfedcba")
	retryClaim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "adjustment-commit-retry", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("retry claim ok=%v err=%v", ok, err)
	}
	if err := processor.Process(t.Context(), retryClaim); err != nil {
		t.Fatal(err)
	}
	assertAdjustmentApplied(t, &f, "adj_zyxwvutsrqponmlkjihgfedcba")
}

type paddleAdjustmentInput struct {
	EventID, EventType, AdjustmentID, Status, Type, ItemType, TransactionID, CustomerID, ItemID, Subtotal, Total, Tax string
	OccurredAt, CreatedAt, UpdatedAt                                                                                  time.Time
}

func paddleAdjustmentBody(in paddleAdjustmentInput) []byte {
	if in.Type == "" {
		in.Type = "full"
	}
	if in.ItemType == "" {
		in.ItemType = "full"
	}
	if in.Subtotal == "" {
		if in.ItemType == "tax" {
			in.Subtotal = "0"
		} else {
			in.Subtotal = in.Total
		}
	}
	return []byte(fmt.Sprintf(`{"event_id":%q,"event_type":%q,"occurred_at":%q,"data":{"id":%q,"action":"refund","status":%q,"type":%q,"transaction_id":%q,"customer_id":%q,"currency_code":"USD","created_at":%q,"updated_at":%q,"totals":{"subtotal":%q,"tax":%q,"total":%q,"currency_code":"USD"},"items":[{"id":"adjitm_abcdefghijklmnopqrstuvwxyz","type":%q,"item_id":%q,"totals":{"subtotal":%q,"tax":%q,"total":%q}}]}}`, in.EventID, in.EventType, in.OccurredAt.Format(time.RFC3339Nano), in.AdjustmentID, in.Status, in.Type, in.TransactionID, in.CustomerID, in.CreatedAt.Format(time.RFC3339Nano), in.UpdatedAt.Format(time.RFC3339Nano), in.Subtotal, in.Tax, in.Total, in.ItemType, in.ItemID, in.Subtotal, in.Tax, in.Total))
}

func newAdjustmentProcessor(t *testing.T, f paymentFixture) *paddle.AdjustmentProcessor {
	t.Helper()
	processor, err := paddle.NewAdjustmentProcessor(paddle.AdjustmentProcessorConfig{Merchant: "merchant-test", Environment: paddle.Sandbox, Repository: f.store.Queue(), Now: func() time.Time { return webhookNow.Add(time.Minute) }, PolicyVersion: "v1", CreditPolicy: purchase.CreditRefundFullOnly})
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func processAdjustmentBody(t *testing.T, f *paymentFixture, processor *paddle.AdjustmentProcessor, body []byte, worker string) {
	t.Helper()
	if _, err := integration.ReceiveWebhook(t.Context(), f.store.Queue(), f.verifier, body, signedWebhookHeaders(body), webhookScope()); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, worker, time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := processor.Process(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
}

func assertAdjustmentApplied(t *testing.T, f *paymentFixture, providerID string) {
	t.Helper()
	var record purchase.AdjustmentRecord
	err := f.store.Purchases().WithinAccount(t.Context(), f.account, func(tx purchase.Tx) error {
		var err error
		record, err = tx.ProviderAdjustment(t.Context(), f.intent.Scope, providerID)
		return err
	})
	if err != nil || !record.Result.Applied {
		t.Fatalf("adjustment=%+v err=%v, want applied", record, err)
	}
}

func assertAdjustmentAbsent(t *testing.T, f *paymentFixture, providerID string) {
	t.Helper()
	err := f.store.Purchases().WithinAccount(t.Context(), f.account, func(tx purchase.Tx) error {
		_, err := tx.ProviderAdjustment(t.Context(), f.intent.Scope, providerID)
		return err
	})
	if !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("pending adjustment lookup err=%v, want not found", err)
	}
}
