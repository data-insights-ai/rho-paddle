package paddle

import (
	"context"
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// TransactionLookup bounds one provider transaction-list request. Paddle's
// list endpoint supports at most 30 records per page.
type TransactionLookup struct {
	Customer      billing.Reference
	Subscription  billing.Reference
	Origin        string
	CreatedAfter  time.Time
	CreatedBefore time.Time
	Status        TransactionStatus
	After         string
	Limit         int
}

type TransactionPage struct {
	Items     []Transaction
	HasMore   bool
	NextAfter string
}

// OriginAny lists a customer's transactions of every origin: checkouts,
// renewals, plan changes. It is what a billing history shows; recovery
// scans keep to one origin.
const OriginAny = "any"

// Transactions lists transactions for one scoped customer. Empty Origin is
// api (checkout recovery). Use origin=subscription_charge to recover one-off
// subscription charges, and OriginAny for a customer's whole history.
// The cursor is treated as an opaque provider cursor value and is never
// followed as a URL.
func (c *Client) Transactions(ctx context.Context, query TransactionLookup) (TransactionPage, error) {
	if c == nil || query.Customer.Scope != c.scope || !paddlewire.ID(query.Customer.ID, "ctm_") || query.Limit < 0 || query.Limit > 30 || (query.After != "" && !paddlewire.ID(query.After, "txn_")) {
		return TransactionPage{}, ErrInvalid
	}
	limit := query.Limit
	if limit == 0 {
		limit = 30
	}
	if !query.CreatedAfter.IsZero() && !query.CreatedBefore.IsZero() && !query.CreatedAfter.Before(query.CreatedBefore) {
		return TransactionPage{}, ErrInvalid
	}
	if query.Status != "" {
		switch query.Status {
		case TransactionDraft, TransactionReady, TransactionBilled, TransactionPaid, TransactionCompleted, TransactionCanceled, TransactionPastDue:
		default:
			return TransactionPage{}, ErrInvalid
		}
	}
	origin := query.Origin
	if origin == "" {
		origin = "api"
	}
	switch origin {
	case "api", "subscription_charge", OriginAny:
	default:
		return TransactionPage{}, ErrInvalid
	}
	if query.Subscription.ID != "" && (query.Subscription.Scope != c.scope || !paddlewire.ID(query.Subscription.ID, "sub_")) {
		return TransactionPage{}, ErrInvalid
	}
	values := url.Values{}
	values.Set("customer_id", query.Customer.ID)
	if origin != OriginAny {
		values.Set("origin", origin)
	}
	if origin == "api" {
		values.Set("collection_mode", "automatic")
	}
	if query.Subscription.ID != "" {
		values.Set("subscription_id", query.Subscription.ID)
	}
	values.Set("order_by", "id[ASC]")
	values.Set("per_page", strconv.Itoa(limit))
	if query.After != "" {
		values.Set("after", query.After)
	}
	if query.Status != "" {
		values.Set("status", string(query.Status))
	}
	if !query.CreatedAfter.IsZero() {
		values.Set("created_at[GT]", query.CreatedAfter.UTC().Format(time.RFC3339Nano))
	}
	if !query.CreatedBefore.IsZero() {
		values.Set("created_at[LT]", query.CreatedBefore.UTC().Format(time.RFC3339Nano))
	}
	var wire struct {
		Data []paddlewire.Transaction `json:"data"`
		Meta *struct {
			Pagination *struct {
				HasMore *bool  `json:"has_more"`
				Next    string `json:"next"`
			} `json:"pagination"`
		} `json:"meta"`
	}
	if err := c.request(ctx, http.MethodGet, "/transactions?"+values.Encode(), nil, &wire); err != nil {
		return TransactionPage{}, err
	}
	if len(wire.Data) > limit || wire.Meta == nil || wire.Meta.Pagination == nil || wire.Meta.Pagination.HasMore == nil {
		return TransactionPage{}, ErrResponse
	}
	nextAfter, ok := c.transactionCursor(wire.Meta.Pagination.Next)
	if wire.Meta.Pagination.Next != "" && !ok {
		return TransactionPage{}, ErrResponse
	}
	page := TransactionPage{HasMore: *wire.Meta.Pagination.HasMore, NextAfter: nextAfter}
	if page.HasMore {
		if len(wire.Data) == 0 || !paddlewire.ID(page.NextAfter, "txn_") {
			return TransactionPage{}, ErrResponse
		}
	}
	previous := query.After
	for _, item := range wire.Data {
		out, err := c.transaction(item)
		if err != nil || out.Customer.ID != query.Customer.ID || (origin != OriginAny && out.Origin != origin) || (origin == "api" && out.CollectionMode != CollectionAutomatic) || (previous != "" && out.Reference.ID <= previous) {
			return TransactionPage{}, ErrResponse
		}
		if query.Subscription.ID != "" && out.Subscription.ID != query.Subscription.ID {
			return TransactionPage{}, ErrResponse
		}
		if (!query.CreatedAfter.IsZero() && !item.CreatedAt.After(query.CreatedAfter)) || (!query.CreatedBefore.IsZero() && !item.CreatedAt.Before(query.CreatedBefore)) || (query.Status != "" && out.Status != query.Status) {
			return TransactionPage{}, ErrResponse
		}
		previous = out.Reference.ID
		page.Items = append(page.Items, out)
	}
	if page.HasMore && page.NextAfter != previous {
		return TransactionPage{}, ErrResponse
	}
	if !page.HasMore {
		page.NextAfter = ""
	}
	return page, nil
}

func (c *Client) transactionCursor(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Path != "/transactions" {
		return "", false
	}
	host := "api.paddle.com"
	if c.scope.Environment == "sandbox" {
		host = "sandbox-api.paddle.com"
	}
	if u.Host != host || len(raw) > 16384 {
		return "", false
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(values["after"]) != 1 || !paddlewire.ID(values.Get("after"), "txn_") {
		return "", false
	}
	return values.Get("after"), true
}
