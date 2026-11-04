package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Noneo-Code/sirene-mcp/internal/config"
	"github.com/Noneo-Code/sirene-mcp/internal/sirenetest"
	"github.com/Noneo-Code/sirene-mcp/internal/syncer"
)

func TestStatusNoDatabase(t *testing.T) {
	p, _ := config.Resolve(t.TempDir())
	var b bytes.Buffer
	if err := printStatus(&b, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `no database yet, run "sirene-mcp sync"`) {
		t.Errorf("out = %q", b.String())
	}
}

func TestSyncThenStatus(t *testing.T) {
	dir := t.TempDir()
	ul, etab := sirenetest.Default()
	sirenetest.Write(t, dir, ul, etab)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := "unite_legale.parquet"
		if r.URL.Path == "/etab" {
			name = "etablissement.parquet"
		}
		http.ServeFile(w, r, filepath.Join(dir, name))
	}))
	defer srv.Close()
	p, _ := config.Resolve(t.TempDir())
	_, err := syncer.Run(context.Background(), p, srv.Client(),
		syncer.Options{ULURL: srv.URL + "/ul", EtabURL: srv.URL + "/etab"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := printStatus(&b, p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{p.DB, "5 legal units, 6 establishments", "exclude closed: false"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %q", want, b.String())
		}
	}
}

func TestServeRefusesRemoteWithoutFlag(t *testing.T) {
	err := run([]string{"serve", "--addr", "0.0.0.0:0", "--data-dir", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "--allow-remote") {
		t.Fatalf("err = %v, want mention of --allow-remote", err)
	}
}

func TestAutoSyncLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	var calls int
	var waits []time.Duration
	sync := func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("boom") // must be logged and retried
		}
		if calls == 3 {
			cancel()
		}
		return nil
	}
	after := func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		c := make(chan time.Time, 1)
		c <- now
		return c
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	autoSyncLoop(ctx, false, func() time.Time { return now }, after, sync, log)
	if calls != 3 {
		t.Fatalf("sync calls = %d, want 3 (failed startup, retry, scheduled)", calls)
	}
	if want := syncer.NextRun(now).Sub(now); len(waits) < 2 || waits[0] != autoSyncRetries[0] || waits[1] != want {
		t.Errorf("waits = %v, want [%v %v ...]", waits, autoSyncRetries[0], want)
	}

	// DB present: no startup sync, first sync only after the wait.
	ctx2, cancel2 := context.WithCancel(context.Background())
	calls = 0
	sync2 := func(context.Context) error { calls++; cancel2(); return nil }
	autoSyncLoop(ctx2, true, func() time.Time { return now }, after, sync2, log)
	if calls != 1 {
		t.Errorf("calls with existing DB = %d, want 1", calls)
	}
}

func TestAutoSyncGivesUpAfterRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	var calls int
	var waits []time.Duration
	sync := func(context.Context) error {
		calls++
		if calls > len(autoSyncRetries)+1 {
			cancel() // the scheduled run
			return nil
		}
		return errors.New("boom")
	}
	after := func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		c := make(chan time.Time, 1)
		c <- now
		return c
	}
	autoSyncLoop(ctx, false, func() time.Time { return now }, after, sync, slog.New(slog.NewTextHandler(io.Discard, nil)))
	want := append(append([]time.Duration{}, autoSyncRetries...), syncer.NextRun(now).Sub(now))
	if calls != len(autoSyncRetries)+2 || len(waits) != len(want) {
		t.Fatalf("calls = %d, waits = %v, want 3 attempts then the scheduled run", calls, waits)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("waits = %v, want %v", waits, want)
		}
	}
	var total time.Duration
	for _, d := range autoSyncRetries {
		total += d
	}
	if total < 30*time.Minute || total > 2*time.Hour {
		t.Errorf("retries span %v, want about an hour", total)
	}
}

func TestEnvFallbacks(t *testing.T) {
	t.Setenv("SIRENE_MCP_EXCLUDE_CLOSED", "true")
	t.Setenv("SIRENE_MCP_MEMORY_LIMIT", "2GB")
	t.Setenv("SIRENE_MCP_EXPORT_DIR", "/tmp/exports-env")
	fs, _ := newFlags("x")
	var o syncer.Options
	var exportDir string
	if err := syncFlags(fs, &o); err != nil {
		t.Fatal(err)
	}
	exportDirFlag(fs, &exportDir)
	if err := parse(fs, nil); err != nil {
		t.Fatal(err)
	}
	if !o.ExcludeClosed || o.MemoryLimit != "2GB" || exportDir != "/tmp/exports-env" {
		t.Errorf("env defaults: exclude=%v mem=%q export=%q", o.ExcludeClosed, o.MemoryLimit, exportDir)
	}

	// Flags win over the environment.
	fs, _ = newFlags("x")
	o, exportDir = syncer.Options{}, ""
	if err := syncFlags(fs, &o); err != nil {
		t.Fatal(err)
	}
	exportDirFlag(fs, &exportDir)
	if err := parse(fs, []string{"--exclude-closed=false", "--memory-limit", "8GB", "--export-dir", "/x"}); err != nil {
		t.Fatal(err)
	}
	if o.ExcludeClosed || o.MemoryLimit != "8GB" || exportDir != "/x" {
		t.Errorf("flags: exclude=%v mem=%q export=%q", o.ExcludeClosed, o.MemoryLimit, exportDir)
	}

	t.Setenv("SIRENE_MCP_EXCLUDE_CLOSED", "maybe")
	fs, _ = newFlags("x")
	if err := syncFlags(fs, &o); err == nil || !strings.Contains(err.Error(), "SIRENE_MCP_EXCLUDE_CLOSED") {
		t.Errorf("invalid bool: err = %v", err)
	}
	for _, v := range []string{"1", "0", "false", "TRUE", ""} {
		t.Setenv("SIRENE_MCP_EXCLUDE_CLOSED", v)
		if _, err := envBool("SIRENE_MCP_EXCLUDE_CLOSED"); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
}

func TestResolveTokenFromEnv(t *testing.T) {
	p, _ := config.Resolve(t.TempDir())
	t.Setenv("SIRENE_MCP_TOKEN", "  0123456789abcdef \n")
	if tok, err := resolveToken(p); err != nil || tok != "0123456789abcdef" {
		t.Errorf("token = %q, %v; want trimmed", tok, err)
	}
	for _, bad := range []string{"short", "   ", " 0123456789abcde "} {
		t.Setenv("SIRENE_MCP_TOKEN", bad)
		if _, err := resolveToken(p); err == nil || !strings.Contains(err.Error(), "16") {
			t.Errorf("%q: err = %v, want length error", bad, err)
		}
	}
	t.Setenv("SIRENE_MCP_TOKEN", "")
	if tok, err := resolveToken(p); err != nil || len(tok) < 16 {
		t.Errorf("generated token = %q, %v", tok, err)
	}
}
