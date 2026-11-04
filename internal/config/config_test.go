package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDefaultsToCacheDir(t *testing.T) {
	t.Setenv(EnvDataDir, "")
	p, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "sirene-mcp"); p.Dir != want {
		t.Errorf("Dir = %q, want %q", p.Dir, want)
	}
	if !strings.HasSuffix(p.DB, "sirene.duckdb") {
		t.Errorf("DB = %q", p.DB)
	}
}

func TestResolveEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(EnvDataDir, tmp)
	p, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != tmp {
		t.Errorf("Dir = %q, want %q", p.Dir, tmp)
	}
	if p.NewDB != filepath.Join(tmp, "sirene.duckdb.new") || p.WorkDir != filepath.Join(tmp, "work") ||
		p.ExportDir != filepath.Join(tmp, "exports") || p.TokenFile != filepath.Join(tmp, "token") ||
		p.LockFile != filepath.Join(tmp, "sync.lock") {
		t.Errorf("unexpected paths: %+v", p)
	}
}

func TestEnsureCreatesPrivateDir(t *testing.T) {
	p, err := Resolve(filepath.Join(t.TempDir(), "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p.Dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("stat = %v, %v", st, err)
	}
}
