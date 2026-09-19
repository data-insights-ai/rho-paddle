package paddle

import (
	"context"
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"net/http"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type ScheduledAction string

const (
	ScheduledPause  ScheduledAction = "pause"
	ScheduledCancel ScheduledAction = "cancel"
	ScheduledResume ScheduledAction = "resume"
)

type ScheduledChange struct {
	Action      ScheduledAction
	EffectiveAt time.Time
	ResumeAt    time.Time
}

type SubscriptionStatus string

const (
	SubscriptionActive   SubscriptionStatus = "active"
	SubscriptionTrialing SubscriptionStatus = "trialing"
	SubscriptionPastDue  SubscriptionStatus = "past_due"
	SubscriptionPaused   SubscriptionStatus = "paused"
	SubscriptionCanceled SubscriptionStatus = "canceled"
)

type ProrationMode string

const (
	ProrationProratedImmediately ProrationMode = "prorated_immediately"
	ProrationProratedNextPeriod  ProrationMode = "prorated_next_billing_period"
	ProrationFullImmediately     ProrationMode = "full_immediately"
	ProrationFullNextPeriod      ProrationMode = "full_next_billing_period"
	ProrationDoNotBill           ProrationMode = "do_not_bill"
)

type Subscription struct {
	Reference       billing.Reference
	Customer        billing.Reference
	Status          SubscriptionStatus
	CollectionMode  CollectionMode
	Items           []CheckoutItem
	ScheduledChange ScheduledChange
}

type SubscriptionUpdate struct {
	Subscription billing.Reference
	Items        []CheckoutItem
	Proration    ProrationMode
}

func (c *Client) Subscription(ctx context.Context, ref billing.Reference) (Subscription, error) {
	if c == nil || ref.Scope != c.scope || !paddlewire.ID(ref.ID, "sub_") {
		return Subscription{}, ErrInvalid
	}
	var wire paddlewire.Subscription
	if err := c.request(ctx, http.MethodGet, "/subscriptions/"+ref.ID, nil, &wire); err != nil {
		return Subscription{}, err
	}
	out, err := c.subscription(wire)
	if err != nil || out.Reference != ref {
		return Subscription{}, ErrResponse
	}
	return out, nil
}

func (c *Client) subscription(w paddlewire.Subscription) (Subscription, error) {
	if !paddlewire.ID(w.ID, "sub_") || !paddlewire.ID(w.CustomerID, "ctm_") {
		return Subscription{}, ErrResponse
	}
	switch SubscriptionStatus(w.Status) {
	case SubscriptionActive, SubscriptionTrialing, SubscriptionPastDue, SubscriptionPaused, SubscriptionCanceled:
	default:
		return Subscription{}, ErrResponse
	}
	switch CollectionMode(w.CollectionMode) {
	case CollectionAutomatic, CollectionManual:
	default:
		return Subscription{}, ErrResponse
	}
	items := make([]CheckoutItem, 0, len(w.Items))
	for _, item := range w.Items {
		if !paddlewire.ID(item.Price.ID, "pri_") || item.Quantity <= 0 {
			return Subscription{}, ErrResponse
		}
		items = append(items, CheckoutItem{Price: billing.Reference{Scope: c.scope, ID: item.Price.ID}, Quantity: item.Quantity})
	}
	change, err := scheduledChange(w)
	if err != nil {
		return Subscription{}, err
	}
	return Subscription{
		Reference:       billing.Reference{Scope: c.scope, ID: w.ID},
		Customer:        billing.Reference{Scope: c.scope, ID: w.CustomerID},
		Status:          SubscriptionStatus(w.Status),
		CollectionMode:  CollectionMode(w.CollectionMode),
		Items:           items,
		ScheduledChange: change,
	}, nil
}

func scheduledChange(w paddlewire.Subscription) (ScheduledChange, error) {
	if w.ScheduledChange == nil {
		return ScheduledChange{}, nil
	}
	switch w.ScheduledChange.Action {
	case string(ScheduledPause), string(ScheduledCancel), string(ScheduledResume):
	default:
		return ScheduledChange{}, ErrResponse
	}
	at, err := parseProviderTime(w.ScheduledChange.EffectiveAt)
	if err != nil {
		return ScheduledChange{}, ErrResponse
	}
	change := ScheduledChange{Action: ScheduledAction(w.ScheduledChange.Action), EffectiveAt: at}
	if w.ScheduledChange.ResumeAt != nil && *w.ScheduledChange.ResumeAt != "" {
		resume, err := parseProviderTime(*w.ScheduledChange.ResumeAt)
		if err != nil {
			return ScheduledChange{}, ErrResponse
		}
		change.ResumeAt = resume
	}
	return change, nil
}

func parseProviderTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, ErrResponse
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return billing.CanonicalTime(parsed), nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return billing.CanonicalTime(parsed), nil
}

func (c *Client) UpdateSubscription(ctx context.Context, in SubscriptionUpdate) (Subscription, error) {
	request, err := c.subscriptionUpdateRequest(in)
	if err != nil {
		return Subscription{}, err
	}
	var wire paddlewire.Subscription
	if err := c.request(ctx, http.MethodPatch, "/subscriptions/"+in.Subscription.ID, request, &wire); err != nil {
		return Subscription{}, err
	}
	out, err := c.subscription(wire)
	if err != nil || out.Reference != in.Subscription {
		return Subscription{}, errors.Join(ErrResponse, ErrUncertain)
	}
	return out, nil
}

// SubscriptionPreview is what a proposed update would do to the money:
// what is charged or credited now, and what recurs afterwards. Nothing is
// applied.
type SubscriptionPreview struct {
	// Action is "charge", "credit" or "" when nothing is due now.
	Action string
	// Amount is the immediate charge or credit in minor units, always
	// non-negative; Action says which.
	Amount   int64
	Currency string
	// Charge and Credit are the gross components the provider computed
	// before netting them into Amount.
	Charge, Credit int64
	// NextBilledAt is when the subscription bills next after the change.
	NextBilledAt time.Time
	// RecurringAmount is the total of each renewal after the change, in
	// minor units, when the provider reports it.
	RecurringAmount int64
}

// PreviewSubscriptionUpdate asks the provider what UpdateSubscription with
// the same input would charge or credit, without applying it. A host shows
// this before it asks the customer to confirm a change.
func (c *Client) PreviewSubscriptionUpdate(ctx context.Context, in SubscriptionUpdate) (SubscriptionPreview, error) {
	request, err := c.subscriptionUpdateRequest(in)
	if err != nil {
		return SubscriptionPreview{}, err
	}
	var wire paddlewire.SubscriptionPreview
	if err := c.request(ctx, http.MethodPatch, "/subscriptions/"+in.Subscription.ID+"/preview", request, &wire); err != nil {
		return SubscriptionPreview{}, err
	}
	out := SubscriptionPreview{Currency: wire.CurrencyCode}
	if wire.UpdateSummary != nil {
		switch wire.UpdateSummary.Result.Action {
		case "charge", "credit":
			out.Action = wire.UpdateSummary.Result.Action
		case "":
		default:
			return SubscriptionPreview{}, ErrResponse
		}
		if out.Amount, err = paddlewire.MinorUnits(wire.UpdateSummary.Result.Amount); err != nil {
			return SubscriptionPreview{}, ErrResponse
		}
		if wire.UpdateSummary.Result.CurrencyCode != "" {
			out.Currency = wire.UpdateSummary.Result.CurrencyCode
		}
		if wire.UpdateSummary.Charge.Amount != "" {
			if out.Charge, err = paddlewire.MinorUnits(wire.UpdateSummary.Charge.Amount); err != nil {
				return SubscriptionPreview{}, ErrResponse
			}
		}
		if wire.UpdateSummary.Credit.Amount != "" {
			if out.Credit, err = paddlewire.MinorUnits(wire.UpdateSummary.Credit.Amount); err != nil {
				return SubscriptionPreview{}, ErrResponse
			}
		}
	}
	if wire.NextBilledAt != "" {
		if out.NextBilledAt, err = parseProviderTime(wire.NextBilledAt); err != nil {
			return SubscriptionPreview{}, ErrResponse
		}
	}
	if wire.RecurringTransactionDetails != nil && wire.RecurringTransactionDetails.Totals.Total != "" {
		if out.RecurringAmount, err = paddlewire.MinorUnits(wire.RecurringTransactionDetails.Totals.Total); err != nil {
			return SubscriptionPreview{}, ErrResponse
		}
	}
	return out, nil
}

// subscriptionUpdateRequest validates an update and shapes it for the wire;
// shared by the update and its preview so the two cannot drift.
func (c *Client) subscriptionUpdateRequest(in SubscriptionUpdate) (any, error) {
	if c == nil || in.Subscription.Scope != c.scope || !paddlewire.ID(in.Subscription.ID, "sub_") || len(in.Items) == 0 || len(in.Items) > 100 {
		return nil, ErrInvalid
	}
	switch in.Proration {
	case ProrationProratedImmediately, ProrationProratedNextPeriod, ProrationFullImmediately, ProrationFullNextPeriod, ProrationDoNotBill:
	default:
		return nil, ErrInvalid
	}
	type item struct {
		PriceID  string `json:"price_id"`
		Quantity int64  `json:"quantity"`
	}
	items := make([]item, len(in.Items))
	for i, line := range in.Items {
		if line.Price.Scope != c.scope || !paddlewire.ID(line.Price.ID, "pri_") || line.Quantity <= 0 {
			return nil, ErrInvalid
		}
		items[i] = item{PriceID: line.Price.ID, Quantity: line.Quantity}
	}
	return struct {
		Items                []item `json:"items"`
		ProrationBillingMode string `json:"proration_billing_mode"`
	}{items, string(in.Proration)}, nil
}
