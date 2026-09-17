package paddle

import (
	"context"
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"net/http"
)

type EffectiveFrom string

const (
	EffectiveImmediately EffectiveFrom = "immediately"
	EffectiveNextPeriod  EffectiveFrom = "next_billing_period"
)

type PauseInput struct {
	Subscription billing.Reference
	When         EffectiveFrom
}

type ResumeInput struct {
	Subscription billing.Reference
	When         EffectiveFrom
}

type CancelInput struct {
	Subscription billing.Reference
	When         EffectiveFrom
}

func SubscriptionLifecycleCapability(operation string) billing.Capability {
	switch operation {
	case "pause", "resume", "cancel":
		return billing.Capability{Operation: "subscription_" + operation, Support: billing.SupportSupported}
	default:
		return billing.Capability{Operation: "subscription_lifecycle", Support: billing.SupportUnsupported, Reason: "lifecycle_operation_not_supported"}
	}
}

func EffectiveFromCapability(operation string, when EffectiveFrom) billing.Capability {
	switch when {
	case EffectiveImmediately:
		return billing.Capability{Operation: "immediate_lifecycle_change", Support: billing.SupportSupported}
	case EffectiveNextPeriod:
		if operation == "resume" {
			return billing.Capability{Operation: "next_period_lifecycle_change", Support: billing.SupportUnsupported, Reason: "resume_next_period_not_supported"}
		}
		return billing.Capability{Operation: "next_period_lifecycle_change", Support: billing.SupportSupported}
	default:
		return billing.Capability{Operation: "subscription_lifecycle", Support: billing.SupportUnsupported, Reason: "effective_from_not_supported"}
	}
}

func AutomaticTopUpCapability() billing.Capability {
	return billing.Capability{Operation: "automatic_top_up", Support: billing.SupportUnsupported, Reason: "automatic_top_up"}
}

func (c *Client) PauseSubscription(ctx context.Context, in PauseInput) (Subscription, error) {
	return c.changeSubscription(ctx, "pause", in.Subscription, in.When)
}

func (c *Client) ResumeSubscription(ctx context.Context, in ResumeInput) (Subscription, error) {
	return c.changeSubscription(ctx, "resume", in.Subscription, in.When)
}

func (c *Client) CancelSubscription(ctx context.Context, in CancelInput) (Subscription, error) {
	return c.changeSubscription(ctx, "cancel", in.Subscription, in.When)
}

func (c *Client) changeSubscription(ctx context.Context, operation string, ref billing.Reference, when EffectiveFrom) (Subscription, error) {
	if c == nil || ref.Scope != c.scope || !paddlewire.ID(ref.ID, "sub_") {
		return Subscription{}, ErrInvalid
	}
	if err := billing.RequireSupported(SubscriptionLifecycleCapability(operation)); err != nil {
		return Subscription{}, err
	}
	if err := billing.RequireSupported(EffectiveFromCapability(operation, when)); err != nil {
		return Subscription{}, err
	}
	request := struct {
		EffectiveFrom string `json:"effective_from"`
	}{string(when)}
	var wire paddlewire.Subscription
	if err := c.request(ctx, http.MethodPost, "/subscriptions/"+ref.ID+"/"+operation, request, &wire); err != nil {
		return Subscription{}, err
	}
	out, err := c.subscription(wire)
	if err != nil || out.Reference != ref {
		return Subscription{}, errors.Join(ErrResponse, ErrUncertain)
	}
	// A subscription can already carry an unrelated scheduled change, so the
	// response must name the operation that was requested, not merely show
	// that some change is scheduled.
	if when == EffectiveNextPeriod && out.ScheduledChange.Action != ScheduledAction(operation) {
		return Subscription{}, errors.Join(ErrResponse, ErrUncertain)
	}
	return out, nil
}
