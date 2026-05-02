//go:build darwin || linux

package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// keyPath returns the path to the X25519 private key file.
func keyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".greenlight", "key"), nil
}

// generateKeypair creates a new X25519 keypair.
func generateKeypair() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

// loadPrivateKey reads a base64-encoded X25519 private key from ~/.greenlight/key.
func loadPrivateKey() (*ecdh.PrivateKey, error) {
	path, err := keyPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	return ecdh.X25519().NewPrivateKey(raw)
}

// savePrivateKey writes a base64-encoded X25519 private key with 0600 perms.
// Creates ~/.greenlight with 0700 if it does not exist. Refuses to overwrite
// unless overwrite is true.
func savePrivateKey(key *ecdh.PrivateKey, overwrite bool) error {
	path, err := keyPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create key dir: %w", err)
	}
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("key already exists at %s", path)
		}
	}
	encoded := base64.StdEncoding.EncodeToString(key.Bytes())
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(encoded), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removePrivateKey deletes the private key file. Idempotent.
func removePrivateKey() error {
	path, err := keyPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// encryptSecret seals plaintext to recipientPub using X25519 + HKDF-SHA256 + ChaChaPoly.
// Format: ephemeral_public(32) || nonce(12) || ciphertext+tag(16). Matches the iOS app.
func encryptSecret(recipientPub *ecdh.PublicKey, plaintext []byte) ([]byte, error) {
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	shared, err := ephemeral.ECDH(recipientPub)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}

	symKey := make([]byte, chacha20poly1305.KeySize)
	if _, err := hkdf.New(sha256.New, shared, nil, nil).Read(symKey); err != nil {
		return nil, fmt.Errorf("HKDF: %w", err)
	}

	aead, err := chacha20poly1305.New(symKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := aead.Seal(nil, nonce, plaintext, nil)

	out := make([]byte, 0, 32+len(nonce)+len(sealed))
	out = append(out, ephemeral.PublicKey().Bytes()...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// decryptSecret opens a blob produced by encryptSecret using priv.
func decryptSecret(priv *ecdh.PrivateKey, blob []byte) ([]byte, error) {
	if len(blob) < 32+12+16 {
		return nil, fmt.Errorf("ciphertext too short")
	}
	ephPub, err := ecdh.X25519().NewPublicKey(blob[:32])
	if err != nil {
		return nil, fmt.Errorf("parse ephemeral pubkey: %w", err)
	}
	nonce := blob[32:44]
	ct := blob[44:]

	shared, err := priv.ECDH(ephPub)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}

	symKey := make([]byte, chacha20poly1305.KeySize)
	if _, err := hkdf.New(sha256.New, shared, nil, nil).Read(symKey); err != nil {
		return nil, fmt.Errorf("HKDF: %w", err)
	}
	aead, err := chacha20poly1305.New(symKey)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ct, nil)
}

// keyFingerprint returns a short identifier for a public key (8 hex chars of SHA-256).
func keyFingerprint(pub []byte) string {
	h := sha256.Sum256(pub)
	return fmt.Sprintf("%x", h[:4])
}
