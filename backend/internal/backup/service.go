package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
)

// Service is the backup and restore façade the admin API talks to. It bundles the database, the
// instance master key and the on-disk snapshot directory, and owns the preview store.
type Service struct {
	DB          *sql.DB
	DataDir     string
	Box         *secret.Box
	MasterKey   []byte
	Previews    *PreviewStore
	Snapshotter *Snapshotter

	// Now overrides the clock in tests.
	Now func() time.Time
}

// NewService wires a backup service for one instance.
func NewService(d *sql.DB, dataDir string, box *secret.Box, masterKey []byte) *Service {
	s := &Service{DB: d, DataDir: dataDir, Box: box, MasterKey: masterKey, Previews: NewPreviewStore()}
	s.Snapshotter = &Snapshotter{
		DB:        d,
		Dir:       s.SnapshotDir(),
		Box:       box,
		MasterKey: masterKey,
		Now:       func() time.Time { return s.now() },
	}
	return s
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// SnapshotDir is <dataDir>/backups, where pre-restore snapshots live.
func (s *Service) SnapshotDir() string {
	return filepath.Join(s.DataDir, snapshotDirName)
}

// Export builds a backup of the current instance state and reports how many storages it holds, so
// the caller can audit the operation without looking inside the (possibly encrypted) payload.
func (s *Service) Export(ctx context.Context, opts ExportOptions) (*Envelope, int, error) {
	opts.Box = s.Box
	if len(opts.MasterKey) == 0 {
		opts.MasterKey = s.MasterKey
	}
	if opts.Now == nil && s.Now != nil {
		opts.Now = s.Now
	}
	env, err := Export(ctx, s.DB, opts)
	if err != nil {
		return nil, 0, err
	}
	if env.Payload != nil {
		return env, len(env.Payload.Storages), nil
	}
	// The payload is sealed; count from the live list the export just read.
	storages, err := countStorages(ctx, s.DB)
	if err != nil {
		return env, 0, nil
	}
	return env, storages, nil
}

// countStorages returns the number of storages rows.
func countStorages(ctx context.Context, d *sql.DB) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM storages`).Scan(&n)
	return n, err
}

// OpenUpload parses and validates an uploaded backup file. Only password-protected and
// unencrypted files are accepted here: an instance-key file can only be read by the instance
// that wrote it, so it is restored through the pre-restore snapshot endpoint instead.
func (s *Service) OpenUpload(data []byte, password string) (*Envelope, *Payload, error) {
	env, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	if env.EncryptionMode == ModeInstanceKey {
		return nil, nil, &ValidationError{Msg: "이 백업은 이 인스턴스 전용(instance-key) 파일입니다. 복원 전 스냅숏 목록에서 선택하세요"}
	}
	payload, err := OpenEnvelope(env, password, s.MasterKey)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidatePayload(payload); err != nil {
		return nil, nil, err
	}
	return env, payload, nil
}

// OpenSnapshotFile reads one stored pre-restore snapshot by file name.
func (s *Service) OpenSnapshotFile(id string) (*Envelope, *Payload, error) {
	return s.Snapshotter.Open(id)
}

// StorePreview registers a validated payload under a fresh preview id bound to session and
// returns the summary the admin UI shows.
func (s *Service) StorePreview(session string, env *Envelope, payload *Payload) (*RestorePreview, error) {
	p, err := s.Previews.Put(session, env, payload)
	if err != nil {
		return nil, err
	}
	return p.Summary(), nil
}

// TakePreview consumes a preview id issued to session.
func (s *Service) TakePreview(id, session string) (*Preview, error) {
	return s.Previews.Take(id, session)
}

// ListSnapshots lists the stored pre-restore snapshots, newest first.
func (s *Service) ListSnapshots() ([]SnapshotInfo, error) { return s.Snapshotter.List() }

// CreateSnapshot writes a pre-restore snapshot of the current state and returns its file name.
func (s *Service) CreateSnapshot(ctx context.Context) (string, error) {
	return s.Snapshotter.Create(ctx)
}

// Apply restores pv: it first stores a pre-restore snapshot of the state being replaced, then
// replaces the admin account, settings and storages in one transaction. It returns the snapshot
// file name so the caller can log it.
func (s *Service) Apply(ctx context.Context, pv *Preview) (string, error) {
	if pv == nil || pv.Payload == nil {
		return "", &ValidationError{Msg: "적용할 미리보기가 없습니다"}
	}
	name, err := s.Snapshotter.Create(ctx)
	if err != nil {
		return "", err
	}
	if err := ApplyTx(ctx, s.DB, pv.Payload, ApplyOptions{Box: s.Box, Now: s.Now}); err != nil {
		return name, err
	}
	return name, nil
}
