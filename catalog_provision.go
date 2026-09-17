package paddle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
)

// catalogKeyField is the provider custom-data field that carries the host's own
// key for a product or price.
//
// Names are not identity: two products may legitimately be called "Pro", and a
// merchant account may be shared with other software. The host's key is the
// only thing that survives a rename, so reconciliation matches on it and
// nothing else.
const catalogKeyField = "billing_catalog_key"

// catalogPageSize is the provider's listing page size for reconciliation.
const catalogPageSize = 100

// maxCatalogObjects bounds a reconciliation walk. A merchant account with more
// products or prices than this is not one this should silently half-read.
const maxCatalogObjects = 10_000

// BillingInterval is a recurring price's period.
type BillingInterval struct {
	// Unit is "day", "week", "month" or "year".
	Unit string
	// Frequency is how many Units one billing period spans.
	Frequency int64
}

// Recurring reports whether this interval describes a subscription rather than
// a one-off charge.
func (b BillingInterval) Recurring() bool { return b.Unit != "" }

func (b BillingInterval) valid() bool {
	if b.Unit == "" {
		return b.Frequency == 0
	}
	switch b.Unit {
	case "day", "week", "month", "year":
	default:
		return false
	}
	return b.Frequency > 0 && b.Frequency <= 365
}

// TaxMode is how a price's amount relates to tax.
type TaxMode string

const (
	// TaxInclusive means the amount already contains tax.
	TaxInclusive TaxMode = "internal"
	// TaxExclusive means tax is added to the amount at checkout.
	TaxExclusive TaxMode = "external"
	// TaxAccountDefault defers to the merchant account setting.
	TaxAccountDefault TaxMode = "account_setting"
)

func (t TaxMode) valid() bool {
	switch t {
	case TaxInclusive, TaxExclusive, TaxAccountDefault:
		return true
	}
	return false
}

// ProductSpec is a product the host declares it sells.
type ProductSpec struct {
	// Key is the host's stable identity for this product. It is written to
	// provider custom data and is what reconciliation matches on.
	Key string
	// Name is what a customer sees. It may be changed freely; it is not identity.
	Name string
	// Description is optional customer-facing copy.
	Description string
	// TaxCategory is the provider's tax classification, for example "saas" or
	// "standard". It is required: the provider computes tax from it, and
	// guessing a default here would be guessing someone's tax liability.
	TaxCategory string
}

// PriceSpec is one purchasable price of a product.
type PriceSpec struct {
	// Key is the host's stable identity for this price.
	Key string
	// ProductKey names the ProductSpec this price belongs to.
	ProductKey string
	// Description is what appears on the invoice line.
	Description string
	// Currency is the ISO 4217 code, for example "EUR".
	Currency string
	// Amount is the price in the currency's minor units.
	Amount int64
	// Interval is the billing period. A zero Interval is a one-off charge.
	Interval BillingInterval
	// Tax is how Amount relates to tax.
	Tax TaxMode
}

// ProviderCatalog is the full set of money objects a host declares.
//
// It is declarative and reconciled as a whole so that the amounts a host quotes
// and the amounts the provider settles come from one source. A host that
// instead pastes provider price ids into configuration is maintaining the same
// number in two places, and the day they disagree a customer is charged
// something they were never shown.
type ProviderCatalog struct {
	Products []ProductSpec
	Prices   []PriceSpec
}

// CatalogMapping is where each declared key ended up in the provider.
type CatalogMapping struct {
	Products map[string]billing.Reference
	Prices   map[string]billing.Reference
}

// Price returns the provider price reference for a declared key.
func (m CatalogMapping) Price(key string) (billing.Reference, bool) {
	ref, ok := m.Prices[key]
	return ref, ok
}

// ErrCatalogDrift reports a provider object that exists under a declared key
// but does not match what was declared.
//
// It is never repaired automatically. A live subscription is billed against its
// price, so editing one silently changes what existing customers pay; the
// provider's own answer to a price change is a new price, and choosing when to
// do that is a commercial decision, not a deployment step.
var ErrCatalogDrift = errors.New("paddle: declared catalog does not match the provider")

// ErrCatalogAmbiguous reports two provider objects claiming the same host key.
var ErrCatalogAmbiguous = errors.New("paddle: two provider objects claim one catalog key")

func (in ProviderCatalog) validate() error {
	if len(in.Products) == 0 || len(in.Products) > 500 || len(in.Prices) == 0 || len(in.Prices) > 2000 {
		return ErrInvalid
	}
	products := make(map[string]struct{}, len(in.Products))
	for _, product := range in.Products {
		if !billing.ValidID(product.Key) || product.Name == "" || len(product.Name) > 200 ||
			len(product.Description) > 2000 || !billing.ValidID(product.TaxCategory) {
			return ErrInvalid
		}
		if _, seen := products[product.Key]; seen {
			return ErrInvalid
		}
		products[product.Key] = struct{}{}
	}
	prices := make(map[string]struct{}, len(in.Prices))
	for _, price := range in.Prices {
		if !billing.ValidID(price.Key) || price.Description == "" || len(price.Description) > 200 ||
			len(price.Currency) != 3 || price.Amount < 0 || !price.Interval.valid() || !price.Tax.valid() {
			return ErrInvalid
		}
		if _, ok := products[price.ProductKey]; !ok {
			return ErrInvalid
		}
		if _, seen := prices[price.Key]; seen {
			return ErrInvalid
		}
		prices[price.Key] = struct{}{}
	}
	return nil
}

// ReconcileCatalog makes the provider's catalog match the declared one and
// reports where each key lives.
//
// It is idempotent and additive: missing objects are created, matching ones are
// left alone, and one that exists but differs is reported as ErrCatalogDrift
// rather than edited. Nothing is ever deleted or archived — withdrawing a price
// that live subscriptions renew against is a commercial decision.
//
// This writes to the financial system of record, so it is an operator action:
// run it at deploy time, not on a request path.
func (c *Client) ReconcileCatalog(ctx context.Context, in ProviderCatalog) (CatalogMapping, error) {
	if c == nil {
		return CatalogMapping{}, ErrInvalid
	}
	if err := in.validate(); err != nil {
		return CatalogMapping{}, err
	}

	existingProducts, err := c.listProducts(ctx)
	if err != nil {
		return CatalogMapping{}, err
	}
	out := CatalogMapping{
		Products: make(map[string]billing.Reference, len(in.Products)),
		Prices:   make(map[string]billing.Reference, len(in.Prices)),
	}
	productIDs := make(map[string]string, len(in.Products))
	for _, spec := range in.Products {
		found, ok := existingProducts[spec.Key]
		if !ok {
			found, err = c.createProduct(ctx, spec)
			if err != nil {
				return CatalogMapping{}, err
			}
		} else if found.TaxCategory != spec.TaxCategory {
			// Tax category decides what the customer is charged on top of the
			// price. A mismatch is not cosmetic.
			return CatalogMapping{}, fmt.Errorf("%w: product %q tax category", ErrCatalogDrift, spec.Key)
		}
		productIDs[spec.Key] = found.ID
		out.Products[spec.Key] = billing.Reference{Scope: c.scope, ID: found.ID}
	}

	existingPrices, err := c.listPrices(ctx)
	if err != nil {
		return CatalogMapping{}, err
	}
	for _, spec := range in.Prices {
		productID := productIDs[spec.ProductKey]
		found, ok := existingPrices[spec.Key]
		if !ok {
			found, err = c.createPrice(ctx, spec, productID)
			if err != nil {
				return CatalogMapping{}, err
			}
		} else if err := priceMatches(found, spec, productID); err != nil {
			return CatalogMapping{}, err
		}
		out.Prices[spec.Key] = billing.Reference{Scope: c.scope, ID: found.ID}
	}
	return out, nil
}

// priceMatches reports whether a provider price still says what was declared.
func priceMatches(got paddlewire.Price, want PriceSpec, productID string) error {
	if got.ProductID != productID {
		return fmt.Errorf("%w: price %q belongs to another product", ErrCatalogDrift, want.Key)
	}
	if got.UnitPrice.Currency != want.Currency {
		return fmt.Errorf("%w: price %q currency", ErrCatalogDrift, want.Key)
	}
	amount, err := strconv.ParseInt(got.UnitPrice.Amount, 10, 64)
	if err != nil || amount != want.Amount {
		return fmt.Errorf("%w: price %q amount", ErrCatalogDrift, want.Key)
	}
	if got.TaxMode != string(want.Tax) {
		return fmt.Errorf("%w: price %q tax mode", ErrCatalogDrift, want.Key)
	}
	switch {
	case want.Interval.Recurring():
		if got.BillingCycle == nil || got.BillingCycle.Interval != want.Interval.Unit ||
			got.BillingCycle.Frequency != want.Interval.Frequency {
			return fmt.Errorf("%w: price %q billing cycle", ErrCatalogDrift, want.Key)
		}
	case got.BillingCycle != nil:
		return fmt.Errorf("%w: price %q is recurring but was declared one-off", ErrCatalogDrift, want.Key)
	}
	return nil
}

func (c *Client) createProduct(ctx context.Context, spec ProductSpec) (paddlewire.Product, error) {
	request := struct {
		Name        string            `json:"name"`
		Description string            `json:"description,omitempty"`
		TaxCategory string            `json:"tax_category"`
		Type        string            `json:"type"`
		CustomData  map[string]string `json:"custom_data"`
	}{spec.Name, spec.Description, spec.TaxCategory, "standard", map[string]string{catalogKeyField: spec.Key}}
	var out paddlewire.Product
	if err := c.request(ctx, http.MethodPost, "/products", request, &out); err != nil {
		return paddlewire.Product{}, err
	}
	// An uncertain create is the dangerous case: the object may exist without
	// this process knowing its id, and a blind retry would make a second one.
	// Refusing to guess leaves the next run to find it by key.
	if !paddlewire.ID(out.ID, "pro_") || out.CustomData[catalogKeyField] != spec.Key || out.TaxCategory != spec.TaxCategory {
		return paddlewire.Product{}, errors.Join(ErrResponse, ErrUncertain)
	}
	return out, nil
}

func (c *Client) createPrice(ctx context.Context, spec PriceSpec, productID string) (paddlewire.Price, error) {
	request := struct {
		ProductID    string                   `json:"product_id"`
		Description  string                   `json:"description"`
		UnitPrice    paddlewire.UnitPrice     `json:"unit_price"`
		BillingCycle *paddlewire.BillingCycle `json:"billing_cycle,omitempty"`
		TaxMode      string                   `json:"tax_mode"`
		CustomData   map[string]string        `json:"custom_data"`
	}{
		ProductID:   productID,
		Description: spec.Description,
		UnitPrice:   paddlewire.UnitPrice{Amount: strconv.FormatInt(spec.Amount, 10), Currency: spec.Currency},
		TaxMode:     string(spec.Tax),
		CustomData:  map[string]string{catalogKeyField: spec.Key},
	}
	if spec.Interval.Recurring() {
		request.BillingCycle = &paddlewire.BillingCycle{Interval: spec.Interval.Unit, Frequency: spec.Interval.Frequency}
	}
	var out paddlewire.Price
	if err := c.request(ctx, http.MethodPost, "/prices", request, &out); err != nil {
		return paddlewire.Price{}, err
	}
	if !paddlewire.ID(out.ID, "pri_") || out.CustomData[catalogKeyField] != spec.Key {
		return paddlewire.Price{}, errors.Join(ErrResponse, ErrUncertain)
	}
	// The echoed object is the only evidence the provider stored what was asked
	// for. Accepting it unchecked would let a silently coerced amount through.
	if err := priceMatches(out, spec, productID); err != nil {
		return paddlewire.Price{}, errors.Join(err, ErrUncertain)
	}
	return out, nil
}

func (c *Client) listProducts(ctx context.Context) (map[string]paddlewire.Product, error) {
	out := make(map[string]paddlewire.Product)
	after := ""
	seen := 0
	for {
		values := url.Values{}
		values.Set("order_by", "id[ASC]")
		values.Set("per_page", strconv.Itoa(catalogPageSize))
		values.Set("status", "active")
		if after != "" {
			values.Set("after", after)
		}
		var wire struct {
			Data []paddlewire.Product `json:"data"`
		}
		if err := c.request(ctx, http.MethodGet, "/products?"+values.Encode(), nil, &wire); err != nil {
			return nil, err
		}
		for _, item := range wire.Data {
			if !paddlewire.ID(item.ID, "pro_") || item.ID <= after {
				return nil, ErrResponse
			}
			after = item.ID
			key := item.CustomData[catalogKeyField]
			if key == "" {
				// Not ours. A shared merchant account is normal.
				continue
			}
			if _, duplicate := out[key]; duplicate {
				return nil, fmt.Errorf("%w: product key %q", ErrCatalogAmbiguous, key)
			}
			out[key] = item
		}
		seen += len(wire.Data)
		if len(wire.Data) < catalogPageSize {
			return out, nil
		}
		if seen > maxCatalogObjects {
			return nil, ErrResponse
		}
	}
}

func (c *Client) listPrices(ctx context.Context) (map[string]paddlewire.Price, error) {
	out := make(map[string]paddlewire.Price)
	after := ""
	seen := 0
	for {
		values := url.Values{}
		values.Set("order_by", "id[ASC]")
		values.Set("per_page", strconv.Itoa(catalogPageSize))
		values.Set("status", "active")
		if after != "" {
			values.Set("after", after)
		}
		var wire struct {
			Data []paddlewire.Price `json:"data"`
		}
		if err := c.request(ctx, http.MethodGet, "/prices?"+values.Encode(), nil, &wire); err != nil {
			return nil, err
		}
		for _, item := range wire.Data {
			if !paddlewire.ID(item.ID, "pri_") || item.ID <= after {
				return nil, ErrResponse
			}
			after = item.ID
			key := item.CustomData[catalogKeyField]
			if key == "" {
				continue
			}
			if _, duplicate := out[key]; duplicate {
				return nil, fmt.Errorf("%w: price key %q", ErrCatalogAmbiguous, key)
			}
			out[key] = item
		}
		seen += len(wire.Data)
		if len(wire.Data) < catalogPageSize {
			return out, nil
		}
		if seen > maxCatalogObjects {
			return nil, ErrResponse
		}
	}
}
