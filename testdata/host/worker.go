package host

import (
	"context"

	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

// PaymentHandler connects a host-owned repository and queue scheduler to the
// adapter without importing implementation packages. The scheduler supplies an
// authenticated inbound claim and handles retry policy when processing fails.
func PaymentHandler(repository integration.Repository, merchant string, environment paddle.Environment) (func(context.Context, integration.Claim) error, error) {
	processor, err := paddle.NewTransactionProcessor(paddle.TransactionProcessorConfig{
		Merchant: merchant, Environment: environment, Repository: repository,
	})
	if err != nil {
		return nil, err
	}
	return processor.Process, nil
}

// AdjustmentHandler connects authenticated adjustment inbox claims to the same
// host-owned repository. The host chooses and versions its reversal policy;
// applied adjustments retain that policy when later provider events replay.
func AdjustmentHandler(repository integration.Repository, client *paddle.Client, merchant string, environment paddle.Environment, policyVersion string, creditPolicy purchase.CreditRefundPolicy) (func(context.Context, integration.Claim) error, error) {
	processor, err := paddle.NewAdjustmentProcessor(paddle.AdjustmentProcessorConfig{
		Merchant: merchant, Environment: environment, Repository: repository,
		// Without a client, chargeback reversals can never be applied.
		Client:        client,
		PolicyVersion: policyVersion, CreditPolicy: creditPolicy,
	})
	if err != nil {
		return nil, err
	}
	return processor.Process, nil
}

// CheckoutHandler lets a host dispatch its claimed outbound checkout while the
// library retains ambiguous outcomes for explicit recovery. A returned checkout
// URL is available only after the binding and result commit successfully.
func CheckoutHandler(repository integration.Repository, client *paddle.Client) (func(context.Context, integration.Claim) (paddle.Transaction, error), error) {
	service, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: repository})
	if err != nil {
		return nil, err
	}
	return service.Dispatch, nil
}

// RefundHandler dispatches host-planned refunds. Ambiguous creates stay unknown
// until RecoverRefund; they are never resent automatically.
func RefundHandler(repository integration.Repository, client *paddle.Client) (func(context.Context, integration.Claim) (paddle.Adjustment, error), error) {
	service, err := paddle.NewRefundService(paddle.RefundServiceConfig{Client: client, Repository: repository})
	if err != nil {
		return nil, err
	}
	return service.Dispatch, nil
}
