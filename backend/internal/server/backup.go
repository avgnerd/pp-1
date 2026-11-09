package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dvfs/dropbox-vfs-manager/internal/auth"
	"github.com/dvfs/dropbox-vfs-manager/internal/backup"
	"github.com/dvfs/dropbox-vfs-manager/internal/settings"
)

// maxBackupBytes is the size limit for an uploaded backup file: 10 MiB, inclusive.
const maxBackupBytes = 10 * 1024 * 1024

// maxJSONWrapperBytes leaves room for the {"backup":..., "password":...} wrapper around a
// 10 MiB file so the size check still applies to the file itself in that form.
const maxJSONWrapperBytes = 64 * 1024

// exportBackup writes the current instance state as a downloadable backup file.
func (s *Server) exportBackup(c *gin.Context) {
	var in struct {
		Encrypted          bool   `json:"encrypted"`
		Password           string `json:"password"`
		ConfirmUnencrypted bool   `json:"confirm_unencrypted"`
	}
	if err := bindJSON(c, &in); err != nil {
		s.logger.Audit("backup.export", "outcome", "failure", "reason", "request_invalid", "ip", c.ClientIP())
		fail(c, err)
		return
	}
	mode := backup.ModeNone
	if in.Encrypted {
		mode = backup.ModePassword
	}
	env, storages, err := s.Backups.Export(c.Request.Context(), backup.ExportOptions{
		Mode:               mode,
		Password:           in.Password,
		ConfirmUnencrypted: in.ConfirmUnencrypted,
	})
	if err != nil {
		s.logger.Audit("backup.export", "outcome", "failure", "reason", auditReason(err),
			"mode", mode, "storages", "unknown", "ip", c.ClientIP())
		writeBackupError(c, err)
		return
	}
	body, err := json.Marshal(env)
	if err != nil {
		s.logger.Audit("backup.export", "outcome", "failure", "reason", "encode_failed", "mode", mode, "ip", c.ClientIP())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "백업 파일을 만들지 못했습니다"})
		return
	}
	s.logger.Audit("backup.export", "outcome", "success", "mode", mode, "storages", storages, "ip", c.ClientIP())
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Disposition", `attachment; filename="`+backupFileName(env.CreatedAt)+`"`)
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// restorePreview validates an uploaded backup file and returns what a restore would change.
func (s *Server) restorePreview(c *gin.Context) {
	data, password, err := readBackupUpload(c)
	if err != nil {
		s.logger.Audit("restore.preview", "outcome", "failure", "reason", auditReason(err),
			"mode", "unknown", "storages", "unknown", "ip", c.ClientIP())
		writeBackupError(c, err)
		return
	}
	env, payload, err := s.Backups.OpenUpload(data, password)
	if err != nil {
		s.logger.Audit("restore.preview", "outcome", "failure", "reason", auditReason(err),
			"mode", "unknown", "storages", "unknown", "ip", c.ClientIP())
		if errors.Is(err, backup.ErrPasswordRequired) {
			// Hand back the plaintext header so the UI can say what it is asking the password for
			// (format version, creation time, app version) without decrypting anything.
			resp := gin.H{"error": "백업 비밀번호를 입력하세요", "code": "password_required"}
			if header, perr := backup.Parse(data); perr == nil {
				resp["format_version"] = header.FormatVersion
				resp["created_at"] = header.CreatedAt
				resp["app_version"] = header.AppVersion
				resp["encrypted"] = header.Encrypted
				resp["encryption_mode"] = header.EncryptionMode
			}
			c.JSON(http.StatusBadRequest, resp)
			return
		}
		writeBackupError(c, err)
		return
	}
	session, _ := c.Cookie(auth.CookieName)
	preview, err := s.Backups.StorePreview(session, env, payload)
	if err != nil {
		s.logger.Audit("restore.preview", "outcome", "failure", "reason", auditReason(err),
			"mode", env.EncryptionMode, "storages", len(payload.Storages), "ip", c.ClientIP())
		writeBackupError(c, err)
		return
	}
	s.logger.Audit("restore.preview", "outcome", "success", "mode", env.EncryptionMode,
		"storages", len(payload.Storages), "ip", c.ClientIP())
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, preview)
}

// restoreApply applies a previewed backup. Only one apply may run at a time.
func (s *Server) restoreApply(c *gin.Context) {
	var in struct {
		PreviewID string `json:"preview_id"`
	}
	if err := bindJSON(c, &in); err != nil {
		s.logger.Audit("restore.apply", "outcome", "failure", "reason", "request_invalid", "ip", c.ClientIP())
		fail(c, err)
		return
	}
	if !s.restoreMu.TryLock() {
		s.logger.Audit("restore.apply", "outcome", "failure", "reason", "restore_in_progress", "ip", c.ClientIP())
		c.JSON(http.StatusConflict, gin.H{"error": "이미 복원 작업이 진행 중입니다", "code": "restore_in_progress"})
		return
	}
	defer s.restoreMu.Unlock()
	// Hold the storage mutation gate across the transaction and the runtime refresh, so a
	// create/update/delete/OAuth completion either finishes before the restore starts or gets a
	// conflict instead of writing a row the restore is replacing.
	s.St.BeginRestore()
	defer s.St.EndRestore()
	if s.restoreHook != nil {
		s.restoreHook()
	}

	session, _ := c.Cookie(auth.CookieName)
	preview, err := s.Backups.TakePreview(in.PreviewID, session)
	if err != nil {
		s.logger.Audit("restore.apply", "outcome", "failure", "reason", auditReason(err),
			"mode", "unknown", "storages", "unknown", "ip", c.ClientIP())
		writeBackupError(c, err)
		return
	}
	// Hold the credential lock across the admin/settings/storage replacement and the runtime
	// refresh, so a profile change that verified the pre-restore admin row cannot write it back
	// over the restored one.
	s.Auth.BeginRestore()
	defer s.Auth.EndRestore()
	snapshot, err := s.Backups.Apply(c.Request.Context(), preview)
	if err != nil {
		s.logger.Audit("restore.apply", "outcome", "failure", "reason", auditReason(err),
			"mode", preview.EncryptionMode, "storages", len(preview.Payload.Storages), "ip", c.ClientIP())
		writeBackupError(c, err)
		return
	}
	// The storage rows are the restored ones now. Invalidate pre-restore clients before the rest
	// of the refresh, so a callback from one of them cannot write over a restored row even if a
	// later refresh step fails.
	s.St.InvalidateClients()
	failed, err := s.refreshRuntime(c)
	if err != nil {
		s.logger.Audit("restore.apply", "outcome", "failure", "reason", "runtime_refresh_failed",
			"mode", preview.EncryptionMode, "storages", len(preview.Payload.Storages),
			"snapshot", snapshot, "ip", c.ClientIP())
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "복원 내용은 저장되었지만 서버의 실행 중 상태를 갱신하지 못했습니다. 서버를 다시 시작하세요",
			"code":  "runtime_refresh_failed",
		})
		return
	}
	s.logger.Audit("restore.apply", "outcome", "success", "mode", preview.EncryptionMode,
		"storages", len(preview.Payload.Storages), "snapshot", snapshot,
		"failed_storages", len(failed), "ip", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true, "failed_storages": failed})
}

// listSnapshots returns the stored pre-restore snapshots, newest first.
func (s *Server) listSnapshots(c *gin.Context) {
	list, err := s.Backups.ListSnapshots()
	if err != nil {
		s.logger.Audit("backup.snapshot.list", "outcome", "failure", "reason", "list_failed", "ip", c.ClientIP())
		writeBackupError(c, err)
		return
	}
	if list == nil {
		list = []backup.SnapshotInfo{}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"snapshots": list})
}

// previewSnapshot previews a stored pre-restore snapshot as a restore candidate.
func (s *Server) previewSnapshot(c *gin.Context) {
	id := c.Param("id")
	if !backup.ValidSnapshotID(id) {
		// The name is checked before any path is built, so nothing outside the snapshot
		// directory can be probed.
		s.logger.Audit("backup.snapshot.preview", "outcome", "failure", "reason", "snapshot_name_invalid", "ip", c.ClientIP())
		c.JSON(http.StatusBadRequest, gin.H{"error": "스냅숏 이름이 올바르지 않습니다", "code": "snapshot_name_invalid"})
		return
	}
	env, payload, err := s.Backups.OpenSnapshotFile(id)
	if err != nil {
		s.logger.Audit("backup.snapshot.preview", "outcome", "failure", "reason", auditReason(err),
			"snapshot", id, "ip", c.ClientIP())
		writeBackupError(c, err)
		return
	}
	session, _ := c.Cookie(auth.CookieName)
	preview, err := s.Backups.StorePreview(session, env, payload)
	if err != nil {
		s.logger.Audit("backup.snapshot.preview", "outcome", "failure", "reason", auditReason(err),
			"snapshot", id, "ip", c.ClientIP())
		writeBackupError(c, err)
		return
	}
	s.logger.Audit("backup.snapshot.preview", "outcome", "success", "mode", env.EncryptionMode,
		"storages", len(payload.Storages), "snapshot", id, "ip", c.ClientIP())
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, preview)
}

// refreshRuntime publishes the restored state without a restart: login sessions first, then
// settings in memory, caches, HLS sessions and storage clients. Revoking sessions before the other
// refreshes means a later refresh failure cannot leave a pre-restore cookie validating against the
// replaced database. It returns the names of storages whose Dropbox client could not be built;
// those are marked "error" and the restore stays valid.
func (s *Server) refreshRuntime(c *gin.Context) ([]string, error) {
	if err := s.Auth.RevokeAllSessionsDuringRestore(); err != nil {
		return nil, err
	}
	restored, err := settings.ReadAll(s.DB)
	if err != nil {
		return nil, err
	}
	s.Settings.ReplaceInMemory(restored)
	s.Cache.InvalidateAll("")
	s.HLS.StopSessions()
	failed, err := s.St.ResetAndInitClients()
	if err != nil {
		return nil, err
	}
	// The browser that asked for the restore holds a session that no longer exists.
	s.setSessionCookie(c, "", -1)
	if failed == nil {
		failed = []string{}
	}
	return failed, nil
}

// readBackupUpload reads the backup file and optional password from either a multipart form
// (fields "file" and "password") or a JSON body (a bare envelope, or {"backup":..., "password":...}).
// It never buffers more than one byte past the 10 MiB limit, so an oversized upload is rejected
// instead of being read to the end.
func readBackupUpload(c *gin.Context) ([]byte, string, error) {
	ct, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if ct == "multipart/form-data" {
		return readMultipartBackup(c)
	}
	return readJSONBackup(c)
}

func readMultipartBackup(c *gin.Context) ([]byte, string, error) {
	mr, err := c.Request.MultipartReader()
	if err != nil {
		return nil, "", toLargeOrBad(err)
	}
	var data []byte
	var password string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", errBodyUnreadable
		}
		switch part.FormName() {
		case "file":
			if data != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(part, maxBackupBytes+1))
				part.Close()
				continue
			}
			buf, err := io.ReadAll(io.LimitReader(part, maxBackupBytes+1))
			part.Close()
			if err != nil {
				return nil, "", errBodyUnreadable
			}
			if int64(len(buf)) > maxBackupBytes {
				return nil, "", errTooLarge
			}
			data = buf
		case "password":
			buf, err := io.ReadAll(io.LimitReader(part, 4096))
			part.Close()
			if err != nil {
				return nil, "", errBodyUnreadable
			}
			password = string(buf)
		default:
			_, _ = io.Copy(io.Discard, io.LimitReader(part, 1<<20))
			part.Close()
		}
	}
	if len(data) == 0 {
		return nil, "", &apiError{status: http.StatusBadRequest, msg: "백업 파일을 선택하세요", field: "file"}
	}
	return data, password, nil
}

func readJSONBackup(c *gin.Context) ([]byte, string, error) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBackupBytes+maxJSONWrapperBytes+1))
	if err != nil {
		return nil, "", errBodyUnreadable
	}
	if int64(len(body)) > maxBackupBytes+maxJSONWrapperBytes {
		return nil, "", errTooLarge
	}
	var wrapper struct {
		Backup   json.RawMessage `json:"backup"`
		Password string          `json:"password"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && len(wrapper.Backup) > 0 {
		if wrapper.Backup[0] == '"' {
			// The file may also be sent as a JSON string holding its text.
			var text string
			if err := json.Unmarshal(wrapper.Backup, &text); err != nil {
				return nil, "", errBodyUnreadable
			}
			if int64(len(text)) > maxBackupBytes {
				return nil, "", errTooLarge
			}
			return []byte(text), wrapper.Password, nil
		}
		if int64(len(wrapper.Backup)) > maxBackupBytes {
			return nil, "", errTooLarge
		}
		return wrapper.Backup, wrapper.Password, nil
	}
	if int64(len(body)) > maxBackupBytes {
		return nil, "", errTooLarge
	}
	if len(body) == 0 {
		return nil, "", &apiError{status: http.StatusBadRequest, msg: "백업 파일을 선택하세요", field: "file"}
	}
	return body, "", nil
}

// Errors raised while reading an upload.
var (
	errTooLarge = &apiError{status: http.StatusRequestEntityTooLarge,
		msg: "백업 파일이 10MB를 넘습니다. 더 작은 파일을 올리세요", field: "file"}
	errBodyUnreadable = &apiError{status: http.StatusBadRequest,
		msg: "업로드한 백업 파일을 읽지 못했습니다", field: "file"}
)

// toLargeOrBad maps a multipart reader setup failure to a client error.
func toLargeOrBad(err error) error {
	if err == http.ErrNotMultipart || err == http.ErrMissingBoundary {
		return &apiError{status: http.StatusBadRequest, msg: "업로드 형식이 올바르지 않습니다", field: "file"}
	}
	if errors.Is(err, http.ErrBodyReadAfterClose) {
		return errBodyUnreadable
	}
	return &apiError{status: http.StatusBadRequest, msg: "업로드 형식이 올바르지 않습니다", field: "file"}
}

// backupFileName renders the download name from the envelope's UTC creation time.
func backupFileName(createdAt string) string {
	at, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		at = time.Now().UTC()
	}
	return "dvfs-backup-" + at.UTC().Format("20060102-150405") + ".json"
}

// writeBackupError maps a backup error to an API response. Messages describe what went wrong and
// what to do next; they never contain credentials, hashes, passwords or ciphertext.
func writeBackupError(c *gin.Context, err error) {
	var ae *apiError
	var ver *backup.VersionError
	var ve *backup.ValidationError
	var cred *backup.CredentialError
	var snap *backup.SnapshotError
	switch {
	case errors.As(err, &ae):
		c.JSON(ae.status, gin.H{"error": ae.msg, "field": ae.field})
	case errors.Is(err, backup.ErrPreviewInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": backup.ErrPreviewInvalid.Error(), "code": "preview_invalid"})
	case errors.As(err, &ver):
		c.JSON(http.StatusBadRequest, gin.H{"error": ver.Error(), "code": "format_version_unsupported"})
	case errors.Is(err, backup.ErrPasswordRequired):
		c.JSON(http.StatusBadRequest, gin.H{"error": "백업 비밀번호를 입력하세요", "code": "password_required"})
	case errors.Is(err, backup.ErrDecrypt):
		c.JSON(http.StatusBadRequest, gin.H{"error": backup.ErrDecrypt.Error(), "code": "decrypt_failed"})
	case errors.Is(err, backup.ErrFormat):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "invalid_format"})
	case errors.As(err, &ve):
		c.JSON(http.StatusBadRequest, gin.H{"error": ve.Msg, "code": "payload_invalid"})
	case errors.Is(err, backup.ErrSnapshotNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": backup.ErrSnapshotNotFound.Error(), "code": "snapshot_not_found"})
	case errors.As(err, &cred):
		// This instance cannot read its own sealed credentials: a server-side problem, so 500.
		c.JSON(http.StatusInternalServerError, gin.H{"error": cred.Error(), "code": "credential_decrypt_failed"})
	case errors.As(err, &snap):
		c.JSON(http.StatusInternalServerError, gin.H{"error": snap.Error(), "code": "snapshot_failed"})
	default:
		slog.Error("backup request failed", "path", c.Request.URL.Path, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// auditReason classifies a failure for the audit log. Only the category is recorded, never the
// underlying detail, so nothing sensitive can slip into the log through an error string.
func auditReason(err error) string {
	var ae *apiError
	var ver *backup.VersionError
	var ve *backup.ValidationError
	var cred *backup.CredentialError
	var snap *backup.SnapshotError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &ae):
		if ae.status == http.StatusRequestEntityTooLarge {
			return "body_too_large"
		}
		if ae.status == http.StatusConflict {
			return "restore_in_progress"
		}
		return "request_invalid"
	case errors.Is(err, backup.ErrPreviewInvalid):
		return "preview_invalid"
	case errors.Is(err, backup.ErrPasswordRequired):
		return "password_missing"
	case errors.Is(err, backup.ErrDecrypt):
		return "decrypt_failed"
	case errors.As(err, &ver):
		return "unsupported_format_version"
	case errors.Is(err, backup.ErrFormat):
		return "format_invalid"
	case errors.As(err, &ve):
		return "payload_invalid"
	case errors.Is(err, backup.ErrSnapshotNotFound):
		return "snapshot_not_found"
	case errors.As(err, &cred):
		return "credential_decrypt_failed"
	case errors.As(err, &snap):
		return "snapshot_failed"
	}
	return "apply_failed"
}
