package model

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestTokenKeyEntropyAndRejection(t *testing.T) {
	// Bytes 248..255 must be discarded, not reduced modulo 62.
	entropy := append([]byte{248, 249, 250, 251, 252, 253, 254, 255}, make([]byte, 48)...)
	for i := range 48 {
		entropy[8+i] = byte(i)
	}

	key, err := generateKey(bytes.NewReader(entropy))
	if err != nil || key != keyChars[:48] {
		t.Fatalf("unbiased sample: key=%q err=%v", key, err)
	}

	generated, err := generateKey(rand.Reader)
	if err != nil || len(generated) != 48 || strings.ContainsAny(generated, "+/=") {
		t.Fatalf("key format: len=%d err=%v", len(generated), err)
	}

	for _, c := range generated {
		if !strings.ContainsRune(keyChars, c) {
			t.Fatal("unexpected key character")
		}
	}
}

func TestTokenCreationFailsClosedWithoutEntropy(t *testing.T) {
	token := Token{PeriodQuota: 1}
	if err := token.beforeCreate(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Fatalf("entropy error lost: %v", err)
	}

	if token.Key != "" || !token.PeriodLastUpdateTime.IsZero() {
		t.Fatal("failed entropy modified token")
	}
	// Provisioned keys never depend on new entropy and retain their exact bytes.
	existing := strings.Repeat("a", 48)

	token.Key = existing
	if err := token.beforeCreate(strings.NewReader("")); err != nil {
		t.Fatal(err)
	}

	if token.Key != existing {
		t.Fatal("existing key was rewritten")
	}
}
