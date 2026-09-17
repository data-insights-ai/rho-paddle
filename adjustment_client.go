package paddle

import (
	"context"
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"net/http"
	"net/url"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type AdjustmentAction string

const (
	AdjustmentRefund                   AdjustmentAction = "refund"
	AdjustmentCredit                   AdjustmentAction = "credit"
	AdjustmentChargeback               AdjustmentAction = "chargeback"
	AdjustmentChargebackReverse        AdjustmentAction = "chargeback_reverse"
	AdjustmentChargebackWarning        AdjustmentAction = "chargeback_warning"
	AdjustmentChargebackWarningReverse AdjustmentAction = "chargeback_warning_reverse"
	AdjustmentCreditReverse            AdjustmentAction = "credit_reverse"
)

type AdjustmentStatus string

const (
	AdjustmentPendingApproval AdjustmentStatus = "pending_approval"
	AdjustmentApproved        AdjustmentStatus = "approved"
	AdjustmentRejected        AdjustmentStatus = "rejected"
	AdjustmentReversed        AdjustmentStatus = "reversed"
)

type AdjustmentType string

const (
	AdjustmentFull      AdjustmentType = "full"
	AdjustmentPartial   AdjustmentType = "partial"
	AdjustmentTax       AdjustmentType = "tax"
	AdjustmentProration AdjustmentType = "proration"
)

// Adjustment is a scoped Paddle adjustment snapshot. Totals are customer
// gross/tax evidence; Paddle fees are omitted.
type Adjustment struct {
	Reference            billing.Reference
	Action               AdjustmentAction
	Status               AdjustmentStatus
	Type                 AdjustmentType
	Transaction          billing.Reference
	Customer             billing.Reference
	Currency             string
	Reason               string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	Items                []AdjustmentLine
	Subtotal, Tax, Gross int64
}

type AdjustmentLine struct {
	Reference            billing.Reference
	TransactionItem      billing.Reference
	Type                 AdjustmentType
	Subtotal, Tax, Gross int64
}

type AdjustmentCreateItem struct {
	TransactionItemID string
	Type              AdjustmentType
	Amount            string
}

type AdjustmentCreate struct {
	Action AdjustmentAction
	Type   AdjustmentType
	// Transaction carries provider scope so a reference minted by one merchant
	// or environment cannot be refunded through another client.
	Transaction billing.Reference
	Reason      string
	Items       []AdjustmentCreateItem
}

type AdjustmentLookup struct {
	Transaction billing.Reference
	Customer    billing.Reference
	Action      AdjustmentAction
	Status      AdjustmentStatus
	After       string
	Limit       int
}

type AdjustmentPage struct {
	Items     []Adjustment
	HasMore   bool
	NextAfter string
}

// CreateAdjustment submits one refund adjustment. Credit actions fail closed
// before provider I/O. A successful create is not proof the refund is approved.
func (c *Client) CreateAdjustment(ctx context.Context, in AdjustmentCreate) (Adjustment, error) {
	if c == nil || in.Action != AdjustmentRefund || (in.Type != AdjustmentFull && in.Type != AdjustmentPartial) || in.Transaction.Scope != c.scope || !paddlewire.ID(in.Transaction.ID, "txn_") || !billing.ValidID(in.Reason) {
		if c != nil && in.Action == AdjustmentCredit {
			return Adjustment{}, adjustmentUnresolved("provider_credit_adjustment_unresolved")
		}
		return Adjustment{}, ErrInvalid
	}
	if in.Type == AdjustmentPartial && (len(in.Items) == 0 || len(in.Items) > 100) {
		return Adjustment{}, ErrInvalid
	}
	type item struct {
		ItemID string `json:"item_id"`
		Type   string `json:"type"`
		Amount string `json:"amount,omitempty"`
	}
	var source []AdjustmentCreateItem
	if in.Type != AdjustmentFull {
		source = in.Items
	}
	items := make([]item, len(source))
	seen := make(map[string]struct{}, len(source))
	for i, line := range source {
		if !paddlewire.ID(line.TransactionItemID, "txnitm_") || (line.Type != AdjustmentFull && line.Type != AdjustmentPartial) {
			return Adjustment{}, ErrInvalid
		}
		if _, ok := seen[line.TransactionItemID]; ok {
			return Adjustment{}, ErrInvalid
		}
		seen[line.TransactionItemID] = struct{}{}
		if line.Type == AdjustmentPartial {
			amount, err := paddlewire.MinorUnits(line.Amount)
			if err != nil || amount <= 0 {
				return Adjustment{}, ErrInvalid
			}
		}
		if line.Type == AdjustmentFull && line.Amount != "" {
			return Adjustment{}, ErrInvalid
		}
		items[i] = item{ItemID: line.TransactionItemID, Type: string(line.Type), Amount: line.Amount}
	}
	request := struct {
		Action        string `json:"action"`
		Type          string `json:"type"`
		TransactionID string `json:"transaction_id"`
		Reason        string `json:"reason"`
		Items         []item `json:"items,omitempty"`
	}{Action: string(AdjustmentRefund), Type: string(in.Type), TransactionID: in.Transaction.ID, Reason: in.Reason, Items: items}
	var wire paddlewire.Adjustment
	if err := c.request(ctx, http.MethodPost, "/adjustments", request, &wire); err != nil {
		return Adjustment{}, err
	}
	out, err := c.adjustment(wire)
	if err != nil || out.Transaction.ID != in.Transaction.ID || out.Action != AdjustmentRefund || out.Reason != in.Reason {
		return Adjustment{}, errors.Join(ErrResponse, ErrUncertain)
	}
	return out, nil
}

func (c *Client) Adjustment(ctx context.Context, ref billing.Reference) (Adjustment, error) {
	if c == nil || ref.Scope != c.scope || !paddlewire.ID(ref.ID, "adj_") {
		return Adjustment{}, ErrInvalid
	}
	var wire paddlewire.Adjustment
	if err := c.request(ctx, http.MethodGet, "/adjustments/"+ref.ID, nil, &wire); err != nil {
		return Adjustment{}, err
	}
	out, err := c.adjustment(wire)
	if err != nil || out.Reference != ref {
		return Adjustment{}, ErrResponse
	}
	return out, nil
}

// Adjustments lists adjustments for one scoped transaction. The cursor is an
// opaque provider identity and is never followed as a URL.
func (c *Client) Adjustments(ctx context.Context, query AdjustmentLookup) (AdjustmentPage, error) {
	if c == nil || query.Transaction.Scope != c.scope || !paddlewire.ID(query.Transaction.ID, "txn_") || query.Limit < 0 || query.Limit > 50 || (query.After != "" && !paddlewire.ID(query.After, "adj_")) {
		return AdjustmentPage{}, ErrInvalid
	}
	if query.Customer.ID != "" && (query.Customer.Scope != c.scope || !paddlewire.ID(query.Customer.ID, "ctm_")) {
		return AdjustmentPage{}, ErrInvalid
	}
	if query.Action != "" {
		switch query.Action {
		case AdjustmentRefund, AdjustmentCredit, AdjustmentChargeback, AdjustmentChargebackReverse, AdjustmentChargebackWarning, AdjustmentChargebackWarningReverse, AdjustmentCreditReverse:
		default:
			return AdjustmentPage{}, ErrInvalid
		}
	}
	if query.Status != "" {
		switch query.Status {
		case AdjustmentPendingApproval, AdjustmentApproved, AdjustmentRejected, AdjustmentReversed:
		default:
			return AdjustmentPage{}, ErrInvalid
		}
	}
	limit := query.Limit
	if limit == 0 {
		limit = 50
	}
	values := url.Values{}
	values.Set("transaction_id", query.Transaction.ID)
	values.Set("order_by", "id[ASC]")
	values.Set("per_page", strconv.Itoa(limit))
	if query.After != "" {
		values.Set("after", query.After)
	}
	if query.Action != "" {
		values.Set("action", string(query.Action))
	}
	if query.Status != "" {
		values.Set("status", string(query.Status))
	}
	if query.Customer.ID != "" {
		values.Set("customer_id", query.Customer.ID)
	}
	var wire struct {
		Data []paddlewire.Adjustment `json:"data"`
		Meta *struct {
			Pagination *struct {
				HasMore *bool  `json:"has_more"`
				Next    string `json:"next"`
			} `json:"pagination"`
		} `json:"meta"`
	}
	if err := c.request(ctx, http.MethodGet, "/adjustments?"+values.Encode(), nil, &wire); err != nil {
		return AdjustmentPage{}, err
	}
	if len(wire.Data) > limit || wire.Meta == nil || wire.Meta.Pagination == nil || wire.Meta.Pagination.HasMore == nil {
		return AdjustmentPage{}, ErrResponse
	}
	nextAfter, ok := c.adjustmentCursor(wire.Meta.Pagination.Next)
	if wire.Meta.Pagination.Next != "" && !ok {
		return AdjustmentPage{}, ErrResponse
	}
	page := AdjustmentPage{HasMore: *wire.Meta.Pagination.HasMore, NextAfter: nextAfter}
	if page.HasMore {
		if len(wire.Data) == 0 || !paddlewire.ID(page.NextAfter, "adj_") {
			return AdjustmentPage{}, ErrResponse
		}
	}
	previous := query.After
	for _, item := range wire.Data {
		out, err := c.adjustment(item)
		if err != nil || out.Transaction.ID != query.Transaction.ID || (previous != "" && out.Reference.ID <= previous) {
			return AdjustmentPage{}, ErrResponse
		}
		if query.Customer.ID != "" && out.Customer.ID != query.Customer.ID {
			return AdjustmentPage{}, ErrResponse
		}
		if query.Action != "" && out.Action != query.Action {
			return AdjustmentPage{}, ErrResponse
		}
		if query.Status != "" && out.Status != query.Status {
			return AdjustmentPage{}, ErrResponse
		}
		previous = out.Reference.ID
		page.Items = append(page.Items, out)
	}
	if page.HasMore && page.NextAfter != previous {
		return AdjustmentPage{}, ErrResponse
	}
	if !page.HasMore {
		page.NextAfter = ""
	}
	return page, nil
}

func (c *Client) adjustment(w paddlewire.Adjustment) (Adjustment, error) {
	if !paddlewire.ID(w.ID, "adj_") || !paddlewire.ID(w.TransactionID, "txn_") || !paddlewire.ID(w.CustomerID, "ctm_") || len(w.Currency) != 3 || w.CreatedAt.IsZero() || w.UpdatedAt.Before(w.CreatedAt) || len(w.Items) == 0 || len(w.Items) > 100 {
		return Adjustment{}, ErrResponse
	}
	switch AdjustmentAction(w.Action) {
	case AdjustmentRefund, AdjustmentCredit, AdjustmentChargeback, AdjustmentChargebackReverse, AdjustmentChargebackWarning, AdjustmentChargebackWarningReverse, AdjustmentCreditReverse:
	default:
		return Adjustment{}, ErrResponse
	}
	switch AdjustmentStatus(w.Status) {
	case AdjustmentPendingApproval, AdjustmentApproved, AdjustmentRejected, AdjustmentReversed:
	default:
		return Adjustment{}, ErrResponse
	}
	if w.Type != "" && AdjustmentType(w.Type) != AdjustmentFull && AdjustmentType(w.Type) != AdjustmentPartial {
		return Adjustment{}, ErrResponse
	}
	subtotal, tax, total, err := adjustmentMoney(w.Totals)
	if err != nil {
		return Adjustment{}, err
	}
	out := Adjustment{
		Reference:   billing.Reference{Scope: c.scope, ID: w.ID},
		Action:      AdjustmentAction(w.Action),
		Status:      AdjustmentStatus(w.Status),
		Type:        AdjustmentType(w.Type),
		Transaction: billing.Reference{Scope: c.scope, ID: w.TransactionID},
		Customer:    billing.Reference{Scope: c.scope, ID: w.CustomerID},
		Currency:    w.Currency,
		Reason:      w.Reason,
		CreatedAt:   billing.CanonicalTime(w.CreatedAt),
		UpdatedAt:   billing.CanonicalTime(w.UpdatedAt),
		Subtotal:    subtotal,
		Tax:         tax,
		Gross:       total,
	}
	seen := make(map[string]struct{}, len(w.Items))
	var itemSubtotal, itemTax, itemTotal int64
	for _, item := range w.Items {
		if !paddlewire.ID(item.ID, "adjitm_") || !paddlewire.ID(item.ItemID, "txnitm_") {
			return Adjustment{}, ErrResponse
		}
		if _, ok := seen[item.ID]; ok {
			return Adjustment{}, ErrResponse
		}
		seen[item.ID] = struct{}{}
		if kind := AdjustmentType(item.Type); kind != AdjustmentFull && kind != AdjustmentPartial && kind != AdjustmentTax && kind != AdjustmentProration {
			return Adjustment{}, ErrResponse
		}
		lineSubtotal, lineTax, lineTotal, err := adjustmentMoney(item.Totals)
		if err != nil {
			return Adjustment{}, err
		}
		itemSubtotal, err = paddlewire.AddMoney(itemSubtotal, lineSubtotal)
		if err != nil {
			return Adjustment{}, err
		}
		itemTax, err = paddlewire.AddMoney(itemTax, lineTax)
		if err != nil {
			return Adjustment{}, err
		}
		itemTotal, err = paddlewire.AddMoney(itemTotal, lineTotal)
		if err != nil {
			return Adjustment{}, err
		}
		out.Items = append(out.Items, AdjustmentLine{
			Reference:       billing.Reference{Scope: c.scope, ID: item.ID},
			TransactionItem: billing.Reference{Scope: c.scope, ID: item.ItemID},
			Type:            AdjustmentType(item.Type),
			Subtotal:        lineSubtotal,
			Tax:             lineTax,
			Gross:           lineTotal,
		})
	}
	if itemSubtotal != subtotal || itemTax != tax || itemTotal != total {
		return Adjustment{}, ErrResponse
	}
	return out, nil
}

func (c *Client) adjustmentCursor(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Path != "/adjustments" {
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
	if err != nil || len(values["after"]) != 1 || !paddlewire.ID(values.Get("after"), "adj_") {
		return "", false
	}
	return values.Get("after"), true
}

func adjustmentWireFromListed(a Adjustment) paddlewire.Adjustment {
	items := make([]paddlewire.AdjustmentItem, len(a.Items))
	for i, item := range a.Items {
		items[i] = paddlewire.AdjustmentItem{
			ID: item.Reference.ID, ItemID: item.TransactionItem.ID, Type: string(item.Type),
			Totals: paddlewire.AdjustmentTotals{Subtotal: strconv.FormatInt(item.Subtotal, 10), Tax: strconv.FormatInt(item.Tax, 10), Total: strconv.FormatInt(item.Gross, 10), Currency: a.Currency},
		}
	}
	return paddlewire.Adjustment{
		ID: a.Reference.ID, Action: string(a.Action), Status: string(a.Status), Type: string(a.Type),
		TransactionID: a.Transaction.ID, CustomerID: a.Customer.ID, Currency: a.Currency, Reason: a.Reason,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, Items: items,
		Totals: paddlewire.AdjustmentTotals{Subtotal: strconv.FormatInt(a.Subtotal, 10), Tax: strconv.FormatInt(a.Tax, 10), Total: strconv.FormatInt(a.Gross, 10), Currency: a.Currency},
	}
}
