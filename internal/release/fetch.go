// Package release distributes releases: devices fetch signed channel
// manifests and encrypted images over plain HTTPS from any static host, and
// `ramekin publish` puts them on GitHub Releases.
package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrNotFound means the URL returned 404 (e.g. nothing published yet).
var ErrNotFound = errors.New("not found")

var client = &http.Client{Timeout: 30 * time.Minute}

// Fetch GETs url, following redirects (GitHub serves release assets from a
// separate signed-URL host). The caller closes the body.
func Fetch(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	// Ask caches for a fresh copy; channel manifests change in place.
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %w", rawURL, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", rawURL, resp.Status)
	}
	return resp.Body, nil
}

// FetchBytes fetches a small document, refusing more than limit bytes.
func FetchBytes(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	rc, err := Fetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", rawURL, limit)
	}
	return b, nil
}

// Resolve interprets ref relative to base, so manifests on any host may use
// relative object paths.
func Resolve(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}
