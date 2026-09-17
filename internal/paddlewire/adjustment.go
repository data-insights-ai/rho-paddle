package paddlewire

import "time"

// The Paddle adjustment envelope, exactly as it arrives on the wire. Nothing
// outside this package may name these shapes: that is the boundary that keeps
// provider JSON out of the adapter's exported API.

type AdjustmentTotals struct {
	Subtotal string `json:"subtotal"`
	Tax      string `json:"tax"`
	Total    string `json:"total"`
	Currency string `json:"currency_code"`
}

type AdjustmentItem struct {
	ID     string           `json:"id"`
	ItemID string           `json:"item_id"`
	Type   string           `json:"type"`
	Totals AdjustmentTotals `json:"totals"`
}

type Adjustment struct {
	ID            string           `json:"id"`
	Action        string           `json:"action"`
	Status        string           `json:"status"`
	Type          string           `json:"type"`
	TransactionID string           `json:"transaction_id"`
	CustomerID    string           `json:"customer_id"`
	Currency      string           `json:"currency_code"`
	Reason        string           `json:"reason"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Items         []AdjustmentItem `json:"items"`
	Totals        AdjustmentTotals `json:"totals"`
}
