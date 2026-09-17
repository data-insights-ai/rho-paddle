package paddle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"io"
	"slices"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-paddle/internal/signature"
)

// Event is authenticated provider evidence, not a grant or proof of tenant
// ownership. Notification/destination IDs do not form its idempotency identity.
//
// The provider payload is deliberately not exported. Handing hosts raw Paddle
// JSON would make Paddle's schema part of this module's compatibility contract,
// so a provider rename would break host resolvers at runtime instead of at this
// module's compile boundary. Use the accessors below.
type Event struct {
	ID         string
	Type       string
	OccurredAt time.Time
	data       json.RawMessage
}

// MarshalJSON and UnmarshalJSON pin the durable inbox payload shape. It must
// not depend on which fields happen to be exported: the payload is persisted
// evidence that later worker runs decode, so unexporting the provider bytes
// would otherwise have silently emptied every stored event.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(eventPayload{ID: e.ID, Type: e.Type, OccurredAt: e.OccurredAt, Data: e.data})
}

func (e *Event) UnmarshalJSON(raw []byte) error {
	var out eventPayload
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	e.ID, e.Type, e.OccurredAt, e.data = out.ID, out.Type, out.OccurredAt, out.Data
	return nil
}

type eventPayload struct {
	ID         string          `json:"event_id"`
	Type       string          `json:"event_type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

// eventIdentity is the subset of a provider payload a host needs to bind an
// event to an account it already knows.
type eventIdentity struct {
	ID             string `json:"id"`
	CustomerID     string `json:"customer_id"`
	SubscriptionID string `json:"subscription_id"`
	TransactionID  string `json:"transaction_id"`
	CustomData     struct {
		Account   string `json:"billing_account_id"`
		Operation string `json:"billing_operation_id"`
		IntentID  string `json:"billing_intent_id"`
	} `json:"custom_data"`
}

func (e Event) identity() eventIdentity {
	var out eventIdentity
	if len(e.data) != 0 {
		_ = json.Unmarshal(e.data, &out)
	}
	return out
}

// ObjectID is the provider id of the object the event is about.
func (e Event) ObjectID() string { return e.identity().ID }

// CustomerID is the Paddle customer the event concerns, empty when absent. It
// is the authoritative key for a host's own customer binding.
func (e Event) CustomerID() string { return e.identity().CustomerID }

// SubscriptionID is the Paddle subscription the event concerns, if any.
func (e Event) SubscriptionID() string { return e.identity().SubscriptionID }

// TransactionID is the Paddle transaction the event concerns, if any.
func (e Event) TransactionID() string { return e.identity().TransactionID }

// Correlation returns the host identity this adapter wrote into custom_data
// when it created the object. It is correlation evidence, not authorization:
// custom_data is attacker-influencable on objects this adapter did not create,
// so resolve accounts through a stored provider binding wherever one exists.
func (e Event) Correlation() (account billing.AccountID, operation, intentID string) {
	id := e.identity()
	return billing.AccountID(id.CustomData.Account), id.CustomData.Operation, id.CustomData.IntentID
}

// AccountResolver binds verified events to an existing host account using
// authoritative local provider references. Do not trust arbitrary checkout
// custom_data or a matching customer email as authorization.
type AccountResolver interface {
	ResolveAccount(context.Context, billing.Scope, Event) (billing.AccountID, error)
}

// WebhookConfig binds a notification destination's secrets to one provider scope.
// During rotation supply old and new secrets briefly, then remove the old secret.
type WebhookConfig struct {
	Merchant    string
	Environment Environment
	Secrets     []string
	Resolver    AccountResolver
	Now         func() time.Time
	Tolerance   time.Duration
}

// WebhookVerifier implements the billing core's authenticated inbox boundary.
type WebhookVerifier struct {
	scope     billing.Scope
	secrets   []string
	resolver  AccountResolver
	now       func() time.Time
	tolerance time.Duration
}

// NewWebhookVerifier performs no requests or worker startup. The default replay
// tolerance is five seconds. Bodies are limited by integration.MaxWebhookBody.
func NewWebhookVerifier(config WebhookConfig) (*WebhookVerifier, error) {
	scope, err := providerScope(config.Merchant, config.Environment)
	if err != nil || config.Resolver == nil || len(config.Secrets) == 0 || len(config.Secrets) > 4 {
		return nil, ErrInvalid
	}
	for _, secret := range config.Secrets {
		if len(secret) == 0 || len(secret) > 4096 {
			return nil, ErrInvalid
		}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Tolerance == 0 {
		config.Tolerance = 5 * time.Second
	}
	if config.Tolerance < 0 || config.Tolerance > 5*time.Minute {
		return nil, ErrInvalid
	}
	return &WebhookVerifier{scope: scope, secrets: slices.Clone(config.Secrets), resolver: config.Resolver, now: config.Now, tolerance: config.Tolerance}, nil
}

// Verify authenticates raw bytes before decoding or resolving tenant ownership.
// Call integration.ReceiveWebhook to commit the returned message before HTTP 200.
func (v *WebhookVerifier) Verify(ctx context.Context, raw []byte, headers integration.WebhookHeaders) (integration.Message, error) {
	if v == nil || v.resolver == nil || v.now == nil || len(raw) > integration.MaxWebhookBody {
		return integration.Message{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return integration.Message{}, err
	}
	var header string
	var count int
	for key, values := range headers {
		if strings.EqualFold(key, "Paddle-Signature") {
			count += len(values)
			if len(values) == 1 {
				header = values[0]
			}
		}
	}
	if count != 1 {
		return integration.Message{}, ErrSignature
	}
	if err := signature.Verify(raw, header, v.secrets, v.now(), v.tolerance); err != nil {
		if errors.Is(err, signature.ErrTimestamp) {
			return integration.Message{}, ErrTimestamp
		}
		return integration.Message{}, ErrSignature
	}
	event, err := decodeEvent(raw)
	if err != nil {
		return integration.Message{}, err
	}
	// The resolver receives a detached payload and cannot alter retained evidence.
	resolvedEvent := event
	resolvedEvent.data = bytes.Clone(event.data)
	account, err := v.resolver.ResolveAccount(ctx, v.scope, resolvedEvent)
	if err != nil {
		return integration.Message{}, err
	}
	if !billing.ValidID(string(account)) {
		return integration.Message{}, ErrInvalid
	}
	payload, err := json.Marshal(event)
	if err != nil || len(payload) > integration.MaxWebhookBody {
		return integration.Message{}, ErrInvalid
	}
	message := integration.Message{Account: account, ID: event.ID, Scope: v.scope, Direction: integration.Inbound, Kind: event.Type, OccurredAt: event.OccurredAt, Payload: payload}
	if err := message.Validate(); err != nil {
		return integration.Message{}, err
	}
	return message, nil
}

func decodeEvent(raw []byte) (Event, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return Event{}, ErrInvalid
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Event{}, ErrInvalid
	}
	object, ok := value.(map[string]any)
	if !ok {
		return Event{}, ErrInvalid
	}
	id, okID := object["event_id"].(string)
	kind, okKind := object["event_type"].(string)
	stamp, okStamp := object["occurred_at"].(string)
	data, okData := object["data"].(map[string]any)
	if !okID || !okKind || !okStamp || !okData || !paddlewire.ID(id, "evt_") || !billing.ValidID(kind) {
		return Event{}, ErrInvalid
	}
	when, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || when.IsZero() {
		return Event{}, ErrInvalid
	}
	body, err := json.Marshal(data)
	if err != nil {
		return Event{}, ErrInvalid
	}
	return Event{ID: id, Type: kind, OccurredAt: billing.CanonicalTime(when), data: body}, nil
}

// Reject duplicate JSON keys before decoding security- or money-relevant fields.
func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, ErrInvalid
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return token, nil
	}
	switch delim {
	case '{':
		out := make(map[string]any)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, ErrInvalid
			}
			if _, exists := out[name]; exists {
				return nil, ErrInvalid
			}
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			out[name] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrInvalid
		}
		return out, nil
	case '[':
		out := make([]any, 0)
		for decoder.More() {
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrInvalid
		}
		return out, nil
	}
	return nil, ErrInvalid
}

var _ integration.WebhookVerifier = (*WebhookVerifier)(nil)
