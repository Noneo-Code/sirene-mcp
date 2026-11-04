// Package config resolves where sirene-mcp keeps its state on disk.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// EnvDataDir overrides the default data directory.
const EnvDataDir = "SIRENE_MCP_DATA_DIR"

// Paths groups every file sirene-mcp reads or writes.
type Paths struct {
	Dir       string // root data directory (created with 0700)
	DB        string // live DuckDB database
	NewDB     string // database being built, swapped in once validated
	WorkDir   string // downloaded Parquet files
	ExportDir string // CSV exports
	TokenFile string // bearer token for the HTTP transport
	LockFile  string // single-sync lock
}

// Resolve returns the paths for dir, or for the default location when dir is
// empty: $SIRENE_MCP_DATA_DIR, otherwise <user cache dir>/sirene-mcp. The data
// is regenerable and several GB, so it lives in the cache directory.
func Resolve(dir string) (Paths, error) {
	if dir == "" {
		dir = os.Getenv(EnvDataDir)
	}
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return Paths{}, fmt.Errorf("locate user cache directory: %w", err)
		}
		dir = filepath.Join(base, "sirene-mcp")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve data directory: %w", err)
	}
	return Paths{
		Dir:       abs,
		DB:        filepath.Join(abs, "sirene.duckdb"),
		NewDB:     filepath.Join(abs, "sirene.duckdb.new"),
		WorkDir:   filepath.Join(abs, "work"),
		ExportDir: filepath.Join(abs, "exports"),
		TokenFile: filepath.Join(abs, "token"),
		LockFile:  filepath.Join(abs, "sync.lock"),
	}, nil
}

// Ensure creates the data directory with owner-only permissions.
func (p Paths) Ensure() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	// Tighten permissions if the directory already existed.
	if err := os.Chmod(p.Dir, 0o700); err != nil && !errors.Is(err, os.ErrPermission) { //nolint:gosec // directories need the execute bit
		return fmt.Errorf("restrict data directory permissions: %w", err)
	}
	return nil
}
