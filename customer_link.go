package paddle

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
)

// customerExistsCode is the provider's rejection when an email is already taken.
const customerExistsCode = "customer_already_exists"

// accountField is the provider custom-data field binding a customer to the host
// account that owns it.
const accountField = "billing_account_id"

// ErrCustomerUnlinked reports a provider customer that exists for this email but
// carries no host account binding.
//
// Adopting it automatically would mean anyone who can present an email address
// inherits whatever that provider customer already has — its payment methods,
// its subscriptions and its history. Whether a pre-existing customer belongs to
// a host account is an operator's judgement, so this refuses and says so.
var ErrCustomerUnlinked = errors.New("paddle: provider customer exists but is not bound to a host account")

// ErrCustomerForeign reports a provider customer bound to a different account.
var ErrCustomerForeign = errors.New("paddle: provider customer belongs to another host account")

// LinkCustomer returns the provider customer for a host account, creating it
// only if it does not exist.
//
// CreateCustomer is the raw call and fails with the provider's
// customer_already_exists once an email has been used. That makes it correct
// exactly once per customer, which is not how checkout works: the second time
// someone subscribes — an upgrade, a renewal after cancelling, a retry of a
// failed card — the same email comes back. This is the call a checkout flow
// wants.
//
// The binding is verified, not assumed. An email is not proof of ownership, so
// a customer already bound to a different account is a conflict rather than a
// silent takeover.
func (c *Client) LinkCustomer(ctx context.Context, in CustomerInput) (CustomerReference, error) {
	if c == nil {
		return CustomerReference{}, ErrInvalid
	}
	created, err := c.CreateCustomer(ctx, in)
	if err == nil {
		return created, nil
	}
	var api *APIError
	if !errors.As(err, &api) || api.Code != customerExistsCode {
		return CustomerReference{}, err
	}
	existing, err := c.customerByEmail(ctx, in.Email)
	if err != nil {
		return CustomerReference{}, err
	}
	switch bound := existing.CustomData[accountField]; {
	case bound == string(in.Account):
		return CustomerReference{
			Account:   in.Account,
			Reference: billing.Reference{Scope: c.scope, ID: existing.ID},
		}, nil
	case bound == "":
		return CustomerReference{}, ErrCustomerUnlinked
	default:
		return CustomerReference{}, ErrCustomerForeign
	}
}

// customerByEmail resolves the single active customer holding an email.
func (c *Client) customerByEmail(ctx context.Context, email string) (paddlewire.Customer, error) {
	values := url.Values{}
	values.Set("email", email)
	values.Set("status", "active")
	values.Set("per_page", strconv.Itoa(2))
	var wire struct {
		Data []paddlewire.Customer `json:"data"`
	}
	if err := c.request(ctx, http.MethodGet, "/customers?"+values.Encode(), nil, &wire); err != nil {
		return paddlewire.Customer{}, err
	}
	// The provider rejected the create because the email was taken, so exactly
	// one must come back. Zero or two means the account changed underneath this
	// call, and picking one of them would be a guess about whose money it is.
	if len(wire.Data) != 1 {
		return paddlewire.Customer{}, ErrResponse
	}
	found := wire.Data[0]
	// Case-insensitively: the provider normalises the address it stores, so the
	// record that comes back is not byte-identical to the one that was sent.
	// Comparing exactly rejects the provider's own answer.
	if !paddlewire.ID(found.ID, "ctm_") || !strings.EqualFold(found.Email, email) || found.Status != "active" {
		return paddlewire.Customer{}, ErrResponse
	}
	return found, nil
}
