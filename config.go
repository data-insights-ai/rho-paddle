package paddle

import (
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"net/http"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-paddle/internal/paddlehttp"
)

// Environment selects a fixed Paddle API origin and provider namespace.
type Environment string

const (
	Sandbox Environment = "sandbox"
	Live    Environment = "live"
)

var (
	ErrInvalid   = errors.New("paddle: invalid configuration or input")
	ErrSignature = errors.New("paddle: invalid webhook signature")
	ErrTimestamp = errors.New("paddle: webhook timestamp outside tolerance")
	// ErrUncertain means a mutation may have reached Paddle. Persist unknown state
	// and reconcile; retrying automatically may create another financial action.
	ErrUncertain = errors.New("paddle: provider mutation outcome is unknown")
	// ErrResponse is defined by the internal wire package and re-exported here:
	// there must be exactly one value, or a consumer matching on it would miss
	// failures raised during decoding.
	ErrResponse    = paddlewire.ErrResponse
	ErrRateLimited = errors.New("paddle: provider rate limited")
)

// Config pins a client to one merchant and environment. The host supplies the
// API key from its secret store. A custom HTTPClient must have a trusted transport.
type Config struct {
	Merchant    string
	Environment Environment
	APIKey      string
	HTTPClient  *http.Client
}

// Client is a scoped Paddle API client. It performs no automatic retries.
type Client struct {
	scope billing.Scope
	http  *paddlehttp.Client
}

func providerScope(merchant string, environment Environment) (billing.Scope, error) {
	if !billing.ValidID(merchant) || (environment != Sandbox && environment != Live) {
		return billing.Scope{}, ErrInvalid
	}
	return billing.Scope{Provider: "paddle", Merchant: merchant, Environment: string(environment)}, nil
}

// New validates configuration without provider requests or worker startup.
func New(config Config) (*Client, error) {
	scope, err := providerScope(config.Merchant, config.Environment)
	if err != nil {
		return nil, err
	}
	origin := "https://api.paddle.com"
	if config.Environment == Sandbox {
		origin = "https://sandbox-api.paddle.com"
	}
	// A live key against the sandbox origin (or the reverse) is a deployment
	// mistake that would otherwise surface only as provider rejections on
	// every mutation. Keys created before Paddle prefixed them carry neither.
	if err := keyMatchesEnvironment(config.APIKey, config.Environment); err != nil {
		return nil, err
	}
	transport, err := paddlehttp.New(paddlehttp.Config{APIKey: config.APIKey, BaseURL: origin, HTTPClient: config.HTTPClient})
	if err != nil {
		return nil, ErrInvalid
	}
	return &Client{scope: scope, http: transport}, nil
}

func (c *Client) Scope() billing.Scope {
	if c == nil {
		return billing.Scope{}
	}
	return c.scope
}

func keyMatchesEnvironment(key string, environment Environment) error {
	const (
		livePrefix    = "pdl_live_apikey_"
		sandboxPrefix = "pdl_sdbx_apikey_"
	)
	switch {
	case strings.HasPrefix(key, livePrefix):
		if environment != Live {
			return ErrInvalid
		}
	case strings.HasPrefix(key, sandboxPrefix):
		if environment != Sandbox {
			return ErrInvalid
		}
	}
	return nil
}
