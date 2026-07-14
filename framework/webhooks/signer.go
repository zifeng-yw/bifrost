// Package webhooks delivers signed event notifications to registered webhook
// endpoints. It renders payloads, signs them in the Standard Webhooks format,
// and runs the delivery worker that drains the webhook job queue with
// at-least-once semantics.
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// secretPrefix is the Standard Webhooks signing-secret prefix; the remainder
// of the secret is the base64-encoded HMAC key.
const secretPrefix = "whsec_"

// Sign computes the Standard Webhooks signature for one delivery attempt:
// HMAC-SHA256 over "{webhookID}.{unix timestamp}.{body}" keyed with the
// base64-decoded endpoint secret, returned in the "v1,<base64>" wire form
// carried by the webhook-signature header.
func Sign(secret, webhookID string, timestamp time.Time, body []byte) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(signedContent(webhookID, timestamp, body))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// signedContent builds the exact byte sequence covered by the signature.
func signedContent(webhookID string, timestamp time.Time, body []byte) []byte {
	prefix := webhookID + "." + strconv.FormatInt(timestamp.Unix(), 10) + "."
	content := make([]byte, 0, len(prefix)+len(body))
	content = append(content, prefix...)
	return append(content, body...)
}

// decodeSecret strips the whsec_ prefix and base64-decodes the remainder into
// the raw HMAC key.
func decodeSecret(secret string) ([]byte, error) {
	if secret == "" {
		return nil, fmt.Errorf("webhook signing secret is empty")
	}
	encoded := strings.TrimPrefix(secret, secretPrefix)
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid webhook signing secret: %w", err)
	}
	return key, nil
}
