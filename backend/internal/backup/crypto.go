package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for password mode: 64 MiB of memory, 3 passes, 4 lanes, 32-byte key.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
)

// Sizes fixed by the file format.
const (
	saltSize  = 16
	nonceSize = 12
	keySize   = 32
)

// Bounds accepted when opening a file, so a hostile envelope cannot ask this process to burn
// gigabytes of memory or hours of CPU before the password is even checked.
const (
	minKDFTime    uint32 = 1
	maxKDFTime    uint32 = 10
	minKDFMemory  uint32 = 16 * 1024
	maxKDFMemory  uint32 = 256 * 1024
	minKDFThreads uint8  = 1
	maxKDFThreads uint8  = 16
)

// SnapshotLabel is the HMAC domain label of the sub-key that encrypts pre-restore snapshots. It
// stays distinct from the labels secret.go derives ("dvfs-encrypt-v1", "dvfs-mac-v1") so a
// snapshot blob can never be mistaken for (or replayed as) an encrypted storage credential.
const SnapshotLabel = "dvfs-backup-snapshot-v1"

// aadLabel prefixes the canonical header bytes bound as additional authenticated data.
const aadLabel = "dvfs-backup-aad-v1\x00"

// aadFields is the canonical form of the plaintext envelope header. Only these fields are
// authenticated: they describe how the body was produced, so changing any of them must make
// decryption fail instead of silently reinterpreting the ciphertext.
type aadFields struct {
	FormatVersion  int        `json:"format_version"`
	CreatedAt      string     `json:"created_at"`
	AppVersion     string     `json:"app_version"`
	Encrypted      bool       `json:"encrypted"`
	EncryptionMode string     `json:"encryption_mode"`
	KDF            *KDFParams `json:"kdf,omitempty"`
}

// aad renders the envelope header as deterministic bytes. json.Marshal writes struct fields in
// declaration order, so the same header always yields the same AAD.
func (e *Envelope) aad() []byte {
	b, err := json.Marshal(aadFields{
		FormatVersion:  e.FormatVersion,
		CreatedAt:      e.CreatedAt,
		AppVersion:     e.AppVersion,
		Encrypted:      e.Encrypted,
		EncryptionMode: e.EncryptionMode,
		KDF:            e.KDF,
	})
	if err != nil {
		// The struct holds only strings, numbers and a pointer to the same; marshal cannot fail.
		return []byte(aadLabel)
	}
	out := make([]byte, 0, len(aadLabel)+len(b))
	out = append(out, aadLabel...)
	return append(out, b...)
}

// SnapshotKey derives the 32-byte AES-256 key used for pre-restore snapshots from the instance
// master key.
func SnapshotKey(master []byte) ([]byte, error) {
	if len(master) != keySize {
		return nil, errors.New("instance master key must be 32 bytes")
	}
	m := hmac.New(sha256.New, master)
	m.Write([]byte(SnapshotLabel))
	return m.Sum(nil), nil
}

// randomBytes returns n bytes from crypto/rand.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("난수 생성에 실패했습니다: %w", err)
	}
	return b, nil
}

// derivePasswordKey runs Argon2id with the parameters recorded in p.
func derivePasswordKey(password string, salt []byte, p *KDFParams) []byte {
	return argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, p.KeyLen)
}

// sealWith encrypts plain with AES-256-GCM and returns the nonce and ciphertext||tag.
func sealWith(key, aad, plain []byte) (nonce, ciphertext []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("암호화 키를 만들지 못했습니다: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("암호화 모드를 초기화하지 못했습니다: %w", err)
	}
	nonce, err = randomBytes(nonceSize)
	if err != nil {
		return nil, nil, err
	}
	return nonce, gcm.Seal(nil, nonce, plain, aad), nil
}

// openWith verifies the GCM tag and returns the plaintext.
func openWith(key, aad, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrDecrypt
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrDecrypt
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, ErrDecrypt
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}

// stdBase64 is the RFC 4648 standard alphabet with padding, as the format requires.
var stdBase64 = base64.StdEncoding

// decodeExact decodes standard base64 and insists on exactly n bytes. Any deviation is a format
// error: the caller must reject the file before running a key derivation.
func decodeExact(field, s string, n int) ([]byte, error) {
	b, err := stdBase64.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %s 값이 올바른 base64가 아닙니다", ErrFormat, field)
	}
	if len(b) != n {
		return nil, fmt.Errorf("%w: %s 값은 %d바이트여야 합니다", ErrFormat, field, n)
	}
	return b, nil
}
