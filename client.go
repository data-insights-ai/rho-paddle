package paddle

import (
	"errors"
	"net/http"
	"time"

	"github.com/data-insights-ai/rho-billing/provider"

	"github.com/data-insights-ai/rho-paddle/internal/paddlehttp"
)

// Environment selects Paddle's sandbox or live API.
type Environment string

const (
	Sandbox Environment = "sandbox"
	Live    Environment = "live"
)

var (
	ErrInvalid = errors.New("paddle: invalid configuration")
	// ErrUncertain: a mutation may have reached Paddle (a timeout after the
	// request, a malformed success). Never retry it blindly.
	ErrUncertain = paddlehttp.ErrUncertain
	// ErrRateLimited: Paddle asked to slow down; the client does not wait.
	ErrRateLimited = paddlehttp.ErrRateLimited
)

// ReferenceKey is the custom data field that carries the host's reference
// on transactions, subscriptions, products and prices.
const ReferenceKey = "reference"

type Config struct {
	Environment Environment
	APIKey      string
	// WebhookSecrets are the notification destination's secrets; more than
	// one while a secret is rotated.
	WebhookSecrets []string
	HTTPClient     *http.Client
}

// Client talks to one Paddle account.
type Client struct {
	api     *paddlehttp.Client
	secrets []string
	now     func() time.Time
}

var _ provider.Provider = (*Client)(nil)

func New(c Config) (*Client, error) {
	base := map[Environment]string{Sandbox: "https://sandbox-api.paddle.com", Live: "https://api.paddle.com"}[c.Environment]
	if base == "" || len(c.WebhookSecrets) == 0 {
		return nil, ErrInvalid
	}
	api, err := paddlehttp.New(paddlehttp.Config{APIKey: c.APIKey, BaseURL: base, HTTPClient: c.HTTPClient})
	if err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	return &Client{api: api, secrets: c.WebhookSecrets, now: time.Now}, nil
}
