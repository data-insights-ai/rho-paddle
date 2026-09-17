package paddle_test

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestAdjustmentProcessorPostgresTerminalRefundBeforeFundingCompletes(t *testing.T) {
	for _, status := range []string{"pending_approval", "rejected"} {
		t.Run(status, func(t *testing.T) {
			f := newPaymentFixture(t, "adjustment-terminal-"+status, true)
			processor := newAdjustmentProcessor(t, f)
			eventID := "evt_00000000000000000000000011"
			if status == "rejected" {
				eventID = "evt_00000000000000000000000012"
			}
			adjustmentID := "adj_00000000000000000000000011"
			body := paddleAdjustmentBody(paddleAdjustmentInput{
				EventID: eventID, EventType: "adjustment.updated", OccurredAt: webhookNow,
				AdjustmentID: adjustmentID, Status: status, TransactionID: f.binding.TransactionID,
				CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID,
				Total: "100", Tax: "0", CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow.Add(-time.Second),
			})
			processAdjustmentBody(t, &f, processor, body, "terminal-refund-worker")
			assertAdjustmentInboxState(t, &f, eventID, "processed")
			assertAdjustmentAbsent(t, &f, adjustmentID)
			assertNoPaymentEffects(t, &f)
		})
	}
}

func TestAdjustmentProcessorPostgresCommitsBusinessRejection(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "adjustment-durable-rejection")
	processor := newAdjustmentProcessor(t, f)
	eventID := "evt_00000000000000000000000013"
	adjustmentID := "adj_00000000000000000000000013"
	body := paddleAdjustmentBody(paddleAdjustmentInput{
		EventID: eventID, EventType: "adjustment.updated", OccurredAt: webhookNow,
		AdjustmentID: adjustmentID, Status: "approved", TransactionID: f.binding.TransactionID,
		CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID,
		Total: "100", Tax: "0", CreatedAt: f.clock.Add(-time.Second), UpdatedAt: webhookNow.Add(-time.Second),
	})
	processAdjustmentBody(t, &f, processor, body, "durable-rejection-worker")
	assertAdjustmentInboxState(t, &f, eventID, "processed")

	var record purchase.AdjustmentRecord
	err := f.store.Purchases().WithinAccount(t.Context(), f.account, func(tx purchase.Tx) error {
		var err error
		record, err = tx.ProviderAdjustment(t.Context(), f.intent.Scope, adjustmentID)
		return err
	})
	if err != nil || record.Result.Applied || record.Result.Rejection != "adjustment_time_outside_funding" || len(record.Result.Effects) != 0 {
		t.Fatalf("durable rejection=%+v err=%v", record, err)
	}
	assertPaymentEffects(t, &f)
}

func TestAdjustmentProcessorPostgresChangedProviderAdjustmentConflicts(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "adjustment-changed-replay")
	processor := newAdjustmentProcessor(t, f)
	adjustmentID := "adj_00000000000000000000000014"
	createdAt := webhookNow.Add(-2 * time.Second)
	first := paddleAdjustmentBody(paddleAdjustmentInput{
		EventID: "evt_00000000000000000000000014", EventType: "adjustment.created", OccurredAt: webhookNow,
		AdjustmentID: adjustmentID, Status: "approved", TransactionID: f.binding.TransactionID,
		CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID,
		Total: "50", Tax: "0", CreatedAt: createdAt, UpdatedAt: webhookNow.Add(-time.Second),
	})
	processAdjustmentBody(t, &f, processor, first, "changed-replay-first-worker")

	secondEvent := "evt_00000000000000000000000015"
	changed := paddleAdjustmentBody(paddleAdjustmentInput{
		EventID: secondEvent, EventType: "adjustment.updated", OccurredAt: webhookNow,
		AdjustmentID: adjustmentID, Status: "approved", TransactionID: f.binding.TransactionID,
		CustomerID: f.binding.CustomerID, ItemID: f.binding.Lines[0].ProviderLineID,
		Total: "40", Tax: "0", CreatedAt: createdAt, UpdatedAt: webhookNow,
	})
	claim := receiveAndClaimAdjustment(t, &f, changed, "changed-replay-second-worker")
	if err := processor.Process(t.Context(), claim); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed replay error=%v, want conflict", err)
	}
	assertAdjustmentInboxState(t, &f, secondEvent, "processing")

	var record purchase.AdjustmentRecord
	err := f.store.Purchases().WithinAccount(t.Context(), f.account, func(tx purchase.Tx) error {
		var err error
		record, err = tx.ProviderAdjustment(t.Context(), f.intent.Scope, adjustmentID)
		return err
	})
	if err != nil || !record.Result.Applied || len(record.Input.Lines) != 1 || record.Input.Lines[0].Gross != 50 {
		t.Fatalf("retained adjustment=%+v err=%v", record, err)
	}
}

func TestAdjustmentProcessorPostgresRejectsCustomerOwnershipMismatch(t *testing.T) {
	f := paidPaddleAdjustmentFixture(t, "adjustment-customer-owner")
	processor := newAdjustmentProcessor(t, f)
	eventID := "evt_00000000000000000000000016"
	body := paddleAdjustmentBody(paddleAdjustmentInput{
		EventID: eventID, EventType: "adjustment.updated", OccurredAt: webhookNow,
		AdjustmentID: "adj_00000000000000000000000016", Status: "approved", TransactionID: f.binding.TransactionID,
		CustomerID: "ctm_00000000000000000000000016", ItemID: f.binding.Lines[0].ProviderLineID,
		Total: "100", Tax: "0", CreatedAt: webhookNow.Add(-2 * time.Second), UpdatedAt: webhookNow.Add(-time.Second),
	})
	claim := receiveAndClaimAdjustment(t, &f, body, "customer-owner-worker")
	if err := processor.Process(t.Context(), claim); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("customer mismatch error=%v, want conflict", err)
	}
	assertAdjustmentInboxState(t, &f, eventID, "processing")
	assertAdjustmentAbsent(t, &f, "adj_00000000000000000000000016")
	assertPaymentEffects(t, &f)
}

func receiveAndClaimAdjustment(t *testing.T, f *paymentFixture, body []byte, worker string) integration.Claim {
	t.Helper()
	if _, err := integration.ReceiveWebhook(t.Context(), f.store.Queue(), f.verifier, body, signedWebhookHeaders(body), webhookScope()); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, worker, time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	return claim
}

func assertAdjustmentInboxState(t *testing.T, f *paymentFixture, eventID, want string) {
	t.Helper()
	var got string
	if err := f.db.QueryRowContext(t.Context(), `SELECT state FROM billing_inbox WHERE account_id=$1 AND message_id=$2`, f.account, eventID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("inbox state=%q, want %q", got, want)
	}
}
