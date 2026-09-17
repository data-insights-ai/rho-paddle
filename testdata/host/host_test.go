package host_test

import (
	"context"
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"testing"

	host "example.com/paddle-host"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

var _ integration.WebhookVerifier = (*paddle.WebhookVerifier)(nil)
var _ func(integration.Repository, *paddle.Client, string, paddle.Environment, string, purchase.CreditRefundPolicy) (func(context.Context, integration.Claim) error, error) = host.AdjustmentHandler

func TestExternalHostConstructsScopedAdapter(t *testing.T) {
	client, err := paddle.New(paddle.Config{Merchant: "host-merchant", Environment: paddle.Sandbox, APIKey: "synthetic-host-test-key"})
	if err != nil {
		t.Fatal(err)
	}
	want := billing.Scope{Provider: "paddle", Merchant: "host-merchant", Environment: "sandbox"}
	if client.Scope() != want {
		t.Fatalf("scope=%+v", client.Scope())
	}
}

func TestExternalHostConfiguresAdjustmentWorker(t *testing.T) {
	handler, err := host.AdjustmentHandler(nil, nil, "host-merchant", paddle.Sandbox, "host-adjustment-v1", purchase.CreditRefundFullOnly)
	if err == nil || handler != nil {
		t.Fatalf("nil repository returned handler=%t err=%v, want constructor rejection", handler != nil, err)
	}
}

func TestExternalHostRejectsManualCollectionBeforeCharge(t *testing.T) {
	err := billing.RequireSupported(paddle.CollectionModeCapability(paddle.CollectionManual))
	capability, ok := errors.AsType[*billing.CapabilityError](err)
	if !ok || capability.Support != billing.SupportUnsupported || capability.Reason != "manual_invoice_collection" {
		t.Fatalf("manual collection=%v", err)
	}
	if err := billing.RequireSupported(paddle.ChargeTimingCapability(paddle.ChargeImmediately)); err != nil {
		t.Fatal(err)
	}
}

func TestExternalHostReadsTypedClosedSetStates(t *testing.T) {
	tx := paddle.Transaction{Status: paddle.TransactionBilled, CollectionMode: paddle.CollectionAutomatic}
	if tx.Status != paddle.TransactionBilled || tx.CollectionMode != paddle.CollectionAutomatic {
		t.Fatalf("transaction=%+v", tx)
	}
	sub := paddle.Subscription{Status: paddle.SubscriptionPaused, CollectionMode: paddle.CollectionManual}
	if sub.Status != paddle.SubscriptionPaused || sub.CollectionMode != paddle.CollectionManual {
		t.Fatalf("subscription=%+v", sub)
	}
	adj := paddle.Adjustment{Action: paddle.AdjustmentRefund, Status: paddle.AdjustmentApproved, Type: paddle.AdjustmentFull}
	if adj.Action != paddle.AdjustmentRefund || adj.Status != paddle.AdjustmentApproved || adj.Type != paddle.AdjustmentFull {
		t.Fatalf("adjustment=%+v", adj)
	}
	update := paddle.SubscriptionUpdate{Proration: paddle.ProrationDoNotBill}
	if update.Proration != paddle.ProrationDoNotBill {
		t.Fatalf("proration=%q", update.Proration)
	}
	pause := paddle.PauseInput{When: paddle.EffectiveImmediately}
	if err := billing.RequireSupported(paddle.EffectiveFromCapability("pause", pause.When)); err != nil {
		t.Fatal(err)
	}
	topUp, ok := errors.AsType[*billing.CapabilityError](billing.RequireSupported(paddle.AutomaticTopUpCapability()))
	if !ok || topUp.Support != billing.SupportUnsupported || topUp.Reason != "automatic_top_up" {
		t.Fatalf("automatic top-up=%v", topUp)
	}
}
