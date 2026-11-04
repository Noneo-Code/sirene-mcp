package syncer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastBackoff(t *testing.T) {
	old := downloadBackoff
	downloadBackoff = time.Millisecond
	t.Cleanup(func() { downloadBackoff = old })
}

func TestDownloadFollowsRedirect(t *testing.T) {
	fastBackoff(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/r", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/file", http.StatusFound)
	})
	mux.HandleFunc("/file", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Last-Modified", "Tue, 03 Nov 2026 10:00:00 GMT")
		_, _ = w.Write([]byte("abc"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dest := t.TempDir() + "/out.parquet"
	d, err := Download(context.Background(), srv.Client(), srv.URL+"/r", dest)
	if err != nil {
		t.Fatal(err)
	}
	if d.Bytes != 3 || !strings.HasSuffix(d.FinalURL, "/file") || d.LastModified != "Tue, 03 Nov 2026 10:00:00 GMT" || d.Path != dest {
		t.Fatalf("unexpected %+v", d)
	}
	if b, _ := os.ReadFile(dest); string(b) != "abc" {
		t.Fatalf("content %q", b)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part left behind")
	}
}

func TestDownloadRetriesThenSucceeds(t *testing.T) {
	fastBackoff(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			http.Error(w, "boom", 500)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	if _, err := Download(context.Background(), srv.Client(), srv.URL, t.TempDir()+"/f"); err != nil {
		t.Fatal(err)
	}
	if hits != 3 {
		t.Fatalf("hits=%d", hits)
	}
}

func TestDownloadTruncatedFails(t *testing.T) {
	fastBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("12345"))
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()
	dest := t.TempDir() + "/f"
	if _, err := Download(context.Background(), srv.Client(), srv.URL, dest); err == nil {
		t.Fatal("expected error")
	}
	for _, p := range []string{dest, dest + ".part"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s exists", p)
		}
	}
}

func TestDownloadEmptyBodyFails(t *testing.T) {
	fastBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	if _, err := Download(context.Background(), srv.Client(), srv.URL, t.TempDir()+"/f"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDownloadNoRetryAfterCancel(t *testing.T) {
	downloadBackoff = time.Second
	defer func() { downloadBackoff = 2 * time.Second }()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "x", 500)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Download(ctx, srv.Client(), srv.URL, t.TempDir()+"/f"); err == nil {
		t.Fatal("expected error")
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
}
