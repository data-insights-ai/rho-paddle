package paddle_test

import (
	"os"
	"strings"
	"testing"

	paddle "github.com/data-insights-ai/rho-paddle"
)

// TestSandboxCatalog proves API acceptance only: the key works and the
// catalog decodes. It reads, it changes nothing.
func TestSandboxCatalog(t *testing.T) {
	if os.Getenv("PADDLE_SANDBOX_TEST") != "1" {
		t.Skip("set PADDLE_SANDBOX_TEST=1 and PADDLE_SANDBOX_KEY_FILE (docs/SANDBOX.md)")
	}
	key, err := os.ReadFile(os.Getenv("PADDLE_SANDBOX_KEY_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := paddle.New(paddle.Config{Environment: paddle.Sandbox, APIKey: strings.TrimSpace(string(key)), WebhookSecrets: []string{"unused"}})
	if err != nil {
		t.Fatal(err)
	}
	products, err := c.Catalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d products", len(products))
}
