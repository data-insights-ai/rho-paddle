package paddle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"

	"github.com/data-insights-ai/rho-billing/provider"
)

// ChargeKey is the custom data a charge's key travels in. Paddle's charge
// has no idempotency key; the key rides on the charge's custom price, so
// only a charge at a custom price can be found again.
const ChargeKey = "charge_key"

// ErrChargeLookup: a charge is looked up by its subscription and key.
var ErrChargeLookup = errors.New("paddle: a charge lookup needs the subscription and the key")

// FindCharge looks up the charge with key on the subscription: its custom
// price under the subscription's product, then that price on the next
// renewal (scheduled) or on one of the subscription's transactions
// (billed). A price Paddle has but no renewal or transaction carries is a
// charge that did not take.
func (c *Client) FindCharge(ctx context.Context, subscription, key string) (provider.ChargeStatus, error) {
	if subscription == "" || key == "" {
		return provider.ChargeStatus{}, ErrChargeLookup
	}
	product, err := c.subscriptionProduct(ctx, subscription)
	if err != nil || product == "" {
		return provider.ChargeStatus{}, err
	}
	price, err := c.keyedPrice(ctx, product, key)
	if err != nil || price == "" {
		return provider.ChargeStatus{}, err
	}
	var next struct {
		Data struct {
			NextTransaction *wireLines `json:"next_transaction"`
		} `json:"data"`
	}
	if err := c.call(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(subscription)+"?include=next_transaction", nil, &next); err != nil {
		return provider.ChargeStatus{}, err
	}
	if t := next.Data.NextTransaction; t != nil && t.bills(price) {
		return provider.ChargeStatus{Found: true}, nil
	}
	var billed string
	query := url.Values{"subscription_id": {subscription}, "order_by": {"created_at[DESC]"}, "per_page": {"200"}}
	err = walk(ctx, c, "/transactions?"+query.Encode(), func(page []wireLines) bool {
		for _, t := range page {
			if t.Status != "canceled" && t.bills(price) {
				billed = t.ID
				return true
			}
		}
		return false
	})
	return provider.ChargeStatus{Found: billed != "", Payment: billed}, err
}

func (c *Client) keyedPrice(ctx context.Context, product, key string) (string, error) {
	var found string
	query := url.Values{"product_id": {product}, "type": {"custom"}, "order_by": {"id[DESC]"}, "per_page": {"200"}}
	err := walk(ctx, c, "/prices?"+query.Encode(), func(page []struct {
		ID         string            `json:"id"`
		CustomData map[string]string `json:"custom_data"`
	}) bool {
		for _, p := range page {
			if p.CustomData[ChargeKey] == key {
				found = p.ID
				return true
			}
		}
		return false
	})
	return found, err
}

type wireLines struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Details struct {
		LineItems []wireLine `json:"line_items"`
	} `json:"details"`
}

type wireLine struct {
	ID       string `json:"id"`
	PriceID  string `json:"price_id"`
	Quantity int    `json:"quantity"`
	Totals   struct {
		Total string `json:"total"`
	} `json:"totals"`
}

func (t wireLines) bills(price string) bool {
	return slices.ContainsFunc(t.Details.LineItems, func(l wireLine) bool { return l.PriceID == price })
}

// walk reads a list page by page until visit says stop or none is left.
func walk[T any](ctx context.Context, c *Client, path string, visit func([]T) bool) error {
	for range maxPages {
		var page struct {
			Data json.RawMessage `json:"data"`
			Meta struct {
				Pagination struct {
					Next    string `json:"next"`
					HasMore bool   `json:"has_more"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := c.call(ctx, http.MethodGet, path, nil, &page); err != nil {
			return err
		}
		var items []T
		if err := json.Unmarshal(page.Data, &items); err != nil {
			return err
		}
		if visit(items) || !page.Meta.Pagination.HasMore {
			return nil
		}
		next, err := url.Parse(page.Meta.Pagination.Next)
		if err != nil {
			return err
		}
		path = next.Path + "?" + next.RawQuery
	}
	return ErrInvalid
}
