package paddle

import (
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"testing"
	"time"
)

func TestCheckoutCheckpointRejectsInvalidIdentityAndProgress(t *testing.T) {
	base := scanCheckpoint{Version: 1, MessageID: "message", Fence: 1, Query: paddlewire.Digest("query"), From: time.Now().UTC(), ObservedAt: time.Now().UTC(), Pages: 1, Rows: 1, Matches: []string{"txn_abcdefghijklmnopqrstuvwxyz"}}
	base.Until = base.From.Add(time.Hour)
	if !base.valid(base, checkoutScanBounds) {
		t.Fatal("valid checkpoint rejected")
	}
	for name, mutate := range map[string]func(*scanCheckpoint){
		"fence":     func(c *scanCheckpoint) { c.Fence = 0 },
		"query":     func(c *scanCheckpoint) { c.Query = "invalid" },
		"window":    func(c *scanCheckpoint) { c.Until = c.From },
		"rows":      func(c *scanCheckpoint) { c.Rows = 31 },
		"duplicate": func(c *scanCheckpoint) { c.Conflicts = c.Matches },
		"time":      func(c *scanCheckpoint) { c.ObservedAt = time.Time{} },
		"candidate_bound": func(c *scanCheckpoint) {
			c.Matches = append(c.Matches, "txn_aaaaaaaaaaaaaaaaaaaaaaaaaa", "txn_bbbbbbbbbbbbbbbbbbbbbbbbbb")
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if changed.valid(changed, checkoutScanBounds) {
				t.Fatal("invalid checkpoint accepted")
			}
		})
	}
	changed := base
	changed.Until = changed.Until.Add(time.Hour)
	if changed.valid(base, checkoutScanBounds) {
		t.Fatal("changed persisted window accepted")
	}
}
