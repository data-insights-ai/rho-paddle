package paddle

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/data-insights-ai/rho-billing/provider"
	"github.com/data-insights-ai/rho-billing/subscription"
)

var ErrMalformed = errors.New("paddle: malformed event")

type envelope struct {
	EventID    string          `json:"event_id"`
	EventType  string          `json:"event_type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

type wireItem struct {
	Quantity int `json:"quantity"`
	Price    struct {
		ID string `json:"id"`
	} `json:"price"`
}

type wirePeriod struct {
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

type wireSubscription struct {
	ID                   string            `json:"id"`
	Status               string            `json:"status"`
	CustomerID           string            `json:"customer_id"`
	Items                []wireItem        `json:"items"`
	CustomData           map[string]string `json:"custom_data"`
	CurrentBillingPeriod *wirePeriod       `json:"current_billing_period"`
	ScheduledChange      *struct {
		Action      string    `json:"action"`
		EffectiveAt time.Time `json:"effective_at"`
	} `json:"scheduled_change"`
}

type wireTransaction struct {
	ID             string            `json:"id"`
	CustomerID     string            `json:"customer_id"`
	SubscriptionID string            `json:"subscription_id"`
	CurrencyCode   string            `json:"currency_code"`
	Items          []wireItem        `json:"items"`
	CustomData     map[string]string `json:"custom_data"`
	BilledAt       time.Time         `json:"billed_at"`
	Payments       []struct {
		Status    string `json:"status"`
		ErrorCode string `json:"error_code"`
	} `json:"payments"`
	Details struct {
		Totals struct {
			GrandTotal string `json:"grand_total"`
		} `json:"totals"`
	} `json:"details"`
}

// Decode turns a verified webhook body into an event; kinds a host does
// not need answer provider.ErrIgnored.
func Decode(body []byte) (provider.Event, error) {
	var e envelope
	if err := json.Unmarshal(body, &e); err != nil || e.EventID == "" || e.OccurredAt.IsZero() {
		return provider.Event{}, ErrMalformed
	}
	out := provider.Event{ID: e.EventID, OccurredAt: e.OccurredAt}
	switch {
	case strings.HasPrefix(e.EventType, "subscription."):
		var s wireSubscription
		if err := json.Unmarshal(e.Data, &s); err != nil || s.ID == "" {
			return provider.Event{}, ErrMalformed
		}
		sub := toSubscription(s, subscription.Revision{At: e.OccurredAt, EventID: e.EventID})
		out.Kind, out.Subscription = provider.SubscriptionChanged, &sub
	case paymentKinds[e.EventType] != "":
		p, err := toPayment(e.Data)
		if err != nil {
			return provider.Event{}, err
		}
		out.Kind, out.Payment = paymentKinds[e.EventType], p
	case e.EventType == "adjustment.created" || e.EventType == "adjustment.updated":
		a, err := toAdjustment(e.Data)
		if err != nil {
			return provider.Event{}, err
		}
		out.Kind, out.Adjustment = provider.PaymentAdjusted, a
	default:
		return out, provider.ErrIgnored
	}
	return out, nil
}

func toSubscription(s wireSubscription, rev subscription.Revision) subscription.Subscription {
	out := subscription.Subscription{
		ID: s.ID, Customer: s.CustomerID, Status: subscription.Status(s.Status), Items: items(s.Items),
		Reference: s.CustomData[ReferenceKey], Revision: rev,
	}
	if p := s.CurrentBillingPeriod; p != nil {
		out.PeriodStart, out.PeriodEnd = p.StartsAt, p.EndsAt
	}
	if c := s.ScheduledChange; c != nil && c.Action == "cancel" {
		out.CancelAt = c.EffectiveAt
	}
	return out
}

func items(in []wireItem) []subscription.Item {
	out := make([]subscription.Item, len(in))
	for i, it := range in {
		out[i] = subscription.Item{PriceID: it.Price.ID, Quantity: it.Quantity}
	}
	return out
}

var paymentKinds = map[string]provider.Kind{
	"transaction.completed":      provider.PaymentCompleted,
	"transaction.payment_failed": provider.PaymentFailed,
	"transaction.canceled":       provider.PaymentCanceled,
}

func toPayment(data json.RawMessage) (*provider.Payment, error) {
	var t wireTransaction
	if err := json.Unmarshal(data, &t); err != nil || t.ID == "" {
		return nil, ErrMalformed
	}
	total, err := amount(t.Details.Totals.GrandTotal)
	if err != nil {
		return nil, err
	}
	p := &provider.Payment{
		ID: t.ID, Customer: t.CustomerID, Subscription: t.SubscriptionID, Items: items(t.Items),
		Reference: t.CustomData[ReferenceKey], Currency: t.CurrencyCode, Total: total, BilledAt: t.BilledAt,
	}
	for _, attempt := range t.Payments {
		if attempt.Status == "error" || attempt.Status == "failed" {
			p.Failure = attempt.ErrorCode
			break
		}
	}
	return p, nil
}

type wireAdjustment struct {
	ID             string `json:"id"`
	Action         string `json:"action"`
	TransactionID  string `json:"transaction_id"`
	SubscriptionID string `json:"subscription_id"`
	CustomerID     string `json:"customer_id"`
	CurrencyCode   string `json:"currency_code"`
	Status         string `json:"status"`
	Items          []struct {
		ItemID string `json:"item_id"`
		Type   string `json:"type"`
		Totals struct {
			Total string `json:"total"`
		} `json:"totals"`
	} `json:"items"`
	Totals struct {
		Total string `json:"total"`
	} `json:"totals"`
}

func toAdjustment(data json.RawMessage) (*provider.Adjustment, error) {
	var w wireAdjustment
	if err := json.Unmarshal(data, &w); err != nil || w.ID == "" || w.TransactionID == "" {
		return nil, ErrMalformed
	}
	total, err := amount(w.Totals.Total)
	if err != nil {
		return nil, err
	}
	a := &provider.Adjustment{
		ID: w.ID, Payment: w.TransactionID, Subscription: w.SubscriptionID, Customer: w.CustomerID,
		Action: provider.AdjustmentAction(w.Action), Status: provider.AdjustmentStatus(w.Status), Currency: w.CurrencyCode, Total: total,
	}
	for _, it := range w.Items {
		line, err := amount(it.Totals.Total)
		if err != nil {
			return nil, err
		}
		a.Items = append(a.Items, provider.AdjustedLine{Line: it.ItemID, Full: it.Type == "full", Total: line})
	}
	return a, nil
}

// amount reads Paddle's decimal string in the currency's minor unit.
func amount(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, ErrMalformed
	}
	return n, nil
}
