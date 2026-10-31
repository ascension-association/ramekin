package release

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// GitHub publishes releases to a repository's GitHub Releases and keeps
// channel manifests as files on a dedicated branch of the same repository.
//
// Layout:
//
//	release <name>-<version>        assets: <name>.sif.age, sbom.json, manifest.json
//	branch <branch>, <name>/<ch>.json   signed channel pointer devices poll
//
// With immutable releases enabled on the repository, published releases'
// assets and tags can never change, which is why releases are created as
// drafts, filled, and only then published.
type GitHub struct {
	Repo   string // owner/name
	Branch string
	Token  string

	// API, Uploads and Web default to github.com; tests point them elsewhere.
	API, Uploads, Web string
	HTTP              *http.Client
}

type Release struct {
	ID        int64   `json:"id"`
	TagName   string  `json:"tag_name"`
	Draft     bool    `json:"draft"`
	Immutable bool    `json:"immutable"`
	HTMLURL   string  `json:"html_url"`
	Assets    []Asset `json:"assets"`
}

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"url"` // API URL; works for private repositories
}

// DownloadAsset fetches a published release's asset through the API, so it
// works for private repositories too.
func (g *GitHub) DownloadAsset(ctx context.Context, tag, name string, limit int64) ([]byte, error) {
	rel, err := g.GetRelease(ctx, tag)
	if err != nil {
		return nil, err
	}
	for _, a := range rel.Assets {
		if a.Name != name {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return nil, err
		}
		var buf limitedBuffer
		buf.limit = limit
		if err := g.do(req, &buf); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("release %s has no asset %s: %w", tag, name, ErrNotFound)
}

// limitedBuffer receives a raw (non-JSON) response body in do.
type limitedBuffer struct {
	bytes.Buffer
	limit int64
}

func (g *GitHub) api() string {
	if g.API != "" {
		return g.API
	}
	return "https://api.github.com"
}

func (g *GitHub) uploads() string {
	if g.Uploads != "" {
		return g.Uploads
	}
	return "https://uploads.github.com"
}

func (g *GitHub) web() string {
	if g.Web != "" {
		return g.Web
	}
	return "https://github.com"
}

// Tag is the release tag for a version of an app.
func Tag(name, version string) string { return name + "-" + version }

// AssetURL is the public download URL of a release asset. It is stable, so
// manifests can be signed before the release is published.
func (g *GitHub) AssetURL(tag, asset string) string {
	return g.web() + "/" + g.Repo + "/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(asset)
}

// CheckRepo makes sure the repository exists and the token can write to it.
func (g *GitHub) CheckRepo(ctx context.Context) error {
	var r struct {
		Private     bool `json:"private"`
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := g.call(ctx, http.MethodGet, "/repos/"+g.Repo, nil, &r); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("repository %s not found (or the token can't see it); create it first", g.Repo)
		}
		return err
	}
	if !r.Permissions.Push {
		return fmt.Errorf("the GitHub token can't write to %s", g.Repo)
	}
	if r.Private {
		return fmt.Errorf("repository %s is private, but devices download releases anonymously; make it public", g.Repo)
	}
	return nil
}

// GetRelease returns the published release with tag, or ErrNotFound.
func (g *GitHub) GetRelease(ctx context.Context, tag string) (*Release, error) {
	var r Release
	if err := g.call(ctx, http.MethodGet, "/repos/"+g.Repo+"/releases/tags/"+url.PathEscape(tag), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListReleases returns all releases (published and draft), newest first.
func (g *GitHub) ListReleases(ctx context.Context) ([]Release, error) {
	var all []Release
	for page := 1; ; page++ {
		var rs []Release
		if err := g.call(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/releases?per_page=100&page=%d", g.Repo, page), nil, &rs); err != nil {
			return nil, err
		}
		all = append(all, rs...)
		if len(rs) < 100 {
			return all, nil
		}
	}
}

// CreateDraft creates a draft release. Any leftover draft with the same tag
// (from an interrupted publish) is deleted first.
func (g *GitHub) CreateDraft(ctx context.Context, tag, title, notes string) (*Release, error) {
	rs, err := g.ListReleases(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.Draft && r.TagName == tag {
			if err := g.call(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/releases/%d", g.Repo, r.ID), nil, nil); err != nil {
				return nil, fmt.Errorf("delete stale draft %s: %w", tag, err)
			}
		}
	}
	var r Release
	err = g.call(ctx, http.MethodPost, "/repos/"+g.Repo+"/releases", map[string]any{
		"tag_name": tag, "name": title, "body": notes, "draft": true,
	}, &r)
	return &r, err
}

// UploadAsset adds a file to a draft release.
func (g *GitHub) UploadAsset(ctx context.Context, rel *Release, name, file, contentType string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/repos/%s/releases/%d/assets?name=%s", g.uploads(), g.Repo, rel.ID, url.QueryEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, f)
	if err != nil {
		return err
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Content-Type", contentType)
	return g.do(req, nil)
}

// Publish makes a draft public (and immutable, if the repository enables
// immutable releases).
func (g *GitHub) Publish(ctx context.Context, rel *Release) (*Release, error) {
	var r Release
	err := g.call(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/releases/%d", g.Repo, rel.ID), map[string]any{"draft": false}, &r)
	return &r, err
}

// ChannelPath is the file holding a channel's manifest on the branch.
func ChannelPath(name, channel string) string { return path.Join(name, channel+".json") }

// ReadFile returns a file from the channel branch, or ErrNotFound. It uses
// the API (not raw.githubusercontent.com) so it never sees a cached copy.
func (g *GitHub) ReadFile(ctx context.Context, p string) ([]byte, string, error) {
	var c struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	err := g.call(ctx, http.MethodGet, "/repos/"+g.Repo+"/contents/"+p+"?ref="+url.QueryEscape(g.Branch), nil, &c)
	if err != nil {
		return nil, "", err
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
	return b, c.SHA, err
}

// WriteFile creates or replaces a file on the channel branch, creating the
// branch (with no history from other branches) if needed. sha must be the
// blob being replaced ("" for a new file), so concurrent writers conflict.
func (g *GitHub) WriteFile(ctx context.Context, p string, content []byte, sha, message string) error {
	if err := g.ensureBranch(ctx); err != nil {
		return err
	}
	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  g.Branch,
	}
	if sha != "" {
		body["sha"] = sha
	}
	return g.call(ctx, http.MethodPut, "/repos/"+g.Repo+"/contents/"+p, body, nil)
}

// ensureBranch creates the channel branch as an orphan containing a README,
// so it shares no history with the code branches.
func (g *GitHub) ensureBranch(ctx context.Context) error {
	err := g.call(ctx, http.MethodGet, "/repos/"+g.Repo+"/branches/"+url.PathEscape(g.Branch), nil, nil)
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	readme := "# Ramekin release channels\n\nSigned channel manifests that devices poll. Written by `ramekin publish` and `ramekin promote`; don't edit by hand.\n"
	var blob, tree, commit struct {
		SHA string `json:"sha"`
	}
	if err := g.call(ctx, http.MethodPost, "/repos/"+g.Repo+"/git/blobs", map[string]any{"content": readme, "encoding": "utf-8"}, &blob); err != nil {
		return fmt.Errorf("create branch %s: %w", g.Branch, err)
	}
	if err := g.call(ctx, http.MethodPost, "/repos/"+g.Repo+"/git/trees", map[string]any{
		"tree": []map[string]any{{"path": "README.md", "mode": "100644", "type": "blob", "sha": blob.SHA}},
	}, &tree); err != nil {
		return fmt.Errorf("create branch %s: %w", g.Branch, err)
	}
	if err := g.call(ctx, http.MethodPost, "/repos/"+g.Repo+"/git/commits", map[string]any{
		"message": "Create Ramekin release channels", "tree": tree.SHA, "parents": []string{},
	}, &commit); err != nil {
		return fmt.Errorf("create branch %s: %w", g.Branch, err)
	}
	return g.call(ctx, http.MethodPost, "/repos/"+g.Repo+"/git/refs", map[string]any{
		"ref": "refs/heads/" + g.Branch, "sha": commit.SHA,
	}, nil)
}

func (g *GitHub) call(ctx context.Context, method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.api()+p, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return g.do(req, out)
}

func (g *GitHub) do(req *http.Request, out any) error {
	if _, raw := out.(*limitedBuffer); raw {
		req.Header.Set("Accept", "application/octet-stream")
	} else {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, ErrNotFound)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		json.Unmarshal(b, &e)
		msg := e.Message
		if msg == "" {
			msg = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("GitHub %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, msg)
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *limitedBuffer:
		n, err := o.ReadFrom(io.LimitReader(resp.Body, o.limit+1))
		if err == nil && n > o.limit {
			err = fmt.Errorf("%s: larger than %d bytes", req.URL.Path, o.limit)
		}
		return err
	default:
		return json.NewDecoder(resp.Body).Decode(out)
	}
}
