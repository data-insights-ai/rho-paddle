package paddle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-paddle/internal/paddlehttp"
)

// APIError contains safe provider diagnostics. It never contains request bodies,
// credentials, customer details or authenticated checkout/portal URLs.
type APIError struct {
	StatusCode int
	Code       string
	RequestID  string
	RetryAfter time.Duration
	uncertain  bool
	cause      error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("paddle: API status %d code %s request %s", e.StatusCode, e.Code, e.RequestID)
}

func (e *APIError) Is(target error) bool {
	if e == nil {
		return false
	}
	if e.uncertain && target == ErrUncertain {
		return true
	}
	return e.StatusCode == http.StatusTooManyRequests && target == ErrRateLimited
}

// Unwrap preserves context cancellation without exposing transport diagnostics.
func (e *APIError) Unwrap() error { return e.cause }

// CustomerReference binds a Paddle customer to its host billing account. Hosts
// must persist and authorize this binding; email matching is not ownership.
type CustomerReference struct {
	Account   billing.AccountID
	Reference billing.Reference
}

// CustomerInput includes a durable host operation identity for reconciliation.
// Operation is correlation metadata, not a provider idempotency guarantee.
type CustomerInput struct {
	Account   billing.AccountID
	Operation string
	Email     string
	Name      string
}

// CreateCustomer creates one customer without automatic retry. Persist an
// unknown command if errors.Is(err, ErrUncertain) before attempting recovery.
func (c *Client) CreateCustomer(ctx context.Context, in CustomerInput) (CustomerReference, error) {
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.Operation) || len(in.Email) > 320 || len(in.Name) > 1024 {
		return CustomerReference{}, ErrInvalid
	}
	address, err := mail.ParseAddress(in.Email)
	if err != nil || address.Address != in.Email {
		return CustomerReference{}, ErrInvalid
	}
	request := struct {
		Email      string            `json:"email"`
		Name       string            `json:"name,omitempty"`
		CustomData map[string]string `json:"custom_data"`
	}{in.Email, in.Name, map[string]string{"billing_account_id": string(in.Account), "billing_operation_id": in.Operation}}
	var data struct {
		ID string `json:"id"`
	}
	if err := c.request(ctx, http.MethodPost, "/customers", request, &data); err != nil {
		return CustomerReference{}, err
	}
	if !paddlewire.ID(data.ID, "ctm_") {
		return CustomerReference{}, errors.Join(ErrResponse, ErrUncertain)
	}
	return CustomerReference{Account: in.Account, Reference: billing.Reference{Scope: c.scope, ID: data.ID}}, nil
}

// CheckoutItem selects an existing Paddle catalog price. Price references must
// be in this client's scope. Hosts own mapping approved commercial revisions to
// provider prices; the adapter does not infer entitlement policy from price IDs.
type CheckoutItem struct {
	Price    billing.Reference
	Quantity int64
}

// CheckoutCorrelation is the frozen host identity carried in Paddle custom data.
// All fields are required when a correlation is supplied.
type CheckoutCorrelation struct {
	Account            billing.AccountID
	Operation          string
	IntentID           string
	QuoteFingerprint   string
	RequestFingerprint string
	Schema             string
}

func (c CheckoutCorrelation) valid(account billing.AccountID, operation string) bool {
	return billing.ValidID(string(c.Account)) && c.Account == account && billing.ValidID(c.Operation) && c.Operation == operation && billing.ValidID(c.IntentID) && paddlewire.Fingerprint(c.QuoteFingerprint) && paddlewire.Fingerprint(c.RequestFingerprint) && c.Schema == "rho.checkout.v1"
}

type CheckoutInput struct {
	// CheckoutPageURL is a public host page with Paddle.js, not an authenticated payment link.
	// Empty uses the merchant default. The host owns domain approval and the page.
	CheckoutPageURL string
	Customer        CustomerReference
	Operation       string
	Items           []CheckoutItem
	Correlation     *CheckoutCorrelation
}

type TransactionStatus string

const (
	TransactionDraft     TransactionStatus = "draft"
	TransactionReady     TransactionStatus = "ready"
	TransactionBilled    TransactionStatus = "billed"
	TransactionPaid      TransactionStatus = "paid"
	TransactionCompleted TransactionStatus = "completed"
	TransactionCanceled  TransactionStatus = "canceled"
	TransactionPastDue   TransactionStatus = "past_due"
)

// Transaction is a provider handoff/read model. Status is provider evidence;
// creation, acceptance or a URL must never be treated as captured payment.
type Transaction struct {
	Reference      billing.Reference
	Customer       billing.Reference
	Subscription   billing.Reference
	Status         TransactionStatus
	Currency       string
	CheckoutURL    string
	Items          []TransactionItem
	Origin         string
	CollectionMode CollectionMode
	CreatedAt      time.Time
	Correlation    CheckoutCorrelation
}

type TransactionItem struct {
	Reference billing.Reference
	Price     billing.Reference
	Quantity  int64
}

// CreateCheckout creates an automatically collected transaction. The resulting
// transaction may remain draft until checkout collects required customer details.
func (c *Client) CreateCheckout(ctx context.Context, in CheckoutInput) (Transaction, error) {
	if c == nil || !checkoutPageURL(in.CheckoutPageURL) || !c.ValidCustomer(in.Customer) || !billing.ValidID(in.Operation) || len(in.Items) == 0 || len(in.Items) > 100 || (in.Correlation != nil && !in.Correlation.valid(in.Customer.Account, in.Operation)) {
		return Transaction{}, ErrInvalid
	}
	type item struct {
		PriceID  string `json:"price_id"`
		Quantity int64  `json:"quantity"`
	}
	items := make([]item, len(in.Items))
	for i, line := range in.Items {
		if line.Price.Scope != c.scope || !paddlewire.ID(line.Price.ID, "pri_") || line.Quantity <= 0 {
			return Transaction{}, ErrInvalid
		}
		items[i] = item{line.Price.ID, line.Quantity}
	}
	customData := map[string]string{"billing_account_id": string(in.Customer.Account), "billing_operation_id": in.Operation}
	if in.Correlation != nil {
		customData["billing_checkout_schema"] = in.Correlation.Schema
		customData["billing_intent_id"] = in.Correlation.IntentID
		customData["billing_quote_fingerprint"] = in.Correlation.QuoteFingerprint
		customData["billing_request_fingerprint"] = in.Correlation.RequestFingerprint
	}
	request := struct {
		CustomerID     string                   `json:"customer_id"`
		CollectionMode string                   `json:"collection_mode"`
		Items          []item                   `json:"items"`
		CustomData     map[string]string        `json:"custom_data"`
		Checkout       *paddlewire.CheckoutPage `json:"checkout,omitempty"`
	}{CustomerID: in.Customer.Reference.ID, CollectionMode: "automatic", Items: items, CustomData: customData}
	if in.CheckoutPageURL != "" {
		request.Checkout = &paddlewire.CheckoutPage{URL: in.CheckoutPageURL}
	}
	var wire paddlewire.Transaction
	if err := c.request(ctx, http.MethodPost, "/transactions", request, &wire); err != nil {
		return Transaction{}, err
	}
	out, err := c.transaction(wire)
	if err != nil || out.Customer.ID != in.Customer.Reference.ID || out.Origin != "api" || out.CollectionMode != CollectionAutomatic || (in.Correlation != nil && out.Correlation != *in.Correlation) {
		return Transaction{}, errors.Join(ErrResponse, ErrUncertain)
	}
	return out, nil
}

// Transaction fetches current provider state by an already known scoped identity.
// It does not resolve an unknown create when no transaction ID is available.
func (c *Client) Transaction(ctx context.Context, ref billing.Reference) (Transaction, error) {
	if c == nil || ref.Scope != c.scope || !paddlewire.ID(ref.ID, "txn_") {
		return Transaction{}, ErrInvalid
	}
	var wire paddlewire.Transaction
	if err := c.request(ctx, http.MethodGet, "/transactions/"+ref.ID, nil, &wire); err != nil {
		return Transaction{}, err
	}
	out, err := c.transaction(wire)
	if err != nil || out.Reference != ref {
		return Transaction{}, ErrResponse
	}
	return out, nil
}

func (c *Client) transaction(w paddlewire.Transaction) (Transaction, error) {
	if !paddlewire.ID(w.ID, "txn_") || (w.CustomerID != "" && !paddlewire.ID(w.CustomerID, "ctm_")) || len(w.Currency) != 3 || strings.ToUpper(w.Currency) != w.Currency {
		return Transaction{}, ErrResponse
	}
	for _, ch := range w.Currency {
		if ch < 'A' || ch > 'Z' {
			return Transaction{}, ErrResponse
		}
	}
	switch TransactionStatus(w.Status) {
	case TransactionDraft, TransactionReady, TransactionBilled, TransactionPaid, TransactionCompleted, TransactionCanceled, TransactionPastDue:
	default:
		return Transaction{}, ErrResponse
	}
	if w.CreatedAt.IsZero() || len(w.Details.LineItems) > 100 {
		return Transaction{}, ErrResponse
	}
	// An unrecognized collection mode must not reach callers as provider
	// evidence; only the list endpoint post-filters it, and only for one origin.
	if CollectionMode(w.CollectionMode) != CollectionAutomatic && CollectionMode(w.CollectionMode) != CollectionManual {
		return Transaction{}, ErrResponse
	}
	out := Transaction{Reference: billing.Reference{Scope: c.scope, ID: w.ID}, Status: TransactionStatus(w.Status), Currency: w.Currency, Origin: w.Origin, CollectionMode: CollectionMode(w.CollectionMode), CreatedAt: billing.CanonicalTime(w.CreatedAt)}
	if w.CustomerID != "" {
		out.Customer = billing.Reference{Scope: c.scope, ID: w.CustomerID}
	}
	if w.SubscriptionID != "" {
		if !paddlewire.ID(w.SubscriptionID, "sub_") {
			return Transaction{}, ErrResponse
		}
		out.Subscription = billing.Reference{Scope: c.scope, ID: w.SubscriptionID}
	}
	seen := make(map[string]struct{}, len(w.Details.LineItems))
	// Pre-sized: a recovery scan parses thousands of transactions, each with up
	// to a hundred line items.
	out.Items = make([]TransactionItem, 0, len(w.Details.LineItems))
	for _, item := range w.Details.LineItems {
		priceID := item.PriceID
		if !paddlewire.ID(item.ID, "txnitm_") || !paddlewire.ID(priceID, "pri_") || item.Quantity <= 0 {
			return Transaction{}, ErrResponse
		}
		if _, ok := seen[item.ID]; ok {
			return Transaction{}, ErrResponse
		}
		seen[item.ID] = struct{}{}
		out.Items = append(out.Items, TransactionItem{Reference: billing.Reference{Scope: c.scope, ID: item.ID}, Price: billing.Reference{Scope: c.scope, ID: priceID}, Quantity: item.Quantity})
	}
	correlation, err := parseCorrelation(w.CustomData)
	if err != nil {
		return Transaction{}, err
	}
	out.Correlation = correlation
	if w.Checkout != nil && w.Checkout.URL != "" {
		if !paddlewire.HTTPSURL(w.Checkout.URL) {
			return Transaction{}, ErrResponse
		}
		out.CheckoutURL = w.Checkout.URL
	}
	return out, nil
}

func parseCorrelation(raw map[string]json.RawMessage) (CheckoutCorrelation, error) {
	data := make(map[string]string)
	for _, key := range []string{"billing_account_id", "billing_operation_id", "billing_checkout_schema", "billing_intent_id", "billing_quote_fingerprint", "billing_request_fingerprint"} {
		if value, ok := raw[key]; ok {
			var decoded string
			if err := json.Unmarshal(value, &decoded); err != nil {
				return CheckoutCorrelation{}, ErrResponse
			}
			data[key] = decoded
		}
	}
	if len(data) == 0 {
		return CheckoutCorrelation{}, nil
	}
	keys := []string{"billing_checkout_schema", "billing_intent_id", "billing_quote_fingerprint", "billing_request_fingerprint"}
	present := false
	for _, key := range keys {
		if data[key] != "" {
			present = true
		}
	}
	if !present {
		return CheckoutCorrelation{}, nil
	}
	out := CheckoutCorrelation{Account: billing.AccountID(data["billing_account_id"]), Operation: data["billing_operation_id"], IntentID: data["billing_intent_id"], QuoteFingerprint: data["billing_quote_fingerprint"], RequestFingerprint: data["billing_request_fingerprint"], Schema: data["billing_checkout_schema"]}
	if !out.valid(out.Account, out.Operation) {
		return CheckoutCorrelation{}, ErrResponse
	}
	return out, nil
}

// PortalSession is an ephemeral authenticated handoff. Do not persist or log its
// URLs. The host must authorize the supplied customer and subscription references.
type PortalSession struct {
	Customer      CustomerReference
	OverviewURL   string
	Subscriptions []PortalSubscription
}

type PortalSubscription struct {
	Reference              billing.Reference
	CancelURL              string
	UpdatePaymentMethodURL string
}

func (c *Client) CreatePortalSession(ctx context.Context, customer CustomerReference, subscriptions []billing.Reference) (PortalSession, error) {
	if c == nil || !c.ValidCustomer(customer) || len(subscriptions) > 25 {
		return PortalSession{}, ErrInvalid
	}
	ids := make([]string, len(subscriptions))
	selected := make(map[string]bool, len(subscriptions))
	for i, ref := range subscriptions {
		if ref.Scope != c.scope || !paddlewire.ID(ref.ID, "sub_") || selected[ref.ID] {
			return PortalSession{}, ErrInvalid
		}
		ids[i], selected[ref.ID] = ref.ID, true
	}
	request := struct {
		Subscriptions []string `json:"subscription_ids"`
	}{ids}
	var wire struct {
		CustomerID string `json:"customer_id"`
		URLs       struct {
			General struct {
				Overview string `json:"overview"`
			} `json:"general"`
			Subscriptions []struct {
				ID      string `json:"id"`
				Cancel  string `json:"cancel_subscription"`
				Payment string `json:"update_subscription_payment_method"`
			} `json:"subscriptions"`
		} `json:"urls"`
	}
	if err := c.request(ctx, http.MethodPost, "/customers/"+customer.Reference.ID+"/portal-sessions", request, &wire); err != nil {
		return PortalSession{}, err
	}
	if wire.CustomerID != customer.Reference.ID || !paddlewire.HTTPSURL(wire.URLs.General.Overview) || len(wire.URLs.Subscriptions) != len(ids) {
		return PortalSession{}, errors.Join(ErrResponse, ErrUncertain)
	}
	out := PortalSession{Customer: customer, OverviewURL: wire.URLs.General.Overview, Subscriptions: make([]PortalSubscription, 0, len(ids))}
	for _, sub := range wire.URLs.Subscriptions {
		if !selected[sub.ID] || !paddlewire.HTTPSURL(sub.Cancel) || !paddlewire.HTTPSURL(sub.Payment) {
			return PortalSession{}, errors.Join(ErrResponse, ErrUncertain)
		}
		delete(selected, sub.ID)
		out.Subscriptions = append(out.Subscriptions, PortalSubscription{Reference: billing.Reference{Scope: c.scope, ID: sub.ID}, CancelURL: sub.Cancel, UpdatePaymentMethodURL: sub.Payment})
	}
	return out, nil
}

// ValidCustomer reports whether a customer reference was minted by this client:
// a valid host account, this client's provider scope, and a Paddle customer id.
func (c *Client) ValidCustomer(customer CustomerReference) bool {
	return billing.ValidID(string(customer.Account)) && customer.Reference.Scope == c.scope && paddlewire.ID(customer.Reference.ID, "ctm_")
}

func checkoutPageURL(raw string) bool {
	if raw == "" {
		return true
	}
	if len(raw) > 2048 || !paddlewire.HTTPSURL(raw) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.RawQuery == "" && !u.ForceQuery
}

func (c *Client) request(ctx context.Context, method, path string, input, output any) error {
	if c == nil || c.http == nil {
		return ErrInvalid
	}
	var raw []byte
	if input != nil {
		var err error
		raw, err = json.Marshal(input)
		if err != nil {
			return ErrInvalid
		}
	}
	response, err := c.http.Do(ctx, method, path, raw)
	if err != nil {
		if api, ok := errors.AsType[*paddlehttp.Error](err); ok {
			var cause error
			if errors.Is(err, context.Canceled) {
				cause = context.Canceled
			}
			if errors.Is(err, context.DeadlineExceeded) {
				cause = context.DeadlineExceeded
			}
			return &APIError{StatusCode: api.StatusCode, Code: api.Code, RequestID: api.RequestID, RetryAfter: api.RetryAfter, uncertain: errors.Is(err, paddlehttp.ErrUncertain), cause: cause}
		}
		var cause error
		switch {
		case errors.Is(err, context.Canceled):
			cause = context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			cause = context.DeadlineExceeded
		default:
			cause = ErrResponse
		}
		if errors.Is(err, paddlehttp.ErrUncertain) {
			return errors.Join(ErrUncertain, cause)
		}
		if errors.Is(err, paddlehttp.ErrInvalid) {
			return ErrInvalid
		}
		return cause
	}
	if output == nil || (method == http.MethodDelete && len(response.Body) == 0) {
		return nil
	}
	if err := decodeResponse(response.Body, output); err != nil {
		if method != http.MethodGet {
			return errors.Join(ErrResponse, ErrUncertain)
		}
		return ErrResponse
	}
	return nil
}

func decodeResponse(raw []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return ErrResponse
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrResponse
	}
	object, ok := value.(map[string]any)
	if !ok {
		return ErrResponse
	}
	data, ok := object["data"]
	if !ok {
		return ErrResponse
	}
	valueToDecode := data
	if _, isList := data.([]any); isList {
		valueToDecode = object
	}
	encoded, err := json.Marshal(valueToDecode)
	if err != nil {
		return ErrResponse
	}
	if err := json.Unmarshal(encoded, output); err != nil {
		return ErrResponse
	}
	return nil
}
