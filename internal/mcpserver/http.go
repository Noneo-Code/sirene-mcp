package mcpserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// RequireBearer rejects requests whose Authorization header does not carry token.
func RequireBearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sirene-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LoadOrCreateToken returns the token stored at path, creating a random one
// (0600) on first use.
//
// Concurrent first uses (serve and token started together, possibly in
// different processes) agree on one token: each candidate is written in full
// to a temporary file and published with a hard link, which fails if path
// already exists, so only one candidate is ever visible at path and the losers
// read the winner's. An existing empty file is replaced.
func LoadOrCreateToken(path string) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		t, err := readToken(path)
		if err != nil {
			return "", err
		}
		if t != "" {
			return t, nil
		}
		t, err = publishToken(path)
		if err == nil {
			return t, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		// Another caller published first (or the file is empty): read it.
	}
	return "", fmt.Errorf("token file %s stays empty; delete it to generate a new token", path)
}

// readToken returns the trimmed token at path, or "" if the file is missing.
// An empty file is removed so that publishToken can replace it.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove empty token file: %w", err)
		}
	}
	return t, nil
}

// publishToken writes a fresh token to path unless path exists, in which case
// the error wraps os.ErrExist.
func publishToken(path string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	t := hex.EncodeToString(raw)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create token dir: %w", err)
	}
	f, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, err = f.WriteString(t + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600) // CreateTemp already uses 0600; be explicit
	}
	if err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", err
		}
		return "", fmt.Errorf("write token: %w", err)
	}
	return t, nil
}

// IsLoopback reports whether addr (host:port) only listens on the local machine.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
