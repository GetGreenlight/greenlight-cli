//go:build darwin || linux

package main

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	priv, err := generateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	cases := [][]byte{
		[]byte("hello"),
		[]byte("a very-long secret value with !@#$%^&*() and unicode 🦊"),
		bytes.Repeat([]byte("x"), 4096),
	}
	for _, plain := range cases {
		blob, err := encryptSecret(priv.PublicKey(), plain)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		got, err := decryptSecret(priv, blob)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("round-trip mismatch: got %q want %q", got, plain)
		}
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	priv1, _ := generateKeypair()
	priv2, _ := generateKeypair()
	blob, err := encryptSecret(priv1.PublicKey(), []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptSecret(priv2, blob); err == nil {
		t.Fatal("expected decrypt failure with wrong key")
	}
}

func TestDecryptShortBlobFails(t *testing.T) {
	priv, _ := generateKeypair()
	if _, err := decryptSecret(priv, []byte("short")); err == nil {
		t.Fatal("expected decrypt failure on short blob")
	}
}
