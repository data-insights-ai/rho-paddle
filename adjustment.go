package paddle

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

// AdjustmentProcessor applies authenticated adjustment observations. Applying a
// chargeback reversal needs the original adjustments, which are not carried in
// the event, so a Client must be supplied and reversal events issue bounded
// provider lookups before the inbox transaction opens. Without a Client those
// events fail closed as unresolved rather than applying a partial reversal.
// Size the inbox lease to allow for that lookup. Hosts retain and retry
// unresolved observations after reconciliation.
type AdjustmentProcessor struct {
	scope         billing.Scope
	repository    integration.Repository
	client        *Client
	now           func() time.Time
	policyVersion string
	creditPolicy  purchase.CreditRefundPolicy
}

type AdjustmentProcessorConfig struct {
	Merchant      string
	Environment   Environment
	Repository    integration.Repository
	Client        *Client
	Now           func() time.Time
	PolicyVersion string
	CreditPolicy  purchase.CreditRefundPolicy
}

func NewAdjustmentProcessor(config AdjustmentProcessorConfig) (*AdjustmentProcessor, error) {
	scope, err := providerScope(config.Merchant, config.Environment)
	if err != nil || config.Repository == nil || !billing.ValidID(config.PolicyVersion) || (config.CreditPolicy != purchase.CreditRefundFullOnly && config.CreditPolicy != purchase.CreditRefundProportional) {
		return nil, ErrInvalid
	}
	if config.Client != nil && config.Client.scope != scope {
		return nil, ErrInvalid
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &AdjustmentProcessor{scope: scope, repository: config.Repository, client: config.Client, now: config.Now, policyVersion: config.PolicyVersion, creditPolicy: config.CreditPolicy}, nil
}

// Process commits the adjustment, product effects, dispute evidence and inbox
// result atomically. Pending or rejected refunds complete without monetary effects.
func (p *AdjustmentProcessor) Process(ctx context.Context, claim integration.Claim) error {
	if p == nil || p.repository == nil || p.now == nil || claim.Message.ProviderScope() != p.scope || claim.Message.Direction != integration.Inbound {
		return ErrInvalid
	}
	if err := claim.Message.Validate(); err != nil {
		return err
	}
	event, err := decodeEvent(claim.Message.Payload)
	if err != nil {
		return err
	}
	if event.ID != claim.Message.ID || event.Type != claim.Message.Kind || !event.OccurredAt.Equal(claim.Message.OccurredAt) {
		return billing.ErrConflict
	}
	if event.Type != "adjustment.created" && event.Type != "adjustment.updated" {
		return adjustmentUnresolved("event_type_not_supported")
	}
	var wire paddlewire.Adjustment
	if json.Unmarshal(event.data, &wire) != nil {
		return ErrResponse
	}
	wire.CreatedAt = billing.CanonicalTime(wire.CreatedAt)
	wire.UpdatedAt = billing.CanonicalTime(wire.UpdatedAt)
	if !paddlewire.ID(wire.ID, "adj_") || !paddlewire.ID(wire.TransactionID, "txn_") || !paddlewire.ID(wire.CustomerID, "ctm_") || wire.CreatedAt.IsZero() || wire.UpdatedAt.Before(wire.CreatedAt) || wire.UpdatedAt.After(event.OccurredAt) || event.OccurredAt.After(billing.CanonicalTime(p.now())) {
		return ErrResponse
	}
	if wire.Type != "" && wire.Type != "full" && wire.Type != "partial" {
		return ErrResponse
	}
	switch wire.Status {
	case "pending_approval", "approved", "rejected", "reversed":
	default:
		return ErrResponse
	}
	originals, err := p.lookupReversedOriginals(ctx, wire)
	if err != nil {
		return err
	}
	planned, err := p.loadPlannedRefund(ctx, claim.Message.Account, wire)
	if err != nil {
		return err
	}
	return p.repository.ProcessInbox(ctx, claim, func(session integration.Session) error {
		service := purchase.New(session.Purchases(), p.now)
		binding, err := service.CollectionBinding(ctx, claim.Message.Account, p.scope, wire.TransactionID)
		if err != nil {
			return err
		}
		intent, err := service.Intent(ctx, claim.Message.Account, binding.IntentID)
		if err != nil {
			return err
		}
		if intent.Scope != p.scope || intent.Currency != wire.Currency || binding.CustomerID != wire.CustomerID || binding.QuoteFingerprint != intent.QuoteFingerprint {
			return billing.ErrConflict
		}
		switch wire.Action {
		case "credit", "credit_reverse":
			return nil
		case "chargeback_reverse", "chargeback_warning_reverse":
			return p.applyReversedOriginals(ctx, session, event, originals, intent, binding)
		case "refund", "chargeback", "chargeback_warning":
		default:
			return adjustmentUnresolved("adjustment_action_not_supported")
		}
		switch wire.Action {
		case "refund":
			switch wire.Status {
			case "pending_approval", "rejected":
				// No money moved, so there is nothing to book.
				return nil
			case "reversed":
				// Paddle undid a refund this adapter may already have booked.
				// Leaving it booked understates the refundable balance, and
				// there is no reversal effect for refunds, so it fails closed
				// for an operator rather than committing a wrong balance.
				return adjustmentUnresolved("refund_reversal_unsupported")
			case "approved":
			default:
				return adjustmentUnresolved("refund_status_unresolved")
			}
		case "chargeback", "chargeback_warning":
			if wire.Status != "approved" && wire.Status != "reversed" {
				return adjustmentUnresolved("dispute_status_unresolved")
			}
		}
		funding, err := service.Funding(ctx, intent.Account, p.scope, wire.TransactionID)
		if err != nil {
			return err
		}
		lines, err := normalizeAdjustment(wire, binding, funding, planned)
		if err != nil {
			return err
		}
		input := purchase.AdjustmentInput{Account: intent.Account, ID: paddlewire.Digest("adjustment", p.scope, wire.ID), IntentID: intent.ID, ProviderAdjustmentID: wire.ID, TransactionID: wire.TransactionID, Scope: p.scope, Kind: purchase.AdjustmentRefund, Currency: wire.Currency, Lines: lines, PolicyVersion: p.policyVersion, CreditPolicy: p.creditPolicy, Actor: "paddle", Reason: "provider_adjustment", OccurredAt: wire.CreatedAt}
		if wire.Action != "refund" {
			input.Kind = purchase.AdjustmentChargeback
		}
		// Once applied, a debit retains its commercial reversal policy across
		// later provider status observations and host configuration changes.
		if old, err := service.Adjustment(ctx, intent.Account, input.ID); err == nil {
			input.PolicyVersion = old.Input.PolicyVersion
			input.CreditPolicy = old.Input.CreditPolicy
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		result, err := service.ApplyAdjustment(ctx, input)
		if err != nil {
			return err
		}
		if !result.Applied {
			// A business rejection is immutable evidence, not a transaction
			// failure. Commit it with the inbox result; Go errors above remain
			// retryable and roll back the transaction.
			return nil
		}
		if wire.Action == "refund" {
			return nil
		}
		return p.applyDispute(ctx, service, event, wire, intent, input, lines)
	})
}

func (p *AdjustmentProcessor) applyDispute(ctx context.Context, service *purchase.Service, event Event, wire paddlewire.Adjustment, intent purchase.Intent, input purchase.AdjustmentInput, lines []purchase.PaidLine) error {
	amount := int64(0)
	var err error
	for _, line := range lines {
		amount, err = paddlewire.AddMoney(amount, line.Gross)
		if err != nil {
			return err
		}
	}
	status := purchase.DisputeLost
	if wire.Action == "chargeback_warning" {
		status = purchase.DisputeWarning
	}
	initial := purchase.DisputeFact{
		Account: intent.Account, Scope: p.scope, EventID: event.ID, DisputeID: wire.ID, IntentID: intent.ID,
		TransactionID: wire.TransactionID, Currency: wire.Currency, Amount: amount, Status: status,
		OccurredAt: wire.CreatedAt, DebitAdjustmentID: input.ID, EvidenceReference: event.ID,
	}
	if wire.Status != "reversed" {
		_, err = service.ApplyDispute(ctx, initial)
		return err
	}
	if _, err = service.Dispute(ctx, intent.Account, p.scope, wire.ID); errors.Is(err, billing.ErrNotFound) {
		initial.EventID = paddlewire.Digest("dispute-open", p.scope, wire.ID)
		if _, err = service.ApplyDispute(ctx, initial); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	recovered := initial
	recovered.EventID = event.ID
	recovered.OccurredAt = wire.UpdatedAt
	recovered.Status = purchase.DisputeWon
	if wire.Action == "chargeback_warning" {
		recovered.Status = purchase.DisputeClosed
	}
	recovered.EvidenceReference = event.ID
	recovered.Recovery = &purchase.DisputeRecovery{ID: paddlewire.Digest("adjustment-full-recovery", p.scope, wire.ID), AdjustmentID: input.ID, Lines: lines, EvidenceReference: input.ID}
	_, err = service.ApplyDispute(ctx, recovered)
	return err
}

func (p *AdjustmentProcessor) lookupReversedOriginals(ctx context.Context, wire paddlewire.Adjustment) ([]paddlewire.Adjustment, error) {
	if wire.Action != "chargeback_reverse" && wire.Action != "chargeback_warning_reverse" {
		return nil, nil
	}
	if p.client == nil {
		return nil, adjustmentUnresolved("original_adjustment_lookup_required")
	}
	want := AdjustmentChargeback
	if wire.Action == "chargeback_warning_reverse" {
		want = AdjustmentChargebackWarning
	}
	var originals []paddlewire.Adjustment
	after := ""
	for range 100 {
		page, err := p.client.Adjustments(ctx, AdjustmentLookup{
			Transaction: billing.Reference{Scope: p.scope, ID: wire.TransactionID},
			Customer:    billing.Reference{Scope: p.scope, ID: wire.CustomerID},
			Action:      want,
			Status:      AdjustmentReversed,
			After:       after,
			Limit:       50,
		})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.Action != want || item.Status != AdjustmentReversed {
				continue
			}
			originals = append(originals, adjustmentWireFromListed(item))
		}
		if !page.HasMore {
			if len(originals) == 0 {
				return nil, adjustmentUnresolved("original_adjustment_lookup_required")
			}
			return originals, nil
		}
		after = page.NextAfter
	}
	return nil, adjustmentUnresolved("original_adjustment_lookup_required")
}

func (p *AdjustmentProcessor) loadPlannedRefund(ctx context.Context, account billing.AccountID, wire paddlewire.Adjustment) (*RefundDispatch, error) {
	if wire.Action != "refund" || !billing.ValidID(wire.Reason) {
		return nil, nil
	}
	messageID, err := integration.ProviderMessageID(p.scope.Provider, p.scope.Merchant, p.scope.Environment, wire.Reason)
	if err != nil {
		// A scope this adapter cannot address is a real failure, not evidence
		// that no refund was planned; treating it as "no plan" would apply the
		// refund without the host's frozen allocations.
		return nil, err
	}
	delivery, err := p.repository.Outbox(ctx, account, p.scope, messageID)
	if err != nil {
		if errors.Is(err, billing.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if delivery.Message.Kind != refundDispatchKind {
		return nil, nil
	}
	payload, err := decodeRefundDispatch(delivery.Message.Payload)
	if err != nil {
		return nil, err
	}
	if payload.TransactionID != wire.TransactionID || payload.Operation != wire.Reason {
		return nil, billing.ErrConflict
	}
	if delivery.ProviderReference != "" && delivery.ProviderReference != wire.ID {
		return nil, billing.ErrConflict
	}
	return &payload, nil
}

func (p *AdjustmentProcessor) applyReversedOriginals(ctx context.Context, session integration.Session, event Event, originals []paddlewire.Adjustment, intent purchase.Intent, binding purchase.CollectionBinding) error {
	if len(originals) == 0 {
		return adjustmentUnresolved("original_adjustment_lookup_required")
	}
	service := purchase.New(session.Purchases(), p.now)
	for _, original := range originals {
		// Each reversal consumes funded amounts, so the bound must be re-read
		// inside the loop; a pre-loop snapshot lets two reversals of the same
		// transaction each pass against the original funded total.
		funding, err := service.Funding(ctx, intent.Account, p.scope, binding.TransactionID)
		if err != nil {
			return err
		}
		if original.TransactionID != binding.TransactionID || original.CustomerID != binding.CustomerID || original.Currency != intent.Currency {
			return billing.ErrConflict
		}
		if dispute, err := service.Dispute(ctx, intent.Account, p.scope, original.ID); err == nil {
			if dispute.Status == purchase.DisputeWon || dispute.Status == purchase.DisputeClosed {
				continue
			}
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		lines, err := normalizeAdjustment(original, binding, funding, nil)
		if err != nil {
			return err
		}
		input := purchase.AdjustmentInput{Account: intent.Account, ID: paddlewire.Digest("adjustment", p.scope, original.ID), IntentID: intent.ID, ProviderAdjustmentID: original.ID, TransactionID: original.TransactionID, Scope: p.scope, Kind: purchase.AdjustmentChargeback, Currency: original.Currency, Lines: lines, PolicyVersion: p.policyVersion, CreditPolicy: p.creditPolicy, Actor: "paddle", Reason: "provider_adjustment", OccurredAt: original.CreatedAt}
		if old, err := service.Adjustment(ctx, intent.Account, input.ID); err == nil {
			input.PolicyVersion = old.Input.PolicyVersion
			input.CreditPolicy = old.Input.CreditPolicy
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		result, err := service.ApplyAdjustment(ctx, input)
		if err != nil {
			return err
		}
		if !result.Applied {
			continue
		}
		synthetic := event
		synthetic.ID = paddlewire.Digest("adjustment-reconcile", p.scope, original.ID, original.UpdatedAt)
		if err := p.applyDispute(ctx, service, synthetic, original, intent, input, lines); err != nil {
			return err
		}
	}
	return nil
}

func adjustmentUnresolved(reason string) error {
	return &billing.CapabilityError{Capability: billing.Capability{Operation: "process_adjustment_event", Support: billing.SupportUnresolved, Reason: reason}}
}
