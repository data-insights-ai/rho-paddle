package paddle

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"slices"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

const refundDispatchKind = "refund.dispatch"

var ErrRefundUnresolved = errors.New("paddle: refund outcome unresolved")

type RefundDispatchLine struct {
	QuoteLineID string
	Gross, Tax  int64
}

type RefundDispatchItem struct {
	ProviderLineID string
	Type           AdjustmentType
	Amount         string
}

type RefundDispatch struct {
	Account          billing.AccountID
	IntentID         string
	Operation        string
	TransactionID    string
	QuoteFingerprint string
	Customer         billing.Reference
	Lines            []RefundDispatchLine
	Items            []RefundDispatchItem
	// Whole records that the plan refunds every bound provider line in full.
	// It is decided against the collection binding at plan time; only a whole
	// plan may be sent as a transaction-level full adjustment.
	Whole       bool
	Correlation string
}

type RefundPlan struct {
	Intent  purchase.Intent
	Message integration.Message
}

type RefundServiceConfig struct {
	Client     *Client
	Repository integration.Repository
	Now        func() time.Time
}

type RefundService struct {
	client *Client
	repo   integration.Repository
	now    func() time.Time
}

func NewRefundService(config RefundServiceConfig) (*RefundService, error) {
	if config.Client == nil || config.Client.http == nil || !config.Client.scope.Valid() || config.Repository == nil {
		return nil, ErrInvalid
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &RefundService{client: config.Client, repo: config.Repository, now: config.Now}, nil
}

// PlanRefund freezes explicit quote-line allocations and provider items. It
// never contacts Paddle. Grouped partials require these frozen lines.
func (s *RefundService) PlanRefund(ctx context.Context, in RefundDispatch) (RefundPlan, error) {
	if s == nil || s.repo == nil || s.client == nil || in.Customer.Scope != s.client.Scope() || !paddlewire.ID(in.Customer.ID, "ctm_") || !paddlewire.ID(in.TransactionID, "txn_") || !billing.ValidID(string(in.Account)) || !billing.ValidID(in.IntentID) || !billing.ValidID(in.Operation) || !paddlewire.Fingerprint(in.QuoteFingerprint) {
		return RefundPlan{}, ErrInvalid
	}
	lines, items, err := canonicalRefund(in.Lines, in.Items)
	if err != nil {
		return RefundPlan{}, err
	}
	in.Lines, in.Items = lines, items
	in.Customer = billing.Reference{Scope: s.client.Scope(), ID: in.Customer.ID}
	messageID, err := integration.ProviderMessageID(s.client.Scope().Provider, s.client.Scope().Merchant, s.client.Scope().Environment, in.Operation)
	if err != nil {
		return RefundPlan{}, err
	}
	message := integration.Message{Account: in.Account, ID: messageID, Scope: s.client.Scope(), Kind: refundDispatchKind, Direction: integration.Outbound}
	var out RefundPlan
	err = s.repo.Atomic(ctx, in.Account, func(session integration.Session) error {
		service := purchase.New(session.Purchases(), s.now)
		intent, err := service.Intent(ctx, in.Account, in.IntentID)
		if err != nil {
			return err
		}
		if intent.Scope != s.client.Scope() || intent.QuoteFingerprint != in.QuoteFingerprint || intent.Account != in.Account {
			return billing.ErrConflict
		}
		funding, err := service.Funding(ctx, intent.Account, s.client.Scope(), in.TransactionID)
		if err != nil {
			return err
		}
		binding, err := service.CollectionBinding(ctx, intent.Account, s.client.Scope(), in.TransactionID)
		if err != nil {
			return err
		}
		if binding.CustomerID != in.Customer.ID || binding.QuoteFingerprint != intent.QuoteFingerprint || funding.Currency != intent.Currency {
			return billing.ErrConflict
		}
		whole, err := refundCoversFunding(ctx, in, binding, funding, session, intent)
		if err != nil {
			return err
		}
		in.Whole = whole
		in.Correlation = paddlewire.Digest(in.Account, in.Operation, in.IntentID, in.TransactionID, in.QuoteFingerprint, in.Customer.ID, in.Lines, in.Items, in.Whole)
		raw, err := json.Marshal(in)
		if err != nil || len(raw) > 1<<20 {
			return ErrInvalid
		}
		message.Payload = raw
		// A replay of the same operation must rebuild an identical message, so
		// the instant is the intent's, never the wall clock.
		message.OccurredAt = intent.CreatedAt
		if err := message.Validate(); err != nil {
			return err
		}
		if err := session.Enqueue(ctx, message); err != nil {
			return err
		}
		out = RefundPlan{Intent: intent, Message: message}
		return nil
	})
	if err != nil {
		return RefundPlan{}, err
	}
	return out, nil
}

// refundCoversFunding bounds the plan against what was collected and still
// refundable, and ties each provider item to the quote lines it owns. It
// reports whether the plan refunds every bound provider line in full, which is
// the only case that may be sent as a transaction-level full adjustment.
func refundCoversFunding(ctx context.Context, in RefundDispatch, binding purchase.CollectionBinding, funding purchase.Funding, session integration.Session, intent purchase.Intent) (bool, error) {
	funded := make(map[string]purchase.PaidLine, len(funding.Lines))
	for _, line := range funding.Lines {
		funded[line.LineID] = line
	}
	owners := make(map[string]string, len(binding.Lines))
	for _, line := range binding.Lines {
		for _, allocation := range line.Allocations {
			owners[allocation.QuoteLineID] = line.ProviderLineID
		}
	}
	var state purchase.AdjustmentState
	err := session.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		var readErr error
		state, readErr = tx.AdjustmentState(ctx, intent.ID)
		if errors.Is(readErr, billing.ErrNotFound) {
			state = purchase.AdjustmentState{Account: intent.Account, IntentID: intent.ID}
			return nil
		}
		return readErr
	})
	if err != nil {
		return false, err
	}
	refunded := make(map[string]purchase.LineAdjustmentTotal, len(state.Lines))
	for _, line := range state.Lines {
		refunded[line.LineID] = line
	}
	remaining := func(quoteLineID string) (int64, int64, bool) {
		paid, ok := funded[quoteLineID]
		if !ok {
			return 0, 0, false
		}
		done := refunded[quoteLineID]
		return paid.Gross - done.RefundedGross, paid.Tax - done.RefundedTax, true
	}
	seen := make(map[string]struct{}, len(in.Lines))
	plannedByOwner := make(map[string][]RefundDispatchLine, len(in.Items))
	for _, line := range in.Lines {
		grossLeft, taxLeft, ok := remaining(line.QuoteLineID)
		if !ok || line.Gross <= 0 || line.Tax < 0 || line.Tax > line.Gross || line.Gross > grossLeft || line.Tax > taxLeft {
			return false, billing.ErrConflict
		}
		if _, exists := seen[line.QuoteLineID]; exists {
			return false, billing.ErrConflict
		}
		seen[line.QuoteLineID] = struct{}{}
		owner, ok := owners[line.QuoteLineID]
		if !ok {
			return false, billing.ErrConflict
		}
		plannedByOwner[owner] = append(plannedByOwner[owner], line)
	}
	itemSeen := make(map[string]struct{}, len(in.Items))
	bound := make(map[string]purchase.CollectionLine, len(binding.Lines))
	for _, line := range binding.Lines {
		bound[line.ProviderLineID] = line
	}
	whole := len(in.Items) == len(bound)
	for _, item := range in.Items {
		collection, ok := bound[item.ProviderLineID]
		if !ok {
			return false, billing.ErrConflict
		}
		if _, ok := itemSeen[item.ProviderLineID]; ok {
			return false, billing.ErrConflict
		}
		itemSeen[item.ProviderLineID] = struct{}{}
		planned := plannedByOwner[item.ProviderLineID]
		if len(planned) == 0 {
			return false, billing.ErrConflict
		}
		var gross int64
		for _, line := range planned {
			gross, err = paddlewire.AddMoney(gross, line.Gross)
			if err != nil {
				return false, err
			}
		}
		switch item.Type {
		case AdjustmentPartial:
			whole = false
			amount, err := paddlewire.MinorUnits(item.Amount)
			if err != nil || amount != gross {
				return false, billing.ErrConflict
			}
		case AdjustmentFull:
			// A full item refunds everything still refundable on that provider
			// line, so the plan must claim exactly that for every allocation.
			if item.Amount != "" {
				return false, billing.ErrConflict
			}
			claimed := make(map[string]RefundDispatchLine, len(planned))
			for _, line := range planned {
				claimed[line.QuoteLineID] = line
			}
			if len(claimed) != len(collection.Allocations) {
				return false, billing.ErrConflict
			}
			for _, allocation := range collection.Allocations {
				line, ok := claimed[allocation.QuoteLineID]
				if !ok {
					return false, billing.ErrConflict
				}
				grossLeft, taxLeft, ok := remaining(allocation.QuoteLineID)
				if !ok || line.Gross != grossLeft || line.Tax != taxLeft {
					return false, billing.ErrConflict
				}
			}
		default:
			return false, billing.ErrConflict
		}
	}
	// A planned ledger line with no provider item would be booked as refunded
	// while no money leaves that transaction item.
	for owner := range plannedByOwner {
		if _, ok := itemSeen[owner]; !ok {
			return false, billing.ErrConflict
		}
	}
	return whole, nil
}

// Dispatch records the send permission before POST /adjustments. Ambiguous
// responses stay unknown and never authorize another create.
func (s *RefundService) Dispatch(ctx context.Context, claim integration.Claim) (Adjustment, error) {
	if s == nil || s.client == nil || s.repo == nil || claim.Message.Direction != integration.Outbound || claim.Message.Kind != refundDispatchKind {
		return Adjustment{}, ErrInvalid
	}
	payload, err := decodeRefundDispatch(claim.Message.Payload)
	if err != nil || payload.Account != claim.Message.Account || payload.Customer.Scope != s.client.Scope() {
		return Adjustment{}, ErrInvalid
	}
	expectedMessageID, err := integration.ProviderMessageID(s.client.Scope().Provider, s.client.Scope().Merchant, s.client.Scope().Environment, payload.Operation)
	if err != nil || claim.Message.ID != expectedMessageID || claim.Message.ProviderScope() != s.client.Scope() {
		return Adjustment{}, billing.ErrConflict
	}
	send, err := s.repo.BeginOutbox(ctx, claim, func(session integration.Session) error {
		service := purchase.New(session.Purchases(), s.now)
		intent, err := service.Intent(ctx, payload.Account, payload.IntentID)
		if err != nil {
			return err
		}
		if intent.Scope != s.client.Scope() || intent.QuoteFingerprint != payload.QuoteFingerprint {
			return billing.ErrConflict
		}
		return nil
	})
	if err != nil {
		return Adjustment{}, err
	}
	if !send {
		return Adjustment{}, billing.ErrConflict
	}
	created, callErr := s.client.CreateAdjustment(ctx, AdjustmentCreate{Action: AdjustmentRefund, Type: refundType(payload), Transaction: billing.Reference{Scope: s.client.Scope(), ID: payload.TransactionID}, Reason: payload.Operation, Items: refundCreateItems(payload.Items)})
	if callErr != nil {
		switch classifyDispatch(callErr) {
		case dispatchUnsent:
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			releaseErr := s.repo.ReleaseOutbox(releaseCtx, claim, "provider_refused_before_send", s.now().Add(releaseDelay(callErr)))
			return Adjustment{}, errors.Join(integration.ErrBackpressure, callErr, releaseErr)
		case dispatchUnknown:
			persistErr := s.finishObservation(ctx, claim, integration.OutboxUnknown, "provider_outcome_uncertain", "")
			return Adjustment{}, errors.Join(ErrRefundUnresolved, callErr, persistErr)
		default:
			persistErr := s.finishObservation(ctx, claim, integration.OutboxRejected, "provider_rejected", "")
			return Adjustment{}, errors.Join(callErr, persistErr)
		}
	}
	if err := validateRefundAdjustment(created, payload); err != nil {
		persistErr := s.finishObservation(ctx, claim, integration.OutboxUnknown, "provider_response_unresolved", created.Reference.ID)
		return Adjustment{}, errors.Join(ErrRefundUnresolved, err, persistErr)
	}
	observation := checkoutObservation(claim.Message.ID, created.Reference.ID, "accepted")
	err = s.repo.FinishOutbox(ctx, claim, integration.OutboxResult{ObservationID: observation, State: integration.OutboxCompleted, ProviderReference: created.Reference.ID, Evidence: "accepted"}, func(integration.Session) error { return nil })
	if err != nil {
		unknownErr := s.finishObservation(ctx, claim, integration.OutboxUnknown, "provider_response_commit_uncertain", created.Reference.ID)
		return Adjustment{}, errors.Join(ErrRefundUnresolved, err, unknownErr)
	}
	return created, nil
}

func (s *RefundService) finishObservation(ctx context.Context, claim integration.Claim, state integration.OutboxState, evidence, providerReference string) error {
	observation := checkoutObservation(claim.Message.ID, providerReference, string(state)+":"+evidence)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.repo.FinishOutbox(finishCtx, claim, integration.OutboxResult{ObservationID: observation, State: state, ProviderReference: providerReference, Evidence: evidence}, func(integration.Session) error { return nil })
}

func refundNoop(integration.Session) error { return nil }

func canonicalRefund(lines []RefundDispatchLine, items []RefundDispatchItem) ([]RefundDispatchLine, []RefundDispatchItem, error) {
	if len(lines) == 0 || len(lines) > 100 || len(items) == 0 || len(items) > 100 {
		return nil, nil, ErrInvalid
	}
	outLines := slices.Clone(lines)
	slices.SortFunc(outLines, func(a, b RefundDispatchLine) int { return cmp.Compare(a.QuoteLineID, b.QuoteLineID) })
	seenLine := make(map[string]struct{}, len(outLines))
	for _, line := range outLines {
		if !billing.ValidID(line.QuoteLineID) || line.Gross <= 0 || line.Tax < 0 || line.Tax > line.Gross {
			return nil, nil, ErrInvalid
		}
		if _, ok := seenLine[line.QuoteLineID]; ok {
			return nil, nil, billing.ErrConflict
		}
		seenLine[line.QuoteLineID] = struct{}{}
	}
	outItems := slices.Clone(items)
	slices.SortFunc(outItems, func(a, b RefundDispatchItem) int { return cmp.Compare(a.ProviderLineID, b.ProviderLineID) })
	seenItem := make(map[string]struct{}, len(outItems))
	for _, item := range outItems {
		if !paddlewire.ID(item.ProviderLineID, "txnitm_") || (item.Type != AdjustmentFull && item.Type != AdjustmentPartial) {
			return nil, nil, ErrInvalid
		}
		if item.Type == AdjustmentPartial {
			if _, err := paddlewire.MinorUnits(item.Amount); err != nil {
				return nil, nil, ErrInvalid
			}
		}
		if item.Type == AdjustmentFull && item.Amount != "" {
			return nil, nil, ErrInvalid
		}
		if _, ok := seenItem[item.ProviderLineID]; ok {
			return nil, nil, billing.ErrConflict
		}
		seenItem[item.ProviderLineID] = struct{}{}
	}
	return outLines, outItems, nil
}

func decodeRefundDispatch(raw []byte) (RefundDispatch, error) {
	var out RefundDispatch
	if json.Unmarshal(raw, &out) != nil || !billing.ValidID(string(out.Account)) || !billing.ValidID(out.IntentID) || !billing.ValidID(out.Operation) || !paddlewire.ID(out.TransactionID, "txn_") || !paddlewire.ID(out.Customer.ID, "ctm_") || !out.Customer.Scope.Valid() || !paddlewire.Fingerprint(out.QuoteFingerprint) || !paddlewire.Fingerprint(out.Correlation) {
		return RefundDispatch{}, ErrResponse
	}
	lines, items, err := canonicalRefund(out.Lines, out.Items)
	if err != nil || !slices.Equal(lines, out.Lines) || !slices.Equal(items, out.Items) {
		return RefundDispatch{}, ErrResponse
	}
	if paddlewire.Digest(out.Account, out.Operation, out.IntentID, out.TransactionID, out.QuoteFingerprint, out.Customer.ID, out.Lines, out.Items, out.Whole) != out.Correlation {
		return RefundDispatch{}, ErrResponse
	}
	return out, nil
}

// refundType is the transaction-level Paddle type. A full adjustment omits the
// items array and refunds the whole transaction, so it is used only when the
// frozen plan covers every bound provider line in full.
func refundType(p RefundDispatch) AdjustmentType {
	if !p.Whole {
		return AdjustmentPartial
	}
	for _, item := range p.Items {
		if item.Type != AdjustmentFull {
			return AdjustmentPartial
		}
	}
	return AdjustmentFull
}

func refundCreateItems(items []RefundDispatchItem) []AdjustmentCreateItem {
	out := make([]AdjustmentCreateItem, len(items))
	for i, item := range items {
		out[i] = AdjustmentCreateItem{TransactionItemID: item.ProviderLineID, Type: item.Type, Amount: item.Amount}
	}
	return out
}

// validateRefundAdjustment reconciles the created adjustment against the frozen
// plan. Identity alone is not enough: an adjustment that moved a different
// amount, or touched different provider lines, must not be recorded as the
// planned refund.
func validateRefundAdjustment(got Adjustment, p RefundDispatch) error {
	if got.Action != AdjustmentRefund || got.Transaction.ID != p.TransactionID || got.Customer.ID != p.Customer.ID || got.Reason != p.Operation {
		return ErrResponse
	}
	switch got.Status {
	case AdjustmentPendingApproval, AdjustmentApproved, AdjustmentRejected, AdjustmentReversed:
	default:
		return ErrResponse
	}
	var gross, tax int64
	var err error
	for _, line := range p.Lines {
		if gross, err = paddlewire.AddMoney(gross, line.Gross); err != nil {
			return err
		}
		if tax, err = paddlewire.AddMoney(tax, line.Tax); err != nil {
			return err
		}
	}
	if got.Gross != gross || got.Tax != tax || got.Subtotal != gross-tax {
		return ErrResponse
	}
	planned := make(map[string]struct{}, len(p.Items))
	for _, item := range p.Items {
		planned[item.ProviderLineID] = struct{}{}
	}
	// A full adjustment omits items on the wire and Paddle echoes back every
	// line it touched, so the echoed set must be exactly what was planned.
	if len(got.Items) != len(planned) {
		return ErrResponse
	}
	for _, line := range got.Items {
		if _, ok := planned[line.TransactionItem.ID]; !ok {
			return ErrResponse
		}
	}
	return nil
}
