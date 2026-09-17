// Package paddlewire holds the primitives shared by every layer of the adapter:
// provider identity shapes, exact money parsing, and the digests that carry
// correlation and observation identity. It is internal so that a Paddle wire
// concern can never reach an exported signature.
package paddlewire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	billing "github.com/data-insights-ai/rho-billing"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// ErrResponse marks a provider response this adapter will not interpret.
var ErrResponse = errors.New("paddle: invalid provider response")

// ID reports whether id is a Paddle identifier with the given prefix. Paddle
// ids are the prefix followed by 26 lowercase alphanumerics.
func ID(id, prefix string) bool {
	if len(id) != len(prefix)+26 || !strings.HasPrefix(id, prefix) {
		return false
	}
	for _, ch := range id[len(prefix):] {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

// Fingerprint reports whether value is a 64-character hex digest.
func Fingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') && (ch < 'A' || ch > 'F') {
			return false
		}
	}
	return true
}

// HTTPSURL reports whether raw is a bounded https URL carrying no credentials
// or fragment.
func HTTPSURL(raw string) bool {
	if len(raw) > 16384 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

// MinorUnits parses a Paddle money string. Paddle sends amounts as decimal
// digits in the currency's minor unit; anything else is not money.
func MinorUnits(value string) (int64, error) {
	if value == "" || len(value) > 19 {
		return 0, ErrResponse
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, ErrResponse
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, ErrResponse
	}
	return n, nil
}

// AddMoney adds two non-negative minor-unit amounts, refusing to overflow. It
// reports the core's canonical overflow error: this predicate previously
// existed in five places with four different outcomes, so a caller matching on
// billing.ErrOverflow saw it only some of the time.
func AddMoney(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, billing.ErrOverflow
	}
	return a + b, nil
}

// Digest is the correlation and observation identity used across dispatch and
// recovery. Components are JSON-encoded and NUL-joined; a literal NUL inside a
// string encodes as an escape, so a component cannot forge the separator.
func Digest(values ...any) string {
	sum := sha256.Sum256([]byte(strings.Join(stringValues(values), "\x00")))
	return hex.EncodeToString(sum[:])
}

func stringValues(values []any) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = strings.TrimSpace(encode(value))
	}
	return out
}

// encode never collapses a failure to the empty string: these components are
// correlation and observation identities, so two different inputs must not
// digest the same. The sentinel cannot collide with JSON output.
func encode(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("\x01unencodable:%T:%v", value, err)
	}
	return string(b)
}
