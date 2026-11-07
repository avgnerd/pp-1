package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/settings"
)

// ApplyOptions carries what a transactional restore needs.
type ApplyOptions struct {
	// Box reseals storage credentials with this instance's master key.
	Box *secret.Box
	// AfterInsert, when set, runs inside the transaction after every row was written and before
	// the commit. Tests use it to force a failure and prove nothing is kept.
	AfterInsert func() error
	// Now overrides the clock in tests.
	Now func() time.Time
}

// ApplyTx replaces the admin account, every setting and every storage with p in one SQLite
// transaction, and drops every login session and pending OAuth state. media_probe rows, the
// thumbs files and the hls files are never touched. Any failure rolls the whole transaction back,
// so the database keeps exactly the state it had before the call.
func ApplyTx(ctx context.Context, d *sql.DB, p *Payload, opts ApplyOptions) error {
	if p == nil {
		return &ValidationError{Msg: "복원할 데이터가 없습니다"}
	}
	if opts.Box == nil {
		return errors.New("복원에 필요한 마스터 키가 없습니다")
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	nowUnix := now().Unix()

	// Step 2: reseal every credential with this instance's master key before the transaction
	// starts, so an encryption failure cannot touch the database.
	type storageRow struct {
		rec        StorageRecord
		appSecret  []byte
		refreshTok []byte
	}
	prepared := make([]storageRow, 0, len(p.Storages))
	for _, rec := range p.Storages {
		row := storageRow{rec: rec}
		if rec.AppSecret != "" {
			b, err := opts.Box.Seal([]byte(rec.AppSecret))
			if err != nil {
				return fmt.Errorf("저장소 '%s'의 App secret을 암호화하지 못했습니다: %w", storageLabel(rec), err)
			}
			row.appSecret = b
		}
		if rec.RefreshToken != "" {
			b, err := opts.Box.Seal([]byte(rec.RefreshToken))
			if err != nil {
				return fmt.Errorf("저장소 '%s'의 refresh token을 암호화하지 못했습니다: %w", storageLabel(rec), err)
			}
			row.refreshTok = b
		}
		prepared = append(prepared, row)
	}

	merged, err := mergedSettings(p.Settings)
	if err != nil {
		return err
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("복원 트랜잭션을 시작하지 못했습니다: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 3.1: the single admin account.
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin`); err != nil {
		return fmt.Errorf("기존 관리자 계정을 지우지 못했습니다: %w", err)
	}
	adminUpdated := nowUnix
	if v, ok := parseRFC3339(p.Admin.CreatedAt); ok {
		adminUpdated = v
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO admin(id, username, password_hash, is_default, updated_at) VALUES(1, ?, ?, ?, ?)`,
		p.Admin.Username, p.Admin.PasswordHash, boolToInt(p.Admin.IsDefault), adminUpdated); err != nil {
		return fmt.Errorf("관리자 계정을 복원하지 못했습니다: %w", err)
	}

	// 3.2: settings — the factory defaults overlaid with the backup's values, so a key the file
	// does not mention still exists afterwards.
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings`); err != nil {
		return fmt.Errorf("기존 설정을 지우지 못했습니다: %w", err)
	}
	for _, k := range settings.AllKeys() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)`, k, merged[k]); err != nil {
			return fmt.Errorf("설정 '%s'을(를) 복원하지 못했습니다: %w", k, err)
		}
	}

	// 3.3: storages, keeping the ids the backup used so mount paths and clients line up.
	if _, err := tx.ExecContext(ctx, `DELETE FROM storages`); err != nil {
		return fmt.Errorf("기존 저장소를 지우지 못했습니다: %w", err)
	}
	for _, row := range prepared {
		rec := row.rec
		createdAt := nowUnix
		if v, ok := parseRFC3339(rec.CreatedAt); ok {
			createdAt = v
		}
		updatedAt := createdAt
		if v, ok := parseRFC3339(rec.UpdatedAt); ok {
			updatedAt = v
		}
		var authorizedAt int64
		if v, ok := parseRFC3339(rec.AuthorizedAt); ok {
			authorizedAt = v
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO storages(
			id, name, mount_path, root_path, app_key, app_secret_enc, refresh_token_enc,
			account_id, account_email, account_name, cache_ttl_min, status, auth_mode,
			redirect_uri, last_error, created_at, updated_at, authorized_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			rec.ID, rec.Name, rec.MountPath, defaultString(rec.RootPath, "/"), rec.AppKey,
			blobOrEmpty(row.appSecret), row.refreshTok, rec.AccountID, rec.AccountEmail, rec.AccountName,
			defaultInt(rec.CacheTTLMin, 30), defaultString(rec.Status, "pending"),
			defaultString(rec.AuthMode, "localhost"), rec.RedirectURI, rec.ErrorMsg,
			createdAt, updatedAt, authorizedAt); err != nil {
			return fmt.Errorf("저장소 '%s'을(를) 복원하지 못했습니다: %w", storageLabel(rec), err)
		}
	}

	// 3.4: transient data that must not survive a restore. dir_cache and search_cache live in
	// memory in this build and have no table; a missing table is not a restore failure.
	for _, table := range []string{"sessions", "oauth_states", "dir_cache", "search_cache"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil && !isMissingTable(err) {
			return fmt.Errorf("%s 정리에 실패했습니다: %w", table, err)
		}
	}

	if opts.AfterInsert != nil {
		if err := opts.AfterInsert(); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("복원을 저장하지 못했습니다: %w", err)
	}
	return nil
}

// mergedSettings renders the settings table content to write: factory defaults first, then every
// value the backup carries. Values stay JSON text, exactly as settings.Store stores them.
func mergedSettings(recs []SettingRecord) (map[string]string, error) {
	b, err := json.Marshal(settings.Defaults())
	if err != nil {
		return nil, err
	}
	var def map[string]json.RawMessage
	if err := json.Unmarshal(b, &def); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(def))
	for k, v := range def {
		out[k] = string(v)
	}
	for _, rec := range recs {
		out[rec.Key] = rec.Value
	}
	return out, nil
}

// isMissingTable reports SQLite's "no such table" error.
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// blobOrEmpty keeps the NOT NULL app_secret_enc column satisfied for a pending storage, which has
// no credential to seal yet.
func blobOrEmpty(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
