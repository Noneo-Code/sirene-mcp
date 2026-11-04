package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
