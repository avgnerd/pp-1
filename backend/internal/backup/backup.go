// Package backup writes the instance state (admin account, settings and storage list) into a
// portable backup file and restores such a file into this instance.
//
// A file is a UTF-8 JSON object with a plaintext Envelope header and either a plaintext Payload
// ("none") or an AES-256-GCM sealed Payload ("password" or "instance-key"). The header fields
// that describe how the body was produced are bound as additional authenticated data, so a file
// whose header was edited fails authentication instead of being silently reinterpreted.
package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
)

// Format identifies a backup file and the version of its layout.
const (
	FormatName = "dvfs-backup"
	// FormatVersion is the version this build writes.
	FormatVersion = 1
	// MaxFormatVersion is the newest version this build can read.
	MaxFormatVersion = 1
)

// Encryption modes.
const (
	// ModePassword derives the key from a password typed by the admin (Argon2id + AES-256-GCM).
	ModePassword = "password"
	// ModeNone stores the payload as plain JSON.
	ModeNone = "none"
	// ModeInstanceKey derives the key from the instance master key; used for pre-restore
	// snapshots, which are only readable by the instance that wrote them.
	ModeInstanceKey = "instance-key"
)

// KDF identifiers.
const KDFArgon2id = "argon2id"

// Password bounds, counted in Unicode code points.
const (
	MinPasswordRunes = 8
	MaxPasswordRunes = 128
)

// AppVersion is written into every envelope. It is a variable so a build can stamp it.
var AppVersion = "1.0.0"

// Errors that map to a client status. They are wrapped with a human readable message so the API
// can say what happened and what to do about it.
var (
	// ErrFormat means the bytes are not a backup file this build understands.
	ErrFormat = errors.New("백업 파일 형식이 아닙니다")
	// ErrPasswordRequired means an encrypted backup was offered without a password.
	ErrPasswordRequired = errors.New("백업 비밀번호가 필요합니다")
	// ErrDecrypt means the password (or master key) was wrong, or the file was modified. The two
	// cases are deliberately indistinguishable.
	ErrDecrypt = errors.New("비밀번호가 틀렸거나 파일이 손상되었습니다")
)

// VersionError reports a file written by a newer app build.
type VersionError struct{ Got int }

func (e *VersionError) Error() string {
	return fmt.Sprintf("더 새로운 버전(%d)의 앱에서 만든 백업입니다. 앱을 업데이트한 뒤 다시 시도하세요", e.Got)
}

// ValidationError reports input this build cannot accept (HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// CredentialError reports a storage whose sealed credentials cannot be decrypted with this
// instance's master key. It names the storage and never the key material or ciphertext.
type CredentialError struct {
	Storage   string `json:"storage"`
	MountPath string `json:"mount_path"`
}

func (e *CredentialError) Error() string {
	name := e.Storage
	if name == "" {
		name = "(이름 없음)"
	}
	return fmt.Sprintf("저장소 '%s'(%s)의 자격 증명을 이 인스턴스의 마스터 키로 복호화하지 못했습니다. secret.key가 바뀌지 않았는지 확인하세요", name, e.MountPath)
}

// SnapshotError reports a failure to write or read a pre-restore snapshot (HTTP 500).
type SnapshotError struct{ Err error }

func (e *SnapshotError) Error() string {
	return "복원 전 상태를 저장하지 못했습니다: " + e.Err.Error()
}

func (e *SnapshotError) Unwrap() error { return e.Err }

// KDFParams are the Argon2id parameters recorded in a password-mode envelope.
type KDFParams struct {
	Algorithm string `json:"algorithm"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
	KeyLen    uint32 `json:"key_len"`
	Salt      string `json:"salt"` // standard base64 of the 16-byte salt
}

// Envelope is the outer structure of a backup file.
type Envelope struct {
	Format         string     `json:"format"`
	FormatVersion  int        `json:"format_version"`
	CreatedAt      string     `json:"created_at"`
	AppVersion     string     `json:"app_version"`
	Encrypted      bool       `json:"encrypted"`
	EncryptionMode string     `json:"encryption_mode"`
	KDF            *KDFParams `json:"kdf,omitempty"`
	Nonce          string     `json:"nonce,omitempty"`      // standard base64 of the 12-byte GCM nonce
	Ciphertext     string     `json:"ciphertext,omitempty"` // standard base64 of ciphertext||tag
	Payload        *Payload   `json:"payload,omitempty"`    // present only when Encrypted is false
}

// AdminRecord is the single administrator account.
type AdminRecord struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	CreatedAt    string `json:"created_at"`
	IsDefault    bool   `json:"is_default"`
}

// SettingRecord is one row of the settings table. Value holds JSON text, exactly as stored.
type SettingRecord struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updated_at"`
}

// StorageRecord is one Dropbox storage. AppKey, AppSecret and RefreshToken are plaintext in the
// payload: the backup is portable between instances with different master keys.
type StorageRecord struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	MountPath    string `json:"mount_path"`
	Status       string `json:"status"`
	ErrorMsg     string `json:"error_msg"`
	AccountEmail string `json:"account_email"`
	AccountID    string `json:"account_id"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	AppKey       string `json:"app_key"`
	AppSecret    string `json:"app_secret"`
	RefreshToken string `json:"refresh_token"`
	// The rest of the storage configuration travels with the file so a restore keeps the
	// storage usable without re-entering anything.
	RootPath     string `json:"root_path"`
	AccountName  string `json:"account_name"`
	CacheTTLMin  int    `json:"cache_ttl_min"`
	AuthMode     string `json:"auth_mode"`
	RedirectURI  string `json:"redirect_uri"`
	AuthorizedAt string `json:"authorized_at"`
}

// Payload is the backed-up data. Sessions, caches, media metadata and OAuth states are never
// part of it.
type Payload struct {
	Admin    AdminRecord     `json:"admin"`
	Settings []SettingRecord `json:"settings"`
	Storages []StorageRecord `json:"storages"`
}

// ValidatePassword enforces the 8~128 code point rule. The bytes are otherwise untouched:
// leading and trailing spaces are part of the password.
func ValidatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordRunes || n > MaxPasswordRunes {
		return &ValidationError{Msg: fmt.Sprintf("백업 비밀번호는 %d~%d자여야 합니다", MinPasswordRunes, MaxPasswordRunes)}
	}
	return nil
}

// Parse decodes a backup file and checks the plaintext header. It does not decrypt anything.
func Parse(data []byte) (*Envelope, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: 파일이 비어 있습니다", ErrFormat)
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%w: JSON을 읽지 못했습니다", ErrFormat)
	}
	if err := env.checkHeader(); err != nil {
		return nil, err
	}
	return &env, nil
}

// checkHeader validates the header fields every encryption mode needs.
func (e *Envelope) checkHeader() error {
	if e.Format != FormatName {
		return fmt.Errorf("%w: 형식 식별자가 '%s'가 아닙니다", ErrFormat, FormatName)
	}
	if e.FormatVersion < 1 {
		return fmt.Errorf("%w: 형식 버전이 없습니다", ErrFormat)
	}
	if e.FormatVersion > MaxFormatVersion {
		return &VersionError{Got: e.FormatVersion}
	}
	if _, err := time.Parse(time.RFC3339, e.CreatedAt); err != nil {
		return fmt.Errorf("%w: 생성 시각이 RFC 3339 형식이 아닙니다", ErrFormat)
	}
	if strings.TrimSpace(e.AppVersion) == "" {
		return fmt.Errorf("%w: 앱 버전이 없습니다", ErrFormat)
	}
	return nil
}

// OpenEnvelope verifies and decrypts a backup file. password is only used for password mode,
// masterKey only for instance-key mode. Nothing is written anywhere: a tampered file fails
// authentication before any caller can act on the payload.
func OpenEnvelope(env *Envelope, password string, masterKey []byte) (*Payload, error) {
	if env == nil {
		return nil, fmt.Errorf("%w: 내용이 없습니다", ErrFormat)
	}
	if err := env.checkHeader(); err != nil {
		return nil, err
	}
	if env.Encrypted {
		switch env.EncryptionMode {
		case ModePassword:
			return openPassword(env, password)
		case ModeInstanceKey:
			return openInstanceKey(env, masterKey)
		default:
			return nil, fmt.Errorf("%w: 암호화 방식 '%s'을(를) 알 수 없습니다", ErrFormat, env.EncryptionMode)
		}
	}
	if env.EncryptionMode != ModeNone {
		return nil, fmt.Errorf("%w: 암호화 여부와 암호화 방식이 맞지 않습니다", ErrFormat)
	}
	if env.KDF != nil || env.Nonce != "" || env.Ciphertext != "" {
		return nil, fmt.Errorf("%w: 암호화하지 않은 파일에 암호화 정보가 있습니다", ErrFormat)
	}
	if env.Payload == nil {
		return nil, fmt.Errorf("%w: 백업 데이터가 없습니다", ErrFormat)
	}
	return env.Payload, nil
}

func openPassword(env *Envelope, password string) (*Payload, error) {
	if env.KDF == nil {
		return nil, fmt.Errorf("%w: 키 유도 정보가 없습니다", ErrFormat)
	}
	if err := env.KDF.check(); err != nil {
		return nil, err
	}
	salt, err := decodeExact("salt", env.KDF.Salt, saltSize)
	if err != nil {
		return nil, err
	}
	nonce, err := decodeExact("nonce", env.Nonce, nonceSize)
	if err != nil {
		return nil, err
	}
	ciphertext, err := decodeBase64("ciphertext", env.Ciphertext)
	if err != nil {
		return nil, err
	}
	if password == "" {
		return nil, ErrPasswordRequired
	}
	key := derivePasswordKey(password, salt, env.KDF)
	plain, err := openWith(key, env.aad(), nonce, ciphertext)
	if err != nil {
		return nil, err
	}
	return decodePayload(plain)
}

func openInstanceKey(env *Envelope, masterKey []byte) (*Payload, error) {
	if env.KDF != nil {
		return nil, fmt.Errorf("%w: instance-key 파일에 키 유도 정보가 있습니다", ErrFormat)
	}
	nonce, err := decodeExact("nonce", env.Nonce, nonceSize)
	if err != nil {
		return nil, err
	}
	ciphertext, err := decodeBase64("ciphertext", env.Ciphertext)
	if err != nil {
		return nil, err
	}
	key, err := SnapshotKey(masterKey)
	if err != nil {
		return nil, ErrDecrypt
	}
	plain, err := openWith(key, env.aad(), nonce, ciphertext)
	if err != nil {
		return nil, err
	}
	return decodePayload(plain)
}

// check enforces the Argon2id parameter bounds before any key derivation runs.
func (k *KDFParams) check() error {
	if k.Algorithm != KDFArgon2id {
		return &ValidationError{Msg: "지원하지 않는 키 유도 방식입니다: " + k.Algorithm}
	}
	if k.Time < minKDFTime || k.Time > maxKDFTime {
		return &ValidationError{Msg: fmt.Sprintf("키 유도 시간(%d)이 허용 범위(%d~%d)를 벗어났습니다", k.Time, minKDFTime, maxKDFTime)}
	}
	if k.MemoryKiB < minKDFMemory || k.MemoryKiB > maxKDFMemory {
		return &ValidationError{Msg: fmt.Sprintf("키 유도 메모리(%d KiB)가 허용 범위(%d~%d KiB)를 벗어났습니다", k.MemoryKiB, minKDFMemory, maxKDFMemory)}
	}
	if k.Threads < minKDFThreads || k.Threads > maxKDFThreads {
		return &ValidationError{Msg: fmt.Sprintf("키 유도 스레드(%d)가 허용 범위(%d~%d)를 벗어났습니다", k.Threads, minKDFThreads, maxKDFThreads)}
	}
	if k.KeyLen != keySize {
		return &ValidationError{Msg: fmt.Sprintf("키 길이(%d)는 %d바이트여야 합니다", k.KeyLen, keySize)}
	}
	return nil
}

// decodeBase64 decodes a standard base64 field that must not be empty.
func decodeBase64(field, s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("%w: %s 값이 없습니다", ErrFormat, field)
	}
	b, err := stdBase64.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %s 값이 올바른 base64가 아닙니다", ErrFormat, field)
	}
	return b, nil
}

// decodePayload parses the plaintext payload and insists every section exists.
func decodePayload(plain []byte) (*Payload, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(plain, &probe); err != nil {
		return nil, fmt.Errorf("%w: 백업 데이터를 읽지 못했습니다", ErrFormat)
	}
	for _, k := range []string{"admin", "settings", "storages"} {
		if _, ok := probe[k]; !ok {
			return nil, fmt.Errorf("%w: %s 항목이 없습니다", ErrFormat, k)
		}
	}
	var p Payload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, fmt.Errorf("%w: 백업 데이터를 읽지 못했습니다", ErrFormat)
	}
	return &p, nil
}

// ExportOptions selects how Export seals the payload.
type ExportOptions struct {
	// Mode is ModePassword, ModeNone or ModeInstanceKey.
	Mode string
	// Password is used only by ModePassword and is never trimmed.
	Password string
	// ConfirmUnencrypted must be true for ModeNone.
	ConfirmUnencrypted bool
	// Box decrypts the stored storage credentials; it must use this instance's master key.
	Box *secret.Box
	// MasterKey is the 32-byte instance master key; used by ModeInstanceKey.
	MasterKey []byte
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Export reads the admin account, every setting and every storage inside one read transaction and
// returns an envelope ready to be written. Storage credentials are decrypted so the file does not
// depend on this instance's master key.
func Export(ctx context.Context, d *sql.DB, opts ExportOptions) (*Envelope, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	switch opts.Mode {
	case ModePassword:
		if err := ValidatePassword(opts.Password); err != nil {
			return nil, err
		}
	case ModeNone:
		if !opts.ConfirmUnencrypted {
			return nil, &ValidationError{Msg: "암호화하지 않은 백업에는 위험 안내 확인이 필요합니다"}
		}
	case ModeInstanceKey:
		if _, err := SnapshotKey(opts.MasterKey); err != nil {
			return nil, err
		}
	default:
		return nil, &ValidationError{Msg: "암호화 방식이 올바르지 않습니다"}
	}
	if opts.Box == nil {
		return nil, errors.New("백업에 필요한 마스터 키가 없습니다")
	}
	payload, err := readPayload(ctx, d, opts.Box, now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	env := &Envelope{
		Format:         FormatName,
		FormatVersion:  FormatVersion,
		CreatedAt:      now().UTC().Format(time.RFC3339),
		AppVersion:     AppVersion,
		EncryptionMode: opts.Mode,
	}
	if opts.Mode == ModeNone {
		env.Encrypted = false
		env.Payload = payload
		return env, nil
	}
	env.Encrypted = true
	plain, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("백업 데이터를 직렬화하지 못했습니다: %w", err)
	}
	var key []byte
	switch opts.Mode {
	case ModePassword:
		salt, err := randomBytes(saltSize)
		if err != nil {
			return nil, err
		}
		env.KDF = &KDFParams{
			Algorithm: KDFArgon2id, Time: argonTime, MemoryKiB: argonMemory,
			Threads: argonThreads, KeyLen: argonKeyLen, Salt: stdBase64.EncodeToString(salt),
		}
		key = derivePasswordKey(opts.Password, salt, env.KDF)
	case ModeInstanceKey:
		key, err = SnapshotKey(opts.MasterKey)
		if err != nil {
			return nil, err
		}
	}
	nonce, ciphertext, err := sealWith(key, env.aad(), plain)
	if err != nil {
		return nil, fmt.Errorf("백업을 암호화하지 못했습니다: %w", err)
	}
	env.Nonce = stdBase64.EncodeToString(nonce)
	env.Ciphertext = stdBase64.EncodeToString(ciphertext)
	return env, nil
}

// readPayload snapshots the backed-up tables. One transaction keeps a concurrent change from
// landing half inside the backup.
func readPayload(ctx context.Context, d *sql.DB, box *secret.Box, exportedAt string) (*Payload, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("백업 읽기를 시작하지 못했습니다: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	p := &Payload{}
	var isDefault int
	var updatedAt int64
	if err := tx.QueryRowContext(ctx,
		`SELECT username, password_hash, is_default, updated_at FROM admin WHERE id = 1`).
		Scan(&p.Admin.Username, &p.Admin.PasswordHash, &isDefault, &updatedAt); err != nil {
		return nil, fmt.Errorf("관리자 계정을 읽지 못했습니다: %w", err)
	}
	p.Admin.IsDefault = isDefault == 1
	p.Admin.CreatedAt = unixToRFC3339(updatedAt)

	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("설정을 읽지 못했습니다: %w", err)
	}
	for rows.Next() {
		var rec SettingRecord
		if err := rows.Scan(&rec.Key, &rec.Value); err != nil {
			rows.Close()
			return nil, fmt.Errorf("설정을 읽지 못했습니다: %w", err)
		}
		rec.UpdatedAt = exportedAt
		p.Settings = append(p.Settings, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("설정을 읽지 못했습니다: %w", err)
	}
	rows.Close()

	srows, err := tx.QueryContext(ctx, `SELECT `+storageExportCols+` FROM storages ORDER BY mount_path, id`)
	if err != nil {
		return nil, fmt.Errorf("저장소를 읽지 못했습니다: %w", err)
	}
	for srows.Next() {
		var rec StorageRecord
		var appSecretEnc, refreshEnc []byte
		var createdAt, updatedAt, authorizedAt int64
		if err := srows.Scan(&rec.ID, &rec.Name, &rec.MountPath, &rec.RootPath, &rec.AppKey,
			&appSecretEnc, &refreshEnc, &rec.AccountID, &rec.AccountEmail, &rec.AccountName,
			&rec.CacheTTLMin, &rec.Status, &rec.AuthMode, &rec.RedirectURI, &rec.ErrorMsg,
			&createdAt, &updatedAt, &authorizedAt); err != nil {
			srows.Close()
			return nil, fmt.Errorf("저장소를 읽지 못했습니다: %w", err)
		}
		rec.CreatedAt = unixToRFC3339(createdAt)
		rec.UpdatedAt = unixToRFC3339(updatedAt)
		rec.AuthorizedAt = millisToRFC3339(authorizedAt)
		if rec.AppSecret, err = openCredential(box, appSecretEnc, rec); err != nil {
			srows.Close()
			return nil, err
		}
		if rec.RefreshToken, err = openCredential(box, refreshEnc, rec); err != nil {
			srows.Close()
			return nil, err
		}
		p.Storages = append(p.Storages, rec)
	}
	if err := srows.Err(); err != nil {
		srows.Close()
		return nil, fmt.Errorf("저장소를 읽지 못했습니다: %w", err)
	}
	srows.Close()
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("백업 읽기를 마치지 못했습니다: %w", err)
	}

	// Deterministic order makes two exports of the same state byte-identical apart from the
	// fresh salt and nonce.
	sort.SliceStable(p.Settings, func(i, j int) bool { return p.Settings[i].Key < p.Settings[j].Key })
	sort.SliceStable(p.Storages, func(i, j int) bool {
		if p.Storages[i].MountPath != p.Storages[j].MountPath {
			return p.Storages[i].MountPath < p.Storages[j].MountPath
		}
		return p.Storages[i].ID < p.Storages[j].ID
	})
	return p, nil
}

// storageExportCols is the column list readPayload scans, in StorageRecord field order.
const storageExportCols = `id, name, mount_path, root_path, app_key, app_secret_enc, refresh_token_enc,
	account_id, account_email, account_name, cache_ttl_min, status, auth_mode, redirect_uri,
	last_error, created_at, updated_at, authorized_at`

// openCredential decrypts one sealed value, naming the storage when it fails.
func openCredential(box *secret.Box, sealed []byte, rec StorageRecord) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	plain, err := box.Open(sealed)
	if err != nil {
		return "", &CredentialError{Storage: rec.Name, MountPath: rec.MountPath}
	}
	return string(plain), nil
}

// unixToRFC3339 renders a unix second timestamp; zero becomes the empty string.
func unixToRFC3339(sec int64) string {
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// parseRFC3339 reads an RFC 3339 timestamp into unix seconds.
func parseRFC3339(s string) (int64, bool) {
	if strings.TrimSpace(s) == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// millisToRFC3339 renders a unix millisecond timestamp, keeping the milliseconds in a fraction;
// zero becomes the empty string. authorized_at is stored in milliseconds, and rendering it as
// seconds wrote a year far past RFC 3339's four-digit range (58820), which the restore could not
// parse, so every round trip reset the value to 0.
func millisToRFC3339(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// parseRFC3339Millis reads an RFC 3339 timestamp into unix milliseconds. RFC3339 parsing accepts
// the optional fractional second millisToRFC3339 writes, so the value survives the round trip.
func parseRFC3339Millis(s string) (int64, bool) {
	if strings.TrimSpace(s) == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}
