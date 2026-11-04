package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestRequireBearer(t *testing.T) {
	h := RequireBearer("s3cret", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for header, want := range map[string]int{
		"":              http.StatusUnauthorized,
		"Bearer wrong":  http.StatusUnauthorized,
		"s3cret":        http.StatusUnauthorized,
		"Bearer s3cret": http.StatusNoContent,
	} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Authorization %q: status %d, want %d", header, rec.Code, want)
		}
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	first, err := LoadOrCreateToken(path)
	if err != nil || len(first) != 64 {
		t.Fatalf("token %q, err %v", first, err)
	}
	info, _ := os.Stat(path)
	// Windows has no Unix permission bits: os.Stat always reports 0666.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v", info.Mode().Perm())
	}
	second, _ := LoadOrCreateToken(path)
	if second != first {
		t.Error("token changed between calls")
	}
}

// TestLoadOrCreateTokenConcurrent starts many first uses at once: they must
// all return the token that ends up in the file.
func TestLoadOrCreateTokenConcurrent(t *testing.T) {
	for round := 0; round < 20; round++ {
		path := filepath.Join(t.TempDir(), "sub", "token")
		const n = 16
		got := make([]string, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got[i], errs[i] = LoadOrCreateToken(path)
			}()
		}
		close(start)
		wg.Wait()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		onDisk := strings.TrimSpace(string(b))
		for i := range n {
			if errs[i] != nil || got[i] != onDisk {
				t.Fatalf("round %d caller %d: token %q err %v, file has %q", round, i, got[i], errs[i], onDisk)
			}
		}
		if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".token-*")); len(left) != 0 {
			t.Fatalf("temporary files left: %v", left)
		}
	}
}

func TestLoadOrCreateTokenReplacesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadOrCreateToken(path)
	if err != nil || len(tok) != 64 {
		t.Fatalf("token %q, err %v", tok, err)
	}
	if again, _ := LoadOrCreateToken(path); again != tok {
		t.Fatalf("token changed: %q then %q", tok, again)
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8765": true, "localhost:8765": true, "[::1]:8765": true,
		"0.0.0.0:8765": false, ":8765": false, "192.168.1.10:8765": false, "nonsense": false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v", addr, got)
		}
	}
}
