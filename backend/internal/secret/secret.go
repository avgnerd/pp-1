// Package secret encrypts small values (app secrets, refresh tokens) with AES-256-GCM
// and signs URLs with HMAC-SHA256. Distinct sub-keys are derived for each purpose.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// Box holds derived keys.
type Box struct {
	aead   cipher.AEAD
	macKey []byte
}

func derive(master []byte, label string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// New derives encryption and MAC keys from a 32-byte master key.
func New(master []byte) (*Box, error) {
	if len(master) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	block, err := aes.NewCipher(derive(master, "dvfs-encrypt-v1"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, macKey: derive(master, "dvfs-mac-v1")}, nil
}

// Seal encrypts plaintext; output is nonce||ciphertext.
func (b *Box) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, nil), nil
}

// Open decrypts a value produced by Seal.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("ciphertext too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], nil)
}

// Sign returns a URL-safe HMAC of msg.
func (b *Box) Sign(msg string) string {
	m := hmac.New(sha256.New, b.macKey)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a signature produced by Sign in constant time.
func (b *Box) Verify(msg, sig string) bool {
	want := b.Sign(msg)
	return hmac.Equal([]byte(want), []byte(sig))
}

// RandomToken returns n random bytes encoded URL-safe.
func RandomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken returns a hex-free base64 SHA-256 of a token for storage lookups.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
