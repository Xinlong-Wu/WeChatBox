package tools

import (
	"bytes"
	"testing"

	"lingobridge/internal/store"
)

func TestDocxAppendEnvelopeCipherRoundTripAndScopeBinding(t *testing.T) {
	cipher := newTestDocxAppendCipher(t)
	operation := store.FeishuDocxAppendOperation{
		RequestID:     "req_cipher",
		AccountID:     "feishu:cli_test",
		ChatID:        "oc_chat",
		ActorOpenID:   "ou_requester",
		ActorUserID:   "u_requester",
		DocumentToken: "doxcn_cipher",
		ClientToken:   "stable-client-token",
		PayloadHash:   "payload-hash",
		EnvelopeHash:  "envelope-hash",
	}
	plaintext := []byte(`{"children":[{"block_type":2,"text":{"elements":[{"text_run":{"content":"private body"}}]}}],"index":1}`)
	ciphertext, err := cipher.encrypt(operation, plaintext)
	if err != nil {
		t.Fatalf("encrypt returned error: %v", err)
	}
	if ciphertext == "" || bytes.Contains([]byte(ciphertext), []byte("private body")) {
		t.Fatalf("ciphertext = %q, want non-plaintext protected value", ciphertext)
	}
	operation.EnvelopeCiphertext = ciphertext
	decrypted, err := cipher.decrypt(operation)
	if err != nil || !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypt = %q err=%v, want original plaintext", decrypted, err)
	}
	tampered := operation
	tampered.DocumentToken = "doxcn_other"
	if _, err := cipher.decrypt(tampered); err == nil {
		t.Fatal("decrypt with a different durable document scope succeeded")
	}
}
