package backup

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/dvfs/dropbox-vfs-manager/internal/pathutil"
	"github.com/dvfs/dropbox-vfs-manager/internal/settings"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
)

// ValidatePayload applies the rules a restore needs before it may touch the database: a usable
// admin account, settings keys this build knows with in-range values, and storages whose ids and
// mount paths do not collide. Every failure is reported in one message so the admin can fix the
// whole file in one pass.
func ValidatePayload(p *Payload) error {
	if p == nil {
		return &ValidationError{Msg: "백업에 복원할 데이터가 없습니다"}
	}
	var errs []string

	if strings.TrimSpace(p.Admin.Username) == "" {
		errs = append(errs, "admin.username: 관리자 이름이 비어 있습니다")
	}
	if strings.TrimSpace(p.Admin.PasswordHash) == "" {
		errs = append(errs, "admin.password_hash: 관리자 비밀번호 해시가 비어 있습니다")
	} else if err := validatePasswordHash(p.Admin.PasswordHash); err != nil {
		errs = append(errs, "admin.password_hash: "+err.Error())
	}

	for _, rec := range p.Settings {
		if err := settings.ValidateKeyValue(rec.Key, rec.Value); err != nil {
			errs = append(errs, err.Error())
		}
	}

	seenID := make(map[int64]string, len(p.Storages))
	type mountPoint struct{ name, path string }
	mounts := make([]mountPoint, 0, len(p.Storages))
	for i, rec := range p.Storages {
		label := fmt.Sprintf("storages[%d]", i)
		if rec.Name != "" {
			label += " '" + rec.Name + "'"
		}
		switch {
		case rec.ID <= 0:
			errs = append(errs, label+": id가 없습니다")
		default:
			if prev, dup := seenID[rec.ID]; dup {
				errs = append(errs, fmt.Sprintf("%s: id %d가 '%s' 저장소와 중복됩니다", label, rec.ID, prev))
			} else {
				seenID[rec.ID] = storageLabel(rec)
			}
		}
		if strings.TrimSpace(rec.Name) == "" {
			errs = append(errs, label+": 저장소 이름이 비어 있습니다")
		}
		norm, err := storage.NormalizeMountPath(rec.MountPath)
		switch {
		case err != nil:
			errs = append(errs, label+": 마운트 경로가 올바르지 않습니다 ("+err.Error()+")")
		case norm != rec.MountPath:
			errs = append(errs, fmt.Sprintf("%s: 마운트 경로 '%s'가 정규화된 '%s'와 다릅니다", label, rec.MountPath, norm))
		default:
			mounts = append(mounts, mountPoint{name: storageLabel(rec), path: norm})
		}
		switch rec.Status {
		case storage.StatusActive, storage.StatusPending, storage.StatusError:
		default:
			errs = append(errs, label+": status는 active, pending, error 중 하나여야 합니다")
		}
		if rec.Status != storage.StatusPending {
			if strings.TrimSpace(rec.AppKey) == "" {
				errs = append(errs, label+": app_key가 비어 있습니다")
			}
			if strings.TrimSpace(rec.AppSecret) == "" {
				errs = append(errs, label+": app_secret이 비어 있습니다")
			}
			if strings.TrimSpace(rec.RefreshToken) == "" {
				errs = append(errs, label+": refresh_token이 비어 있습니다")
			}
		}
	}
	// Two mount points may not be equal or nested: /a/b would be reachable through /a.
	for i := 0; i < len(mounts); i++ {
		for j := i + 1; j < len(mounts); j++ {
			if pathutil.MountsConflict(mounts[i].path, mounts[j].path) {
				errs = append(errs, fmt.Sprintf("마운트 경로 '%s'(%s)와 '%s'(%s)가 겹칩니다",
					mounts[i].path, mounts[i].name, mounts[j].path, mounts[j].name))
			}
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Msg: strings.Join(errs, "; ")}
	}
	return nil
}

// storageLabel names a storage for error messages.
func storageLabel(rec StorageRecord) string {
	if rec.Name != "" {
		return rec.Name
	}
	if rec.MountPath != "" {
		return rec.MountPath
	}
	return fmt.Sprintf("#%d", rec.ID)
}

// validatePasswordHash accepts the hashes this app can verify: the argon2id PHC string
// auth.HashPassword writes, or bcrypt for a backup produced by a build that used it. A random
// string is rejected so a damaged file cannot silently lock the admin out of the instance.
func validatePasswordHash(h string) error {
	if _, err := bcrypt.Cost([]byte(h)); err == nil {
		return nil
	}
	if validateArgon2PHC(h) == nil {
		return nil
	}
	return errors.New("비밀번호 해시 형식을 알 수 없습니다")
}

var errNotArgon2 = errors.New("not an argon2id hash")

// validateArgon2PHC checks the shape of an argon2id PHC string without running the KDF.
func validateArgon2PHC(h string) error {
	parts := strings.Split(h, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || !strings.HasPrefix(parts[2], "v=") {
		return errNotArgon2
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return errNotArgon2
	}
	if m < 8 || m > 1<<21 || t < 1 || t > 32 || p < 1 || p > 64 {
		return errNotArgon2
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return errNotArgon2
	}
	sum, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(sum) < 16 {
		return errNotArgon2
	}
	return nil
}
