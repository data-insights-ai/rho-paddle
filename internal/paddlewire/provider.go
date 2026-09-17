package paddlewire

import (
	"encoding/json"
	"time"
)

// The Paddle envelopes for transactions, subscriptions, payments and
// notification settings, exactly as they arrive on the wire.

type Transaction struct {
	ID             string                     `json:"id"`
	CustomerID     string                     `json:"customer_id"`
	SubscriptionID string                     `json:"subscription_id"`
	Status         string                     `json:"status"`
	Currency       string                     `json:"currency_code"`
	Origin         string                     `json:"origin"`
	CollectionMode string                     `json:"collection_mode"`
	CreatedAt      time.Time                  `json:"created_at"`
	CustomData     map[string]json.RawMessage `json:"custom_data"`
	Details        struct {
		LineItems []struct {
			ID       string `json:"id"`
			PriceID  string `json:"price_id"`
			Quantity int64  `json:"quantity"`
		} `json:"line_items"`
	} `json:"details"`
	Checkout *struct {
		URL string `json:"url"`
	} `json:"checkout"`
}

type Subscription struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	CustomerID     string `json:"customer_id"`
	CollectionMode string `json:"collection_mode"`
	Items          []struct {
		Quantity int64 `json:"quantity"`
		Price    struct {
			ID string `json:"id"`
		} `json:"price"`
	} `json:"items"`
	ScheduledChange *struct {
		Action      string  `json:"action"`
		EffectiveAt string  `json:"effective_at"`
		ResumeAt    *string `json:"resume_at"`
	} `json:"scheduled_change"`
}

type Payment struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Status     string `json:"status"`
	Currency   string `json:"currency_code"`
	Details    struct {
		Totals struct {
			Total           string `json:"total"`
			Tax             string `json:"tax"`
			Credit          string `json:"credit"`
			CreditToBalance string `json:"credit_to_balance"`
			GrandTotal      string `json:"grand_total"`
			Balance         string `json:"balance"`
			Currency        string `json:"currency_code"`
		} `json:"totals"`
		Lines []struct {
			ID       string `json:"id"`
			PriceID  string `json:"price_id"`
			Quantity int64  `json:"quantity"`
			Totals   struct {
				Total string `json:"total"`
				Tax   string `json:"tax"`
			} `json:"totals"`
		} `json:"line_items"`
	} `json:"details"`
	Payments []struct {
		ID         string     `json:"payment_attempt_id"`
		Status     string     `json:"status"`
		Amount     string     `json:"amount"`
		CreatedAt  time.Time  `json:"created_at"`
		CapturedAt *time.Time `json:"captured_at"`
	} `json:"payments"`
}

type NotificationSetting struct {
	ID                string `json:"id"`
	Description       string `json:"description"`
	Type              string `json:"type"`
	Destination       string `json:"destination"`
	Active            bool   `json:"active"`
	EndpointSecretKey string `json:"endpoint_secret_key"`
	SubscribedEvents  []struct {
		Name string `json:"name"`
	} `json:"subscribed_events"`
}

// CheckoutPage is the public Paddle.js page a transaction is handed off to.
type CheckoutPage struct {
	URL string `json:"url"`
}
