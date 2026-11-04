package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// downloadBackoff is the base wait between attempts (multiplied by the attempt
// number). Overridable in tests.
var downloadBackoff = 2 * time.Second

const downloadAttempts = 3

// Downloaded describes a completed download.
type Downloaded struct {
	Path         string
	FinalURL     string
	LastModified string
	Bytes        int64
}

// Download streams url to dest (via dest+".part", renamed only on success),
// following redirects, with up to 3 attempts. The caller's client controls
// timeouts; ctx cancellation stops retries.
func Download(ctx context.Context, client *http.Client, url, dest string) (Downloaded, error) {
	var lastErr error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		d, err := downloadOnce(ctx, client, url, dest)
		if err == nil {
			return d, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return Downloaded{}, fmt.Errorf("download %s: %w", url, ctx.Err())
		}
		if attempt < downloadAttempts {
			select {
			case <-ctx.Done():
				return Downloaded{}, fmt.Errorf("download %s: %w", url, ctx.Err())
			case <-time.After(downloadBackoff * time.Duration(attempt)):
			}
		}
	}
	return Downloaded{}, fmt.Errorf("download %s failed after %d attempts: %w", url, downloadAttempts, lastErr)
}

func downloadOnce(ctx context.Context, client *http.Client, url, dest string) (d Downloaded, err error) {
	part := dest + ".part"
	defer func() {
		if err != nil {
			_ = os.Remove(part)
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return d, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return d, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return d, fmt.Errorf("unexpected status %s", resp.Status)
	}
	f, err := os.Create(part)
	if err != nil {
		return d, err
	}
	n, copyErr := io.Copy(f, resp.Body)
	if copyErr != nil {
		_ = f.Close()
		return d, copyErr
	}
	if n == 0 {
		_ = f.Close()
		return d, errors.New("empty body")
	}
	if resp.ContentLength >= 0 && resp.Header.Get("Content-Encoding") == "" && n != resp.ContentLength {
		_ = f.Close()
		return d, fmt.Errorf("short download: got %d of %d bytes", n, resp.ContentLength)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return d, err
	}
	if err = f.Close(); err != nil {
		return d, err
	}
	if err = os.Rename(part, dest); err != nil {
		return d, err
	}
	return Downloaded{
		Path:         dest,
		FinalURL:     resp.Request.URL.String(),
		LastModified: resp.Header.Get("Last-Modified"),
		Bytes:        n,
	}, nil
}
