// Package signature verifies Paddle webhook signatures.
package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	maxHeaderLength = 8192
	maxSignatures   = 8
	maxSecrets      = 4
	maxSecretLength = 4096
)

var (
	ErrInvalid   = errors.New("invalid signature input")
	ErrSignature = errors.New("signature mismatch")
	ErrTimestamp = errors.New("signature timestamp invalid")
)

// Verify authenticates raw as a Paddle webhook body using the Paddle-Signature
// header, one or more destination secrets, and a symmetric timestamp window.
func Verify(raw []byte, header string, secrets []string, now time.Time, tolerance time.Duration) error {
	if len(header) == 0 || len(header) > maxHeaderLength || now.IsZero() || tolerance <= 0 {
		return ErrInvalid
	}
	if len(secrets) == 0 || len(secrets) > maxSecrets {
		return ErrInvalid
	}
	for _, secret := range secrets {
		if len(secret) == 0 || len(secret) > maxSecretLength {
			return ErrInvalid
		}
	}

	timestamp, timestampText, signatures, err := parseHeader(header)
	if err != nil {
		return err
	}
	signedAt := time.Unix(timestamp, 0)
	if signedAt.Before(now.Add(-tolerance)) || signedAt.After(now.Add(tolerance)) {
		return ErrTimestamp
	}

	// The HMAC is streamed rather than built into one buffer: this runs on the
	// synchronous webhook path for bodies up to a megabyte, and the digest is
	// identical either way.
	matched := 0
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(timestampText))
		_, _ = mac.Write([]byte{':'})
		_, _ = mac.Write(raw)
		expected := mac.Sum(nil)
		for _, signature := range signatures {
			matched |= subtle.ConstantTimeCompare(expected, signature[:])
		}
	}
	if matched != 1 {
		return ErrSignature
	}
	return nil
}

func parseHeader(header string) (int64, string, [][sha256.Size]byte, error) {
	var timestamp int64
	var timestampText string
	var haveTimestamp bool
	signatures := make([][sha256.Size]byte, 0, 1)

	for field := range strings.SplitSeq(header, ";") {
		field = strings.TrimSpace(field)
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			return 0, "", nil, ErrInvalid
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		switch name {
		case "ts":
			if haveTimestamp {
				return 0, "", nil, ErrTimestamp
			}
			haveTimestamp = true
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed <= 0 {
				return 0, "", nil, ErrTimestamp
			}
			timestamp = parsed
			timestampText = value
		case "h1":
			if len(signatures) == maxSignatures || len(value) != sha256.Size*2 {
				return 0, "", nil, ErrInvalid
			}
			var signature [sha256.Size]byte
			decoded, err := hex.Decode(signature[:], []byte(value))
			if err != nil || decoded != sha256.Size {
				return 0, "", nil, ErrInvalid
			}
			signatures = append(signatures, signature)
		default:
			return 0, "", nil, ErrInvalid
		}
	}

	if !haveTimestamp {
		return 0, "", nil, ErrTimestamp
	}
	if len(signatures) == 0 {
		return 0, "", nil, ErrInvalid
	}
	return timestamp, timestampText, signatures, nil
}
