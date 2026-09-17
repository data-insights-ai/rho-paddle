package paddlewire

// Product is the provider's product object.
type Product struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description *string           `json:"description"`
	TaxCategory string            `json:"tax_category"`
	Type        string            `json:"type"`
	Status      string            `json:"status"`
	CustomData  map[string]string `json:"custom_data"`
}

// BillingCycle is a recurring price's period.
type BillingCycle struct {
	Interval  string `json:"interval"`
	Frequency int64  `json:"frequency"`
}

// UnitPrice is an amount in a currency's minor units, carried as a string
// because the provider refuses to put money in a JSON number.
type UnitPrice struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency_code"`
}

// Price is the provider's price object.
type Price struct {
	ID           string            `json:"id"`
	ProductID    string            `json:"product_id"`
	Description  string            `json:"description"`
	Type         string            `json:"type"`
	BillingCycle *BillingCycle     `json:"billing_cycle"`
	TaxMode      string            `json:"tax_mode"`
	UnitPrice    UnitPrice         `json:"unit_price"`
	Status       string            `json:"status"`
	CustomData   map[string]string `json:"custom_data"`
}

// Customer is the provider's customer object.
type Customer struct {
	ID         string            `json:"id"`
	Email      string            `json:"email"`
	Status     string            `json:"status"`
	CustomData map[string]string `json:"custom_data"`
}
