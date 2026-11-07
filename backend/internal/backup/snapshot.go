package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
)

// Pre-restore snapshot naming.
const (
	snapshotDirName  = "backups"
	snapshotPrefix   = "pre-restore-"
	snapshotSuffix   = ".json"
	snapshotTimeForm = "20060102-150405"
	// SnapshotMaxKeep is how many pre-restore snapshots stay on disk.
	SnapshotMaxKeep = 5
)

// snapshotIDPattern is the only shape a snapshot file name may have. It is checked before any
// path is built, so a request can never point outside the snapshot directory.
var snapshotIDPattern = regexp.MustCompile(`^pre-restore-[A-Za-z0-9_-]+\.json$`)

// ErrSnapshotNotFound reports a snapshot file that does not exist (HTTP 404).
var ErrSnapshotNotFound = errors.New("스냅숏 파일을 찾을 수 없습니다")

// ValidSnapshotID reports whether id is a plain snapshot file name.
func ValidSnapshotID(id string) bool {
	if !snapshotIDPattern.MatchString(id) {
		return false
	}
	return !strings.Contains(id, "..") && !strings.ContainsAny(id, `/\`)
}

// SnapshotInfo describes one stored snapshot for the admin list.
type SnapshotInfo struct {
	ID           string `json:"id"`
	CreatedAt    string `json:"created_at"`
	Size         int64  `json:"size"`
	AppVersion   string `json:"app_version"`
	StorageCount int    `json:"storage_count"`
}

// Snapshotter writes and reads pre-restore snapshots in <dataDir>/backups.
type Snapshotter struct {
	DB        *sql.DB
	Dir       string
	Box       *secret.Box
	MasterKey []byte
	Now       func() time.Time
}

// clock returns the configured time source.
func (s *Snapshotter) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Create exports the current state encrypted with the instance key and stores it in Dir. It
// returns the file name it wrote. Nothing is left behind when any step fails: the file is written
// to a temporary name and renamed into place only after the bytes are on disk.
func (s *Snapshotter) Create(ctx context.Context) (string, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", &SnapshotError{Err: fmt.Errorf("%s 디렉터리를 만들지 못했습니다: %w", s.Dir, err)}
	}
	env, err := Export(ctx, s.DB, ExportOptions{
		Mode:      ModeInstanceKey,
		Box:       s.Box,
		MasterKey: s.MasterKey,
		Now:       s.Now,
	})
	if err != nil {
		return "", &SnapshotError{Err: err}
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return "", &SnapshotError{Err: err}
	}
	at := s.clock().UTC()
	name, err := nextSnapshotName(s.Dir, at)
	if err != nil {
		return "", &SnapshotError{Err: err}
	}
	if err := writeFileAtomic(filepath.Join(s.Dir, name), data); err != nil {
		return "", &SnapshotError{Err: err}
	}
	if removed, failed := PruneSnapshots(s.Dir, SnapshotMaxKeep); len(removed) > 0 || len(failed) > 0 {
		slog.Info("backup: pre-restore snapshots pruned", "removed", len(removed), "failed", len(failed))
	}
	return name, nil
}

// List returns the stored snapshots, newest first.
func (s *Snapshotter) List() ([]SnapshotInfo, error) {
	parts, err := SnapshotFiles(s.Dir)
	if err != nil {
		return nil, &SnapshotError{Err: err}
	}
	out := make([]SnapshotInfo, 0, len(parts))
	for _, part := range parts {
		info := SnapshotInfo{ID: part.name, CreatedAt: part.at.Format(time.RFC3339)}
		path := filepath.Join(s.Dir, part.name)
		if st, err := os.Stat(path); err == nil {
			info.Size = st.Size()
		}
		if data, err := os.ReadFile(path); err == nil {
			if env, err := Parse(data); err == nil {
				info.CreatedAt = env.CreatedAt
				info.AppVersion = env.AppVersion
				// The payload holds plaintext credentials, so it is only counted here, never
				// returned and never cached.
				if payload, err := OpenEnvelope(env, "", s.MasterKey); err == nil {
					info.StorageCount = len(payload.Storages)
				}
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// Open reads one stored snapshot and returns its validated payload.
func (s *Snapshotter) Open(id string) (*Envelope, *Payload, error) {
	if !ValidSnapshotID(id) {
		return nil, nil, &ValidationError{Msg: "스냅숏 이름이 올바르지 않습니다"}
	}
	path := filepath.Join(s.Dir, id)
	if filepath.Dir(path) != filepath.Clean(s.Dir) {
		return nil, nil, &ValidationError{Msg: "스냅숏 이름이 올바르지 않습니다"}
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil, ErrSnapshotNotFound
	case err != nil:
		return nil, nil, &SnapshotError{Err: err}
	}
	env, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	if env.EncryptionMode != ModeInstanceKey {
		return nil, nil, &ValidationError{Msg: "복원 전 스냅숏 파일이 아닙니다"}
	}
	payload, err := OpenEnvelope(env, "", s.MasterKey)
	if err != nil {
		if errors.Is(err, ErrDecrypt) {
			return nil, nil, &ValidationError{Msg: "이 스냅숏을 현재 인스턴스 마스터 키로 복호화하지 못했습니다. secret.key가 바뀌었거나 다른 인스턴스의 스냅숏입니다"}
		}
		return nil, nil, err
	}
	if err := ValidatePayload(payload); err != nil {
		return nil, nil, err
	}
	return env, payload, nil
}

// writeFileAtomic writes data to a temporary file next to path and renames it into place, so a
// crash never leaves a half-written snapshot under the real name.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pre-restore-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// snapshotPart is a parsed snapshot file name.
type snapshotPart struct {
	name string
	at   time.Time
	seq  int
}

// parseSnapshotName splits "pre-restore-20261101-030000[-N].json".
func parseSnapshotName(name string) (snapshotPart, bool) {
	if !ValidSnapshotID(name) {
		return snapshotPart{}, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, snapshotPrefix), snapshotSuffix)
	if len(body) < len(snapshotTimeForm) {
		return snapshotPart{}, false
	}
	at, err := time.ParseInLocation(snapshotTimeForm, body[:len(snapshotTimeForm)], time.UTC)
	if err != nil {
		return snapshotPart{}, false
	}
	part := snapshotPart{name: name, at: at, seq: 1}
	if rest := body[len(snapshotTimeForm):]; rest != "" {
		if !strings.HasPrefix(rest, "-") {
			return snapshotPart{}, false
		}
		var seq int
		if _, err := fmt.Sscanf(rest, "-%d", &seq); err != nil || seq < 2 {
			return snapshotPart{}, false
		}
		part.seq = seq
	}
	return part, true
}

// snapshotName renders the file name for at and seq.
func snapshotName(at time.Time, seq int) string {
	base := snapshotPrefix + at.UTC().Format(snapshotTimeForm)
	if seq > 1 {
		base += fmt.Sprintf("-%d", seq)
	}
	return base + snapshotSuffix
}

// nextSnapshotName picks a name for at that no other snapshot in dir uses yet. Two restores in
// the same second get "-2", "-3", ... instead of overwriting each other.
func nextSnapshotName(dir string, at time.Time) (string, error) {
	for seq := 1; seq < 1000; seq++ {
		name := snapshotName(at, seq)
		_, err := os.Stat(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("같은 시각의 스냅숏 이름을 너무 많이 만들었습니다")
}

// SnapshotFiles lists the well-formed snapshot file names in dir, newest first.
func SnapshotFiles(dir string) ([]snapshotPart, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	parts := make([]snapshotPart, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if part, ok := parseSnapshotName(e.Name()); ok {
			parts = append(parts, part)
		}
	}
	sort.Slice(parts, func(i, j int) bool {
		if !parts[i].at.Equal(parts[j].at) {
			return parts[i].at.After(parts[j].at)
		}
		return parts[i].seq > parts[j].seq
	})
	return parts, nil
}

// PruneSnapshots keeps the newest keep snapshots in dir and deletes the rest. Files this build did
// not write are neither counted nor removed. Removing an old snapshot may fail without failing
// the restore that triggered it: the failure is logged and the file name returned.
func PruneSnapshots(dir string, keep int) (removed, failed []string) {
	parts, err := SnapshotFiles(dir)
	if err != nil {
		return nil, nil
	}
	if keep < 0 {
		keep = 0
	}
	for i, part := range parts {
		if i < keep {
			continue
		}
		if err := os.Remove(filepath.Join(dir, part.name)); err != nil {
			failed = append(failed, part.name)
			slog.Warn("backup: could not prune pre-restore snapshot", "file", part.name, "err", err)
			continue
		}
		removed = append(removed, part.name)
	}
	return removed, failed
}
