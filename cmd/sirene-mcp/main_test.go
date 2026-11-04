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
			return errors.New("boom") // must be logged, loop continues
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
		t.Fatalf("sync calls = %d, want 3 (startup + 2 scheduled)", calls)
	}
	if want := syncer.NextRun(now).Sub(now); len(waits) < 2 || waits[0] != want {
		t.Errorf("waits = %v, want first %v", waits, want)
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
