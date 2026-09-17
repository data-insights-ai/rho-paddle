package paddle

import (
	"context"
	"encoding/json"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

// scanCheckpoint is the durable cursor a recovery scan writes between pages.
// Checkout and refund recovery share it: they differ only in the provider ID
// prefix they scan and how many rows a page may carry. Keeping one definition
// keeps those two bounds from drifting apart, which previously let a change to
// one scan silently loosen the other.
type scanCheckpoint struct {
	Version    int       `json:"version"`
	MessageID  string    `json:"message_id"`
	Fence      int64     `json:"fence"`
	Query      string    `json:"query"`
	From       time.Time `json:"from"`
	Until      time.Time `json:"until"`
	Cursor     string    `json:"cursor"`
	Matches    []string  `json:"matches"`
	Conflicts  []string  `json:"conflicts"`
	Pages      int64     `json:"pages"`
	Rows       int64     `json:"rows"`
	Complete   bool      `json:"complete"`
	ObservedAt time.Time `json:"observed_at"`
	Reason     string    `json:"reason"`
}

// scanBounds fixes the per-scan differences in one place.
type scanBounds struct {
	prefix   string
	pageRows int64
}

var (
	checkoutScanBounds = scanBounds{prefix: "txn_", pageRows: 30}
	refundScanBounds   = scanBounds{prefix: "adj_", pageRows: 50}
)

func (c scanCheckpoint) valid(expected scanCheckpoint, bounds scanBounds) bool {
	if !billing.ValidID(c.MessageID) || c.Fence <= 0 || !paddlewire.Fingerprint(c.Query) || c.From.IsZero() || !c.Until.After(c.From) ||
		c.Version != 1 || c.MessageID != expected.MessageID || c.Fence != expected.Fence || c.Query != expected.Query ||
		!c.From.Equal(expected.From) || !c.Until.Equal(expected.Until) || c.Pages < 0 || c.Pages > 1_000_000 ||
		c.Rows < 0 || c.Rows > bounds.pageRows*c.Pages || len(c.Matches) > 2 || len(c.Conflicts) > 2 ||
		c.ObservedAt.IsZero() || len(c.Reason) > 128 || (c.Cursor != "" && !paddlewire.ID(c.Cursor, bounds.prefix)) {
		return false
	}
	seen := make(map[string]bool)
	for _, group := range [][]string{c.Matches, c.Conflicts} {
		for _, id := range group {
			if !paddlewire.ID(id, bounds.prefix) || seen[id] {
				return false
			}
			seen[id] = true
		}
	}
	return true
}

// saveScanCheckpoint persists a mid-scan cursor as an unknown outbox
// observation. The write must outlive a cancelled scan context, or the scan
// would repeat work it had already paid for.
func saveScanCheckpoint(ctx context.Context, repo integration.Repository, claim integration.Claim, cp scanCheckpoint, bounds scanBounds, previous string, now time.Time, commit func(observation string) func(integration.Session) error) (string, error) {
	cp.ObservedAt = billing.CanonicalTime(now)
	if !cp.valid(cp, bounds) {
		return "", ErrResponse
	}
	raw, err := json.Marshal(cp)
	if err != nil || len(raw) > 4096 {
		return "", ErrResponse
	}
	observation := checkoutObservation(claim.Message.ID, previous, string(raw))
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := repo.ResolveOutbox(persistCtx, claim, integration.OutboxResult{ObservationID: observation, ExpectedPrevious: previous, State: integration.OutboxUnknown, Evidence: string(raw)}, commit(observation)); err != nil {
		return "", err
	}
	return observation, nil
}
