package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const ciphertextVersion = "v1"

// AccountCipher protects account-scoped sensitive data with an independent
// HKDF context per caller. Callers own the authenticated-data schema.
type AccountCipher struct {
	aead cipher.AEAD
}

func NewAccountCipher(secret, accountID, keyInfo string) (*AccountCipher, error) {
	secret = strings.TrimSpace(secret)
	accountID = strings.TrimSpace(accountID)
	keyInfo = strings.TrimSpace(keyInfo)
	if secret == "" || accountID == "" || keyInfo == "" {
		return nil, fmt.Errorf("account cipher secret, account, and key context are required")
	}
	key, err := hkdf.Key(sha256.New, []byte(secret), []byte(accountID), keyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("derive account cipher key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create account cipher block: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create account cipher AEAD: %w", err)
	}
	return &AccountCipher{aead: aead}, nil
}

func (c *AccountCipher) Encrypt(plaintext, additionalData []byte) (string, error) {
	if c == nil || c.aead == nil {
		return "", fmt.Errorf("account cipher is unavailable")
	}
	if len(plaintext) == 0 {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate account cipher nonce: %w", err)
	}
	sealed := c.aead.Seal(nil, nonce, plaintext, additionalData)
	encoded := append(nonce, sealed...)
	return ciphertextVersion + "." + base64.RawURLEncoding.EncodeToString(encoded), nil
}

func (c *AccountCipher) Decrypt(ciphertext string, additionalData []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, fmt.Errorf("account cipher is unavailable")
	}
	version, encoded, ok := strings.Cut(strings.TrimSpace(ciphertext), ".")
	if !ok || version != ciphertextVersion || encoded == "" {
		return nil, fmt.Errorf("unsupported account ciphertext")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode account ciphertext: %w", err)
	}
	if len(raw) <= c.aead.NonceSize() {
		return nil, fmt.Errorf("invalid account ciphertext")
	}
	nonce := raw[:c.aead.NonceSize()]
	sealed := raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, sealed, additionalData)
	if err != nil {
		return nil, fmt.Errorf("decrypt account ciphertext: %w", err)
	}
	return plain, nil
}
