package tools

import (
	"fmt"
	"strings"

	feishusecure "lingobridge/internal/platform/feishu/secure"
	"lingobridge/internal/store"
)

const (
	docxAppendEnvelopeKeyInfo = "lingobridge/feishu/docx-append-envelope/v1"
	docxAppendEnvelopeContext = "docx_append"
)

// DocxAppendEnvelopeCipher protects the exact append request body while a
// durable operation may still need restart recovery.
type DocxAppendEnvelopeCipher struct {
	accountID string
	cipher    *feishusecure.AccountCipher
}

func NewDocxAppendEnvelopeCipher(secret, accountID string) (*DocxAppendEnvelopeCipher, error) {
	accountID = strings.TrimSpace(accountID)
	accountCipher, err := feishusecure.NewAccountCipher(secret, accountID, docxAppendEnvelopeKeyInfo)
	if err != nil {
		return nil, fmt.Errorf("create feishu docx append envelope cipher: %w", err)
	}
	return &DocxAppendEnvelopeCipher{accountID: accountID, cipher: accountCipher}, nil
}

func (c *DocxAppendEnvelopeCipher) encrypt(operation store.FeishuDocxAppendOperation, plaintext []byte) (string, error) {
	if c == nil || c.cipher == nil {
		return "", fmt.Errorf("feishu docx append envelope cipher is unavailable")
	}
	additionalData, err := c.additionalData(operation)
	if err != nil {
		return "", err
	}
	ciphertext, err := c.cipher.Encrypt(plaintext, additionalData)
	if err != nil {
		return "", fmt.Errorf("encrypt feishu docx append envelope: %w", err)
	}
	return ciphertext, nil
}

func (c *DocxAppendEnvelopeCipher) decrypt(operation store.FeishuDocxAppendOperation) ([]byte, error) {
	if c == nil || c.cipher == nil {
		return nil, fmt.Errorf("feishu docx append envelope cipher is unavailable")
	}
	additionalData, err := c.additionalData(operation)
	if err != nil {
		return nil, err
	}
	plaintext, err := c.cipher.Decrypt(operation.EnvelopeCiphertext, additionalData)
	if err != nil {
		return nil, fmt.Errorf("decrypt feishu docx append envelope: %w", err)
	}
	return plaintext, nil
}

func (c *DocxAppendEnvelopeCipher) additionalData(operation store.FeishuDocxAppendOperation) ([]byte, error) {
	operation.RequestID = strings.TrimSpace(operation.RequestID)
	operation.AccountID = strings.TrimSpace(operation.AccountID)
	operation.ChatID = strings.TrimSpace(operation.ChatID)
	operation.ActorOpenID = strings.TrimSpace(operation.ActorOpenID)
	operation.ActorUserID = strings.TrimSpace(operation.ActorUserID)
	operation.DocumentToken = strings.TrimSpace(operation.DocumentToken)
	operation.ClientToken = strings.TrimSpace(operation.ClientToken)
	operation.PayloadHash = strings.TrimSpace(operation.PayloadHash)
	operation.EnvelopeHash = strings.TrimSpace(operation.EnvelopeHash)
	if operation.RequestID == "" || operation.AccountID == "" || operation.AccountID != c.accountID ||
		operation.ChatID == "" || (operation.ActorOpenID == "" && operation.ActorUserID == "") ||
		operation.DocumentToken == "" || operation.ClientToken == "" || operation.PayloadHash == "" || operation.EnvelopeHash == "" {
		return nil, fmt.Errorf("valid feishu docx append envelope encryption context is required")
	}
	return []byte(strings.Join([]string{
		"v1",
		docxAppendEnvelopeContext,
		operation.AccountID,
		operation.RequestID,
		operation.ChatID,
		operation.ActorOpenID,
		operation.ActorUserID,
		operation.DocumentToken,
		operation.ClientToken,
		operation.PayloadHash,
		operation.EnvelopeHash,
	}, "\x00")), nil
}
