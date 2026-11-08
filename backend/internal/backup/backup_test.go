package backup

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dvfs/dropbox-vfs-manager/internal/auth"
	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

// ---- fixtures ----

// fixture is one instance: its database, master key and secret box.
type fixture struct {
	t   *testing.T
	db  *sql.DB
	key []byte
	box *secret.Box
}

func rawKey(seed byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

// newFixture creates an instance with a seeded admin, two settings rows and one storage.
func newFixture(t *testing.T) *fixture {
	return newFixtureWithKey(t, rawKey(7))
}

// newFixtureWithKey is newFixture with an explicit master key, for tests that move data between
// two instances.
func newFixtureWithKey(t *testing.T, key []byte) *fixture {
	t.Helper()
	d, err := db.OpenPath(filepath.Join(testutil.TempDir(t), "app.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	box, err := secret.New(key)
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	f := &fixture{t: t, db: d, key: key, box: box}
	f.seed()
	return f
}

func (f *fixture) seed() {
	f.t.Helper()
	hash, err := auth.HashPassword("restored-password")
	if err != nil {
		f.t.Fatalf("hash: %v", err)
	}
	f.mustExec(`INSERT INTO admin(id, username, password_hash, is_default, updated_at) VALUES(1, ?, ?, 0, 1700000000)`,
		"admin", hash)
	f.mustExec(`INSERT INTO settings(key, value) VALUES('per_page', '42')`)
	f.mustExec(`INSERT INTO settings(key, value) VALUES('robots_txt', '"User-agent: *"')`)
	f.seedStorage(1, "Main", "/main", "active", "app-key-1", "app-secret-1", "refresh-1")
	f.seedStorage(2, "Archive", "/archive", "error", "app-key-2", "app-secret-2", "refresh-2")
}

func (f *fixture) seedStorage(id int64, name, mount, status, appKey, appSecret, refresh string) {
	f.t.Helper()
	secEnc, err := f.box.Seal([]byte(appSecret))
	if err != nil {
		f.t.Fatalf("seal secret: %v", err)
	}
	rtEnc, err := f.box.Seal([]byte(refresh))
	if err != nil {
		f.t.Fatalf("seal token: %v", err)
	}
	f.mustExec(`INSERT INTO storages(id, name, mount_path, root_path, app_key, app_secret_enc, account_id, account_email,
		account_name, cache_ttl_min, status, auth_mode, redirect_uri, last_error, created_at, updated_at, authorized_at)
		VALUES(?,?,?,?,?,x'00',?,?,?,?,?,?,?,?,?,?,?)`,
		id, name, mount, "/acct", appKey, "acct-id", "mail@example.com", "Name",
		30, status, "localhost", "", "", 1700000000, 1700000000, 1700000001)
	_, err = f.db.Exec(`UPDATE storages SET app_secret_enc = ?, refresh_token_enc = ? WHERE id = ?`, secEnc, rtEnc, id)
	if err != nil {
		f.t.Fatalf("set sealed values: %v", err)
	}
}

func (f *fixture) mustExec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("exec %q: %v", q, err)
	}
}

func (f *fixture) export(mode, password string, confirm bool) *Envelope {
	f.t.Helper()
	env, _, err := NewService(f.db, f.t.TempDir(), f.box, f.key).Export(context.Background(), ExportOptions{
		Mode: mode, Password: password, ConfirmUnencrypted: confirm,
	})
	if err != nil {
		f.t.Fatalf("export (%s): %v", mode, err)
	}
	return env
}

// openFile marshals env, parses the bytes back and decrypts them, exactly as the HTTP path does.
func openFile(t *testing.T, env *Envelope, password string, masterKey []byte) (*Envelope, *Payload) {
	t.Helper()
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	payload, err := OpenEnvelope(parsed, password, masterKey)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return parsed, payload
}

// ---- 1. round trip ----

func TestExportRoundTripAllModes(t *testing.T) {
	f := newFixture(t)
	const pw = "correct horse battery"
	fixedNow := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	s := NewService(f.db, t.TempDir(), f.box, f.key)
	s.Now = func() time.Time { return fixedNow }
	var payloads []*Payload
	for _, tc := range []struct {
		mode    string
		pass    string
		confirm bool
	}{
		{ModePassword, pw, false},
		{ModeNone, "", true},
		{ModeInstanceKey, "", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			env, _, err := s.Export(context.Background(), ExportOptions{
				Mode: tc.mode, Password: tc.pass, ConfirmUnencrypted: tc.confirm,
			})
			if err != nil {
				t.Fatalf("export (%s): %v", tc.mode, err)
			}
			if env.Format != FormatName || env.FormatVersion != FormatVersion {
				t.Fatalf("header = %s v%d", env.Format, env.FormatVersion)
			}
			if _, err := time.Parse(time.RFC3339, env.CreatedAt); err != nil {
				t.Fatalf("created_at %q: %v", env.CreatedAt, err)
			}
			if env.AppVersion == "" {
				t.Fatal("app_version is empty")
			}
			if env.Encrypted != (tc.mode != ModeNone) {
				t.Fatalf("encrypted = %v for mode %s", env.Encrypted, tc.mode)
			}
			if env.EncryptionMode != tc.mode {
				t.Fatalf("mode = %s, want %s", env.EncryptionMode, tc.mode)
			}
			switch tc.mode {
			case ModeNone:
				if env.Payload == nil || env.Nonce != "" || env.Ciphertext != "" || env.KDF != nil {
					t.Fatalf("plain envelope carries encryption fields")
				}
			case ModePassword:
				if env.Payload != nil {
					t.Fatal("encrypted envelope carries a plaintext payload")
				}
				k := env.KDF
				if k == nil {
					t.Fatal("no kdf params")
				}
				if k.Algorithm != KDFArgon2id || k.Time != 3 || k.MemoryKiB != 64*1024 || k.Threads != 4 || k.KeyLen != 32 {
					t.Fatalf("kdf params = %+v", k)
				}
				salt, err := base64.StdEncoding.DecodeString(k.Salt)
				if err != nil || len(salt) != 16 {
					t.Fatalf("salt = %q (%d bytes, err %v)", k.Salt, len(salt), err)
				}
			case ModeInstanceKey:
				if env.Payload != nil || env.KDF != nil {
					t.Fatal("instance-key envelope must not carry a payload or kdf params")
				}
			}
			if tc.mode != ModeNone {
				nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
				if err != nil || len(nonce) != 12 {
					t.Fatalf("nonce = %q (%d bytes, err %v)", env.Nonce, len(nonce), err)
				}
				if env.Ciphertext == "" {
					t.Fatal("no ciphertext")
				}
			}

			_, payload := openFile(t, env, tc.pass, f.key)
			if payload.Admin.Username != "admin" {
				t.Fatalf("admin username = %q", payload.Admin.Username)
			}
			if payload.Admin.PasswordHash == "" {
				t.Fatal("password hash missing")
			}
			if len(payload.Settings) != 2 {
				t.Fatalf("settings = %d", len(payload.Settings))
			}
			if payload.Settings[0].Key != "per_page" || payload.Settings[1].Key != "robots_txt" {
				t.Fatalf("settings order = %q, %q", payload.Settings[0].Key, payload.Settings[1].Key)
			}
			if len(payload.Storages) != 2 {
				t.Fatalf("storages = %d", len(payload.Storages))
			}
			if payload.Storages[0].MountPath != "/archive" || payload.Storages[1].MountPath != "/main" {
				t.Fatalf("storages order = %q, %q", payload.Storages[0].MountPath, payload.Storages[1].MountPath)
			}
			if payload.Storages[1].AppSecret != "app-secret-1" || payload.Storages[1].RefreshToken != "refresh-1" {
				t.Fatalf("credentials not portable: %+v", payload.Storages[1])
			}
			payloads = append(payloads, payload)
		})
	}
	// All three modes carry the same data.
	for i := 1; i < len(payloads); i++ {
		if !equalPayloads(payloads[0], payloads[i]) {
			t.Fatalf("payload from mode %d differs from password mode", i)
		}
	}
}

func equalPayloads(a, b *Payload) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func TestEncryptedBackupHidesCredentials(t *testing.T) {
	f := newFixture(t)
	for _, mode := range []struct {
		name    string
		mode    string
		pass    string
		confirm bool
	}{
		{"password", ModePassword, "correct horse battery", false},
		{"instance-key", ModeInstanceKey, "", false},
	} {
		env := f.export(mode.mode, mode.pass, mode.confirm)
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		for _, secretValue := range []string{"app-secret-1", "app-secret-2", "refresh-1", "refresh-2"} {
			if strings.Contains(string(data), secretValue) {
				t.Fatalf("%s backup leaks %q", mode.name, secretValue)
			}
		}
	}
}

// ---- 2. salt and nonce uniqueness ----

func TestSaltAndNonceAreFresh(t *testing.T) {
	f := newFixture(t)
	const pw = "correct horse battery"
	a := f.export(ModePassword, pw, false)
	b := f.export(ModePassword, pw, false)
	if a.KDF.Salt == b.KDF.Salt {
		t.Fatal("two exports reused the salt")
	}
	if a.Nonce == b.Nonce {
		t.Fatal("two exports reused the nonce")
	}
	if a.Ciphertext == b.Ciphertext {
		t.Fatal("two exports produced the same ciphertext")
	}
	// The instance-key mode uses a fresh nonce too.
	c := f.export(ModeInstanceKey, "", false)
	d := f.export(ModeInstanceKey, "", false)
	if c.Nonce == d.Nonce {
		t.Fatal("two instance-key exports reused the nonce")
	}
}

// ---- 3. tamper rejection ----

func TestTamperRejection(t *testing.T) {
	f := newFixture(t)
	const pw = "correct horse battery"
	base := f.export(ModePassword, pw, false)

	// mutate returns a fresh copy of base with one field changed.
	mutate := func(t *testing.T, fn func(env *Envelope)) *Envelope {
		t.Helper()
		clone := *base
		kdf := *base.KDF
		clone.KDF = &kdf
		fn(&clone)
		return &clone
	}
	flipCiphertext := func(t *testing.T, env *Envelope) {
		ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
		if err != nil {
			t.Fatal(err)
		}
		ct[0] ^= 0x01
		env.Ciphertext = base64.StdEncoding.EncodeToString(ct)
	}
	freshNonce := func(t *testing.T, env *Envelope) {
		env.Nonce = base64.StdEncoding.EncodeToString([]byte("0123456789ab"))
	}
	freshSalt := func(t *testing.T, env *Envelope) {
		env.KDF.Salt = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	}

	cases := []struct {
		name string
		fn   func(t *testing.T, env *Envelope)
	}{
		{"ciphertext byte", flipCiphertext},
		{"nonce", freshNonce},
		{"salt", freshSalt},
		{"format_version newer", func(_ *testing.T, env *Envelope) { env.FormatVersion = 2 }},
		{"format_version zero", func(_ *testing.T, env *Envelope) { env.FormatVersion = 0 }},
		{"created_at", func(_ *testing.T, env *Envelope) { env.CreatedAt = "2020-01-01T00:00:00Z" }},
		{"app_version", func(_ *testing.T, env *Envelope) { env.AppVersion = "9.9.9" }},
		{"encrypted=false", func(_ *testing.T, env *Envelope) { env.Encrypted = false }},
		{"encryption_mode=none", func(_ *testing.T, env *Envelope) { env.EncryptionMode = ModeNone }},
		{"encryption_mode=instance-key", func(_ *testing.T, env *Envelope) { env.EncryptionMode = ModeInstanceKey }},
		{"format identifier", func(_ *testing.T, env *Envelope) { env.Format = "other" }},
		{"kdf time", func(_ *testing.T, env *Envelope) { env.KDF.Time = 4 }},
		{"kdf memory", func(_ *testing.T, env *Envelope) { env.KDF.MemoryKiB = 32 * 1024 }},
		{"kdf threads", func(_ *testing.T, env *Envelope) { env.KDF.Threads = 2 }},
		{"kdf algorithm", func(_ *testing.T, env *Envelope) { env.KDF.Algorithm = "scrypt" }},
		{"kdf key_len", func(_ *testing.T, env *Envelope) { env.KDF.KeyLen = 16 }},
		{"kdf memory out of bounds", func(_ *testing.T, env *Envelope) { env.KDF.MemoryKiB = 1 << 20 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := mutate(t, func(e *Envelope) { tc.fn(t, e) })
			payload, err := OpenEnvelope(env, pw, f.key)
			if err == nil {
				t.Fatalf("tampered envelope was accepted")
			}
			if payload != nil {
				t.Fatalf("tampered envelope returned a payload")
			}
			// A file that is rejected must also fail when it makes a round trip through JSON.
			if data, mErr := json.Marshal(env); mErr == nil {
				if parsed, pErr := Parse(data); pErr == nil {
					if _, oErr := OpenEnvelope(parsed, pw, f.key); oErr == nil {
						t.Fatalf("tampered file was accepted after a JSON round trip")
					}
				}
			}
		})
	}
}

func TestWrongPasswordIsRejectedWithoutDetail(t *testing.T) {
	f := newFixture(t)
	env := f.export(ModePassword, "correct horse battery", false)
	payload, err := OpenEnvelope(env, "wrong horse battery", f.key)
	if err == nil || payload != nil {
		t.Fatal("wrong password accepted")
	}
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("err = %v, want ErrDecrypt", err)
	}
	if err.Error() != ErrDecrypt.Error() {
		t.Fatalf("error leaks detail: %q", err.Error())
	}
	// No password at all is a distinct, actionable error.
	if _, err := OpenEnvelope(env, "", f.key); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("empty password err = %v, want ErrPasswordRequired", err)
	}
	// A different instance master key cannot open an instance-key file.
	other := f.export(ModeInstanceKey, "", false)
	if _, err := OpenEnvelope(other, "", rawKey(99)); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("other master key err = %v, want ErrDecrypt", err)
	}
}

// ---- 4. password rules ----

func TestPasswordValidation(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name    string
		pass    string
		confirm bool
		wantErr bool
	}{
		{"empty", "", true, true},
		{"seven runes", "1234567", true, true},
		{"eight runes", "12345678", true, false},
		{"seven korean runes", "가나다라마바사", true, true},
		{"eight korean runes", "가나다라마바사아", true, false},
		{"128 runes", strings.Repeat("a", 128), true, false},
		{"129 runes", strings.Repeat("a", 129), true, true},
		{"spaces only", strings.Repeat(" ", 8), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := NewService(f.db, t.TempDir(), f.box, f.key).Export(context.Background(), ExportOptions{
				Mode: ModePassword, Password: tc.pass, ConfirmUnencrypted: tc.confirm,
			})
			if tc.wantErr {
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %v, want ValidationError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestPasswordWhitespaceIsPreserved(t *testing.T) {
	f := newFixture(t)
	const pw = "  spaced secret  "
	env := f.export(ModePassword, pw, false)
	if _, err := OpenEnvelope(env, pw, f.key); err != nil {
		t.Fatalf("exact password rejected: %v", err)
	}
	for _, wrong := range []string{"spaced secret", "   spaced secret  ", "  spaced secret "} {
		if _, err := OpenEnvelope(env, wrong, f.key); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("password %q err = %v, want ErrDecrypt", wrong, err)
		}
	}
}

func TestUnencryptedNeedsConfirmation(t *testing.T) {
	f := newFixture(t)
	_, _, err := NewService(f.db, t.TempDir(), f.box, f.key).Export(context.Background(), ExportOptions{Mode: ModeNone})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if !strings.Contains(ve.Msg, "확인") {
		t.Fatalf("message does not explain the confirmation: %q", ve.Msg)
	}
}

// ---- 6. payload validation ----

// validPayload is a payload that passes ValidatePayload.
func validPayload(t *testing.T) *Payload {
	t.Helper()
	hash, err := auth.HashPassword("restored-password")
	if err != nil {
		t.Fatal(err)
	}
	return &Payload{
		Admin: AdminRecord{Username: "admin", PasswordHash: hash, CreatedAt: "2024-01-01T00:00:00Z"},
		Settings: []SettingRecord{
			{Key: "per_page", Value: "30"},
			{Key: "robots_txt", Value: `"User-agent: *"`},
		},
		Storages: []StorageRecord{
			{ID: 1, Name: "Main", MountPath: "/main", Status: "active", AppKey: "k", AppSecret: "s", RefreshToken: "r"},
			{ID: 2, Name: "Empty", MountPath: "/empty", Status: "pending", AppKey: "k"},
		},
	}
}

func TestValidatePayloadAcceptsGoodPayload(t *testing.T) {
	if err := ValidatePayload(validPayload(t)); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
}

func TestValidatePayloadRejectsBadPayloads(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(p *Payload)
		wantSub string
	}{
		{"empty username", func(p *Payload) { p.Admin.Username = "   " }, "username"},
		{"empty hash", func(p *Payload) { p.Admin.PasswordHash = "" }, "password_hash"},
		{"bad hash", func(p *Payload) { p.Admin.PasswordHash = "not-a-hash" }, "password_hash"},
		{"unknown setting", func(p *Payload) {
			p.Settings = append(p.Settings, SettingRecord{Key: "not_a_setting", Value: "1"})
		}, "not_a_setting"},
		{"invalid setting value", func(p *Payload) {
			p.Settings[0] = SettingRecord{Key: "per_page", Value: "1"}
		}, "per_page"},
		{"setting value not json", func(p *Payload) {
			p.Settings[0] = SettingRecord{Key: "per_page", Value: "nope"}
		}, "per_page"},
		{"duplicate mount path", func(p *Payload) {
			p.Storages[1].MountPath = "/main"
		}, "겹칩니다"},
		{"nested mount path", func(p *Payload) {
			p.Storages[1].MountPath = "/main/sub"
		}, "겹칩니다"},
		{"duplicate id", func(p *Payload) { p.Storages[1].ID = p.Storages[0].ID }, "중복"},
		{"missing id", func(p *Payload) { p.Storages[0].ID = 0 }, "id"},
		{"relative mount", func(p *Payload) { p.Storages[0].MountPath = "main" }, "마운트"},
		{"unnormalized mount", func(p *Payload) { p.Storages[0].MountPath = "/main/" }, "정규화"},
		{"bad status", func(p *Payload) { p.Storages[0].Status = "broken" }, "status"},
		{"active without secret", func(p *Payload) { p.Storages[0].AppSecret = "" }, "app_secret"},
		{"active without token", func(p *Payload) { p.Storages[0].RefreshToken = "" }, "refresh_token"},
		{"active without app key", func(p *Payload) { p.Storages[0].AppKey = "" }, "app_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPayload(t)
			tc.mutate(p)
			err := ValidatePayload(p)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want ValidationError", err)
			}
			if !strings.Contains(ve.Msg, tc.wantSub) {
				t.Fatalf("message %q does not mention %q", ve.Msg, tc.wantSub)
			}
		})
	}
}

// TestValidatePayloadAcceptsBcryptHash is the compatibility contract for a backup written by a
// build that hashed the admin password with bcrypt: the file must pass validation, and the login
// path must verify the same password against that hash.
func TestValidatePayloadAcceptsBcryptHash(t *testing.T) {
	h, err := bcrypt.GenerateFromPassword([]byte("restored-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.VerifyPassword(string(h), "restored-password") {
		t.Fatal("login rejects a bcrypt hash that validation accepts")
	}
	if auth.VerifyPassword(string(h), "wrong-password") {
		t.Fatal("login accepts the wrong password against a bcrypt hash")
	}
	p := validPayload(t)
	p.Admin.PasswordHash = string(h)
	if err := ValidatePayload(p); err != nil {
		t.Fatalf("bcrypt hash rejected: %v", err)
	}
}

// TestValidatePayloadBcryptCostBoundary pins the same cap as login: cost 4..14 is accepted and
// cost 15..31 is rejected before any KDF runs. Only the cost field of a valid hash changes.
func TestValidatePayloadBcryptCostBoundary(t *testing.T) {
	base, err := bcrypt.GenerateFromPassword([]byte("restored-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(base), "$", 4)
	if len(parts) != 4 {
		t.Fatalf("test bcrypt hash has an unexpected shape: %q", base)
	}
	for _, cost := range []int{4, 10, 14} {
		parts[2] = fmt.Sprintf("%02d", cost)
		p := validPayload(t)
		p.Admin.PasswordHash = strings.Join(parts, "$")
		if err := ValidatePayload(p); err != nil {
			t.Errorf("bcrypt cost %d rejected: %v", cost, err)
		}
	}
	for _, cost := range []int{15, 31} {
		parts[2] = fmt.Sprintf("%02d", cost)
		p := validPayload(t)
		p.Admin.PasswordHash = strings.Join(parts, "$")
		err := ValidatePayload(p)
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Msg, "password_hash") {
			t.Errorf("bcrypt cost %d: err = %v, want a password_hash validation error", cost, err)
		}
	}
}

// TestValidatePayloadRejectsHostilePasswordHashParameters keeps a restored instance from accepting
// an argon2 hash whose verification would exhaust memory or a worker thread. Bcrypt is covered by
// the legacy compatibility test above; its high-cost CPU is a documented user-selected constraint.
func TestValidatePayloadRejectsHostilePasswordHashParameters(t *testing.T) {
	salt := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	sum := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	cases := []struct{ name, hash string }{
		{"argon2 memory 2 GiB", "$argon2id$v=19$m=2097152,t=3,p=2$" + salt + "$" + sum},
		{"argon2 time 32", "$argon2id$v=19$m=65536,t=32,p=2$" + salt + "$" + sum},
		{"argon2 threads 64", "$argon2id$v=19$m=65536,t=3,p=64$" + salt + "$" + sum},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validPayload(t)
			p.Admin.PasswordHash = c.hash
			err := ValidatePayload(p)
			var ve *ValidationError
			if !errors.As(err, &ve) || !strings.Contains(ve.Msg, "password_hash") {
				t.Fatalf("err = %v, want a password_hash validation error", err)
			}
		})
	}
}

// ---- 7. preview store ----

func TestPreviewSingleUseAndSessionBinding(t *testing.T) {
	store := NewPreviewStore()
	p := validPayload(t)
	env := &Envelope{FormatVersion: 1, Encrypted: true, EncryptionMode: ModePassword,
		CreatedAt: "2024-01-01T00:00:00Z", AppVersion: "1.0.0"}

	pv, err := store.Put("session-a", env, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.ID) != 32 {
		t.Fatalf("preview id %q is not 32 hex characters", pv.ID)
	}
	summary := pv.Summary()
	body, _ := json.Marshal(summary)
	for _, forbidden := range []string{
		"password_hash", "app_secret", "refresh_token", "app_key",
		p.Admin.PasswordHash, "app-secret-1", "refresh-1",
	} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("preview leaks %q: %s", forbidden, body)
		}
	}
	if summary.AdminUsername != "admin" || summary.SettingsCount != 2 || len(summary.Storages) != 2 {
		t.Fatalf("summary = %+v", summary)
	}

	// Another session cannot consume it, and the preview survives that attempt.
	if _, err := store.Take(pv.ID, "session-b"); !errors.Is(err, ErrPreviewInvalid) {
		t.Fatalf("other session err = %v", err)
	}
	if store.Len() != 1 {
		t.Fatal("rejected attempt consumed the preview")
	}
	// The owning session consumes it exactly once.
	if _, err := store.Take(pv.ID, "session-a"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if _, err := store.Take(pv.ID, "session-a"); !errors.Is(err, ErrPreviewInvalid) {
		t.Fatalf("replay err = %v, want ErrPreviewInvalid", err)
	}
	if _, err := store.Take("", "session-a"); !errors.Is(err, ErrPreviewInvalid) {
		t.Fatalf("empty id err = %v", err)
	}
}

func TestPreviewExpires(t *testing.T) {
	store := NewPreviewStore()
	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }
	pv, err := store.Put("session-a", &Envelope{FormatVersion: 1, EncryptionMode: ModeNone,
		CreatedAt: "2024-05-01T12:00:00Z", AppVersion: "1.0.0"}, validPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base.Add(PreviewTTL - time.Second) }
	if _, err := store.Take(pv.ID, "session-a"); err != nil {
		t.Fatalf("preview expired too early: %v", err)
	}
	store.now = func() time.Time { return base }
	pv2, err := store.Put("session-a", &Envelope{FormatVersion: 1, EncryptionMode: ModeNone,
		CreatedAt: "2024-05-01T12:00:00Z", AppVersion: "1.0.0"}, validPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base.Add(PreviewTTL + time.Second) }
	if _, err := store.Take(pv2.ID, "session-a"); !errors.Is(err, ErrPreviewInvalid) {
		t.Fatalf("expired preview err = %v", err)
	}
	if store.Len() != 0 {
		t.Fatalf("expired preview still stored (%d)", store.Len())
	}
}

// ---- 8. transactional apply ----

func TestApplyReEncryptsWithTargetInstanceKey(t *testing.T) {
	source := newFixture(t)
	env := source.export(ModePassword, "correct horse battery", false)
	_, payload := openFile(t, env, "correct horse battery", source.key)

	target := newFixtureWithKey(t, rawKey(120))
	// The target starts from a different state: one extra storage and another admin name.
	target.mustExec(`INSERT INTO storages(id, name, mount_path, root_path, app_key, app_secret_enc,
		created_at, updated_at) VALUES(9,'Local','/local','/','k',x'00',1,1)`)
	target.mustExec(`UPDATE admin SET username = 'someone-else'`)

	if err := ApplyTx(context.Background(), target.db, payload, ApplyOptions{Box: target.box}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var n int
	if err := target.db.QueryRow(`SELECT COUNT(*) FROM storages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("storages = %d, want 2 (the backup's list replaces the old one)", n)
	}
	if _, ok := lookupStorage(t, target.db, 9); ok {
		t.Fatal("storage that is not in the backup survived the restore")
	}

	// The credentials are sealed with the target's master key, not the source's.
	var enc []byte
	if err := target.db.QueryRow(`SELECT app_secret_enc FROM storages WHERE id = 1`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	plain, err := target.box.Open(enc)
	if err != nil {
		t.Fatalf("target box cannot open restored secret: %v", err)
	}
	if string(plain) != "app-secret-1" {
		t.Fatalf("secret = %q", plain)
	}
	if sourceBox, err := secret.New(source.key); err == nil {
		if _, err := sourceBox.Open(enc); err == nil {
			t.Fatal("restored secret is still readable with the source instance key")
		}
	}
	var adminName string
	if err := target.db.QueryRow(`SELECT username FROM admin WHERE id = 1`).Scan(&adminName); err != nil {
		t.Fatal(err)
	}
	if adminName != "admin" {
		t.Fatalf("admin = %q", adminName)
	}
	// Settings are the backup's values, and every default key exists.
	assertSettingsCount(t, target.db, 13)
	if v := settingValue(t, target.db, "per_page"); v != "42" {
		t.Fatalf("per_page = %q", v)
	}
}

func TestApplyRollsBackOnFailure(t *testing.T) {
	f := newFixture(t)
	f.mustExec(`INSERT INTO sessions(token_hash, created_at, expires_at, last_seen) VALUES('keep', 1, 9999999999, 1)`)
	before := snapshotState(t, f.db)

	payload := validPayload(t)
	payload.Admin.Username = "restored"
	payload.Storages = payload.Storages[:1]
	failErr := errors.New("boom")
	err := ApplyTx(context.Background(), f.db, payload, ApplyOptions{
		Box:         f.box,
		AfterInsert: func() error { return failErr },
	})
	if !errors.Is(err, failErr) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	after := snapshotState(t, f.db)
	if before != after {
		t.Fatalf("database changed after a failed restore:\nbefore %s\nafter  %s", before, after)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sessions = %d, want 1: a rollback must not log the admin out", n)
	}
}

func TestApplyClearsSessionsAndKeepsCaches(t *testing.T) {
	f := newFixture(t)
	f.mustExec(`INSERT INTO sessions(token_hash, created_at, expires_at, last_seen) VALUES('hash', 1, 9999999999, 1)`)
	f.mustExec(`INSERT INTO oauth_states(state_hash, storage_id, expires_at) VALUES('state', 1, 9999999999)`)
	f.mustExec(`INSERT INTO media_probe(file_id, rev, data, created_at) VALUES('file-1','rev-1','{}',1)`)

	if err := ApplyTx(context.Background(), f.db, validPayload(t), ApplyOptions{Box: f.box}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM sessions`); n != 0 {
		t.Fatalf("sessions = %d, want 0", n)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM oauth_states`); n != 0 {
		t.Fatalf("oauth_states = %d, want 0", n)
	}
	if n := count(t, f.db, `SELECT COUNT(*) FROM media_probe`); n != 1 {
		t.Fatalf("media_probe = %d, want 1: probe rows must be preserved", n)
	}
}

func TestApplyMergesDefaultSettings(t *testing.T) {
	f := newFixture(t)
	f.mustExec(`DELETE FROM settings`)
	p := validPayload(t)
	p.Settings = []SettingRecord{{Key: "per_page", Value: "77"}}
	if err := ApplyTx(context.Background(), f.db, p, ApplyOptions{Box: f.box}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertSettingsCount(t, f.db, 13)
	if v := settingValue(t, f.db, "per_page"); v != "77" {
		t.Fatalf("per_page = %q", v)
	}
	// A key the backup did not mention falls back to its default.
	if v := settingValue(t, f.db, "cache_cleanup_sec"); v != "120" {
		t.Fatalf("cache_cleanup_sec = %q, want the default", v)
	}
}

// ---- 10. snapshots ----

func TestSnapshotCreateListAndPrune(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	svc := NewService(f.db, t.TempDir(), f.box, f.key)
	dir := svc.SnapshotDir()
	svc.Snapshotter.Now = func() time.Time { return now }

	name, err := svc.CreateSnapshot(context.Background())
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if name != "pre-restore-20261101-030000.json" {
		t.Fatalf("name = %q", name)
	}
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("snapshot is empty")
	}

	// A second snapshot in the same second must not overwrite the first.
	second, err := svc.CreateSnapshot(context.Background())
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if second == name {
		t.Fatal("second snapshot reused the first name")
	}
	if second != "pre-restore-20261101-030000-2.json" {
		t.Fatalf("second name = %q", second)
	}

	// Two more seconds, seven snapshots in total; only five may remain.
	for i := 1; i <= 5; i++ {
		now = now.Add(time.Second)
		if _, err := svc.CreateSnapshot(context.Background()); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}
	parts, err := SnapshotFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != SnapshotMaxKeep {
		t.Fatalf("kept %d snapshots, want %d: %v", len(parts), SnapshotMaxKeep, names(parts))
	}
	// Newest first, and the newest one is the last written.
	if parts[0].name != "pre-restore-20261101-030005.json" {
		t.Fatalf("newest = %q", parts[0].name)
	}
	if _, err := os.Stat(filepath.Join(dir, "pre-restore-20261101-030000.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest snapshot was not pruned (err = %v)", err)
	}

	// Files this build did not write are never deleted or counted.
	foreign := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, failed := PruneSnapshots(dir, 1)
	if len(failed) != 0 {
		t.Fatalf("prune failures: %v", failed)
	}
	if len(removed) != SnapshotMaxKeep-1 {
		t.Fatalf("removed %d, want %d", len(removed), SnapshotMaxKeep-1)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign file was deleted: %v", err)
	}

	list, err := svc.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d entries, want 1", len(list))
	}
	if list[0].StorageCount != 2 || list[0].AppVersion == "" || list[0].Size == 0 {
		t.Fatalf("list entry = %+v", list[0])
	}
}

func TestSnapshotOpen(t *testing.T) {
	f := newFixture(t)
	svc := NewService(f.db, t.TempDir(), f.box, f.key)
	name, err := svc.CreateSnapshot(context.Background())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	env, payload, err := svc.OpenSnapshotFile(name)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	if env.EncryptionMode != ModeInstanceKey {
		t.Fatalf("mode = %s", env.EncryptionMode)
	}
	if len(payload.Storages) != 2 || payload.Storages[1].AppSecret != "app-secret-1" {
		t.Fatalf("snapshot payload = %+v", payload.Storages)
	}

	// Names that point outside the directory are rejected before the filesystem is touched.
	for _, bad := range []string{
		"../pre-restore-20261101-030000.json",
		"pre-restore-../../etc/passwd.json",
		`pre-restore-..\..\secret.json`,
		"pre-restore-2026/11/01.json",
		"pre-restore-20261101-030000.json/../../x",
		"/etc/passwd",
		"pre-restore-.json",
		"pre-restore-20261101-030000.txt",
		"..",
	} {
		if ValidSnapshotID(bad) {
			t.Fatalf("%q was accepted as a snapshot id", bad)
		}
		if _, _, err := svc.OpenSnapshotFile(bad); err == nil {
			t.Fatalf("%q was opened", bad)
		}
		var ve *ValidationError
		if err := openErr(svc, bad); !errors.As(err, &ve) {
			t.Fatalf("%q err = %v, want ValidationError", bad, err)
		}
	}
	// A well-formed name that does not exist is a 404, not a validation error.
	if _, _, err := svc.OpenSnapshotFile("pre-restore-20200101-000000.json"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("missing snapshot err = %v, want ErrSnapshotNotFound", err)
	}

	// A snapshot written by another instance cannot be opened with this master key.
	other := NewService(f.db, t.TempDir(), f.box, rawKey(200))
	other.Snapshotter.Dir = svc.Snapshotter.Dir
	other.Snapshotter.Now = func() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }
	otherName, err := other.CreateSnapshot(context.Background())
	if err != nil {
		t.Fatalf("other snapshot: %v", err)
	}
	_, _, err = svc.OpenSnapshotFile(otherName)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("other-instance snapshot err = %v, want ValidationError", err)
	}
	if strings.Contains(err.Error(), "app-secret-1") {
		t.Fatal("error leaks a credential")
	}
}

func openErr(svc *Service, id string) error {
	_, _, err := svc.OpenSnapshotFile(id)
	return err
}

// ---- helpers ----

func names(parts []snapshotPart) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.name
	}
	return out
}

func count(t *testing.T, d *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func settingValue(t *testing.T, d *sql.DB, key string) string {
	t.Helper()
	var v string
	if err := d.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		t.Fatalf("settings[%s]: %v", key, err)
	}
	return v
}

func assertSettingsCount(t *testing.T, d *sql.DB, want int) {
	t.Helper()
	if n := count(t, d, `SELECT COUNT(*) FROM settings`); n != want {
		t.Fatalf("settings rows = %d, want %d", n, want)
	}
}

func lookupStorage(t *testing.T, d *sql.DB, id int64) (string, bool) {
	t.Helper()
	var name string
	err := d.QueryRow(`SELECT name FROM storages WHERE id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("lookup storage %d: %v", id, err)
	}
	return name, true
}

// snapshotState renders the restored tables so a rollback can be compared byte for byte.
func snapshotState(t *testing.T, d *sql.DB) string {
	t.Helper()
	var out strings.Builder
	rows, err := d.Query(`SELECT username, password_hash, is_default, updated_at FROM admin ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var u, h string
		var def int
		var up int64
		if err := rows.Scan(&u, &h, &def, &up); err != nil {
			t.Fatal(err)
		}
		out.WriteString("admin|" + u + "|" + h + "|" + string(rune('0'+def)) + "|\n")
	}
	rows.Close()
	rows, err = d.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out.WriteString("setting|" + k + "|" + v + "\n")
	}
	rows.Close()
	rows, err = d.Query(`SELECT id, name, mount_path, status, app_key, hex(app_secret_enc), hex(refresh_token_enc) FROM storages ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var name, mount, status, key, sec, rt string
		if err := rows.Scan(&id, &name, &mount, &status, &key, &sec, &rt); err != nil {
			t.Fatal(err)
		}
		out.WriteString("storage|" + name + "|" + mount + "|" + status + "|" + key + "|" + sec + "|" + rt + "\n")
	}
	rows.Close()
	return out.String()
}
