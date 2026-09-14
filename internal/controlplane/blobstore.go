package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver
)

// BlobStore is the persistence backend the admin state stores
// (overrides, algorithms, routes, sticky, rate limits) read and write
// their JSON snapshots through. Keying by a short logical name (e.g.
// "routes") decouples the store from where the bytes actually live: a
// local file (the zero-dependency default) or a shared database (so
// multiple control-plane replicas share one source of truth — the basis
// for control-plane HA).
//
// Implementations must be safe for concurrent use.
type BlobStore interface {
	// Load returns the bytes previously saved under key, or (nil, nil) if
	// nothing has been saved yet (not an error — a first run).
	Load(key string) ([]byte, error)
	// Save atomically stores data under key.
	Save(key string, data []byte) error
}

// FileBlobStore stores each key as a JSON file in a directory. It
// preserves the project's original behaviour (local JSON files, no
// external database) and is the default backend.
type FileBlobStore struct {
	dir string
}

// NewFileBlobStore stores blobs as <dir>/<key>.json. The directory is
// created if missing.
func NewFileBlobStore(dir string) (*FileBlobStore, error) {
	if dir == "" {
		dir = "/var/lib/go-loadbalancer"
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("controlplane: create store dir %q: %w", dir, err)
	}
	return &FileBlobStore{dir: dir}, nil
}

func (s *FileBlobStore) path(key string) string {
	return filepath.Join(s.dir, key+".json")
}

// Load implements BlobStore.
func (s *FileBlobStore) Load(key string) ([]byte, error) {
	data, err := os.ReadFile(s.path(key))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

// Save implements BlobStore. It writes to a temp file and renames, so a
// reader never observes a half-written file.
func (s *FileBlobStore) Save(key string, data []byte) error {
	tmp := s.path(key) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(key))
}

// PostgresBlobStore stores blobs as rows in a single table, so multiple
// control-plane replicas pointed at the same database share one view of
// the admin state — the persistence half of control-plane HA. The table
// is created on first use.
type PostgresBlobStore struct {
	db *sql.DB
}

// NewPostgresBlobStore opens a connection pool to the given DSN (e.g.
// "postgres://user:pass@host:5432/db?sslmode=require") and ensures the
// backing table exists.
func NewPostgresBlobStore(dsn string) (*PostgresBlobStore, error) {
	if dsn == "" {
		return nil, fmt.Errorf("controlplane: postgres store requires a DSN (set LB_STORE_DSN)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("controlplane: open postgres: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("controlplane: ping postgres: %w", err)
	}
	s := &PostgresBlobStore{db: db}
	if err := s.ensureTable(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *PostgresBlobStore) ensureTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS jbalance_admin_state (
			key   TEXT PRIMARY KEY,
			data  BYTEA NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("controlplane: create state table: %w", err)
	}
	return nil
}

// Load implements BlobStore.
func (s *PostgresBlobStore) Load(key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM jbalance_admin_state WHERE key = $1`, key).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: load %q from postgres: %w", key, err)
	}
	return data, nil
}

// Save implements BlobStore via an upsert.
func (s *PostgresBlobStore) Save(key string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jbalance_admin_state (key, data, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET data = EXCLUDED.data, updated_at = now()`, key, data)
	if err != nil {
		return fmt.Errorf("controlplane: save %q to postgres: %w", key, err)
	}
	return nil
}

// Close releases the database pool.
func (s *PostgresBlobStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// fileStoreFromPath maps a legacy "<dir>/<name>.json" path to a
// FileBlobStore over <dir> plus the key "<name>", preserving the original
// on-disk layout for the backward-compatible NewXStore(path) constructors.
// An empty path yields a nil store (in-memory only, never persists).
func fileStoreFromPath(path string) (BlobStore, string) {
	if path == "" {
		return nil, ""
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	key := base
	if ext := filepath.Ext(base); ext != "" {
		key = base[:len(base)-len(ext)]
	}
	// Best-effort: if the dir can't be created the first Save surfaces the
	// error; Load of a missing file is already treated as empty.
	s := &FileBlobStore{dir: dir}
	return s, key
}

// NewBlobStore builds the configured blob store: "file" (default) or
// "postgres". For file, dir is the directory; for postgres, dsn is the
// connection string.
func NewBlobStore(backend, dir, dsn string) (BlobStore, func() error, error) {
	switch backend {
	case "", "file":
		s, err := NewFileBlobStore(dir)
		return s, func() error { return nil }, err
	case "postgres":
		s, err := NewPostgresBlobStore(dsn)
		if err != nil {
			return nil, func() error { return nil }, err
		}
		return s, s.Close, nil
	default:
		return nil, func() error { return nil }, fmt.Errorf("controlplane: unknown store backend %q (want file or postgres)", backend)
	}
}
