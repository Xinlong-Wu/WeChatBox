package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// StableUUID returns a deterministic RFC 4122-shaped identifier for one
// Feishu idempotency namespace and its stable inputs. Distinct namespaces keep
// unrelated API operations from accidentally sharing a remote idempotency key.
func StableUUID(namespace string, parts ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(strings.TrimSpace(namespace)))
	for _, part := range parts {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(strings.TrimSpace(part)))
	}
	sum := hash.Sum(nil)
	value := append([]byte(nil), sum[:16]...)
	value[6] = (value[6] & 0x0f) | 0x50
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value)
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32]
}
