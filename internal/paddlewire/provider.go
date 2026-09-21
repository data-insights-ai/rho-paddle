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
	BilledAt       *time.Time                 `json:"billed_at"`
	CustomData     map[string]json.RawMessage `json:"custom_data"`
	Details        struct {
		Totals *struct {
			GrandTotal string `json:"grand_total"`
			Tax        string `json:"tax"`
		} `json:"totals"`
		LineItems []struct {
			ID       string `json:"id"`
			PriceID  string `json:"price_id"`
			Quantity int64  `json:"quantity"`
		} `json:"line_items"`
	} `json:"details"`
	InvoiceNumber string `json:"invoice_number"`
	Checkout      *struct {
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

// PaymentLine is one line of a transaction. It is named rather than
// anonymous so that a caller can build one without restating the whole
// shape, which is how adding a field here used to break every test that
// constructed a transaction by hand.
type PaymentLine struct {
	ID       string `json:"id"`
	PriceID  string `json:"price_id"`
	Quantity int64  `json:"quantity"`
	Totals   struct {
		Subtotal string `json:"subtotal"`
		Discount string `json:"discount"`
		Total    string `json:"total"`
		Tax      string `json:"tax"`
	} `json:"totals"`
}

type Payment struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Status     string `json:"status"`
	Currency   string `json:"currency_code"`
	Details    struct {
		Totals struct {
			// Subtotal is before tax and before any discount; Discount is
			// what the provider took off. Total is subtotal minus discount
			// plus tax, which is what the customer owes.
			Subtotal        string `json:"subtotal"`
			Discount        string `json:"discount"`
			Total           string `json:"total"`
			Tax             string `json:"tax"`
			Credit          string `json:"credit"`
			CreditToBalance string `json:"credit_to_balance"`
			GrandTotal      string `json:"grand_total"`
			Balance         string `json:"balance"`
			Currency        string `json:"currency_code"`
		} `json:"totals"`
		Lines []PaymentLine `json:"line_items"`
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

// Money is an amount with its currency, as the provider states totals.
type Money struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
}

// SubscriptionPreview is the provider's answer to a previewed update.
type SubscriptionPreview struct {
	CurrencyCode  string `json:"currency_code"`
	NextBilledAt  string `json:"next_billed_at"`
	UpdateSummary *struct {
		Result struct {
			Action       string `json:"action"`
			Amount       string `json:"amount"`
			CurrencyCode string `json:"currency_code"`
		} `json:"result"`
		Charge Money `json:"charge"`
		Credit Money `json:"credit"`
	} `json:"update_summary"`
	RecurringTransactionDetails *struct {
		Totals struct {
			Total string `json:"total"`
		} `json:"totals"`
	} `json:"recurring_transaction_details"`
}
