package release

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub implements the slice of the GitHub API that GitHub uses.
type fakeGitHub struct {
	mu       sync.Mutex
	releases map[int64]*fakeRelease
	nextID   int64
	branches map[string]bool
	files    map[string]string // path -> content
	sha      map[string]string // path -> blob sha
	commits  int
	srv      *httptest.Server
}

type fakeRelease struct {
	Release
	assets map[string][]byte
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{releases: map[int64]*fakeRelease{}, branches: map[string]bool{}, files: map[string]string{}, sha: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) client() *GitHub {
	return &GitHub{Repo: "o/r", Branch: "ramekin-channels", Token: "t", API: f.srv.URL, Uploads: f.srv.URL, Web: f.srv.URL + "/web"}
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer t" {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	var in map[string]any
	if strings.Contains(r.Header.Get("Content-Type"), "json") {
		json.NewDecoder(r.Body).Decode(&in)
	}
	p := strings.TrimPrefix(r.URL.Path, "/repos/o/r")
	switch {
	case strings.HasPrefix(r.URL.Path, "/repos/") && !strings.HasPrefix(r.URL.Path, "/repos/o/r"):
		http.NotFound(w, r)
	case r.Method == "GET" && p == "":
		reply(map[string]any{"private": false, "permissions": map[string]bool{"push": true}})
	case r.Method == "GET" && strings.HasPrefix(p, "/releases/tags/"):
		tag := strings.TrimPrefix(p, "/releases/tags/")
		for _, rel := range f.releases {
			if rel.TagName == tag && !rel.Draft {
				reply(f.view(rel))
				return
			}
		}
		http.NotFound(w, r)
	case r.Method == "GET" && p == "/releases":
		var out []Release
		for _, rel := range f.releases {
			out = append(out, f.view(rel))
		}
		reply(out)
	case r.Method == "POST" && p == "/releases":
		f.nextID++
		rel := &fakeRelease{Release: Release{ID: f.nextID, TagName: in["tag_name"].(string), Draft: in["draft"].(bool)}, assets: map[string][]byte{}}
		f.releases[rel.ID] = rel
		reply(f.view(rel))
	case strings.HasPrefix(p, "/releases/assets/"):
		id := strings.TrimPrefix(p, "/releases/assets/")
		rid, name, _ := strings.Cut(id, ":")
		n, _ := strconv.ParseInt(rid, 10, 64)
		if r.Header.Get("Accept") != "application/octet-stream" {
			http.Error(w, "want octet-stream", 400)
			return
		}
		w.Write(f.releases[n].assets[name])
	case strings.HasPrefix(p, "/releases/") && strings.HasSuffix(p, "/assets"):
		n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(p, "/releases/"), "/assets"), 10, 64)
		rel := f.releases[n]
		if !rel.Draft && rel.Immutable {
			http.Error(w, `{"message":"Cannot upload assets to an immutable release"}`, 422)
			return
		}
		b, _ := io.ReadAll(r.Body)
		rel.assets[r.URL.Query().Get("name")] = b
		reply(map[string]any{})
	case strings.HasPrefix(p, "/releases/"):
		n, _ := strconv.ParseInt(strings.TrimPrefix(p, "/releases/"), 10, 64)
		rel, ok := f.releases[n]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == "DELETE" {
			delete(f.releases, n)
			w.WriteHeader(204)
			return
		}
		rel.Draft = in["draft"].(bool)
		rel.Immutable = true
		rel.HTMLURL = "https://example/" + rel.TagName
		reply(f.view(rel))
	case r.Method == "GET" && strings.HasPrefix(p, "/branches/"):
		if !f.branches[strings.TrimPrefix(p, "/branches/")] {
			http.NotFound(w, r)
			return
		}
		reply(map[string]any{})
	case r.Method == "POST" && strings.HasPrefix(p, "/git/"):
		if p == "/git/commits" && len(in["parents"].([]any)) != 0 {
			http.Error(w, "channel branch must be an orphan", 400)
			return
		}
		if p == "/git/refs" {
			f.branches[strings.TrimPrefix(in["ref"].(string), "refs/heads/")] = true
		}
		f.commits++
		reply(map[string]any{"sha": fmt.Sprintf("sha%d", f.commits)})
	case strings.HasPrefix(p, "/contents/"):
		path := strings.TrimPrefix(p, "/contents/")
		if r.Method == "GET" {
			c, ok := f.files[path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			reply(map[string]any{"content": base64.StdEncoding.EncodeToString([]byte(c)), "sha": f.sha[path]})
			return
		}
		if !f.branches[in["branch"].(string)] {
			http.Error(w, "no branch", 404)
			return
		}
		got, _ := in["sha"].(string)
		if got != f.sha[path] {
			http.Error(w, `{"message":"sha does not match"}`, http.StatusConflict)
			return
		}
		b, _ := base64.StdEncoding.DecodeString(in["content"].(string))
		f.files[path] = string(b)
		f.commits++
		f.sha[path] = fmt.Sprintf("blob%d", f.commits)
		reply(map[string]any{})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 500)
	}
}

func (f *fakeGitHub) view(rel *fakeRelease) Release {
	v := rel.Release
	v.Assets = nil
	for name := range rel.assets {
		v.Assets = append(v.Assets, Asset{Name: name, URL: fmt.Sprintf("%s/repos/o/r/releases/assets/%d:%s", f.srv.URL, rel.ID, name)})
	}
	return v
}

func TestPublishFlow(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	g := f.client()
	file := filepath.Join(t.TempDir(), "img")
	os.WriteFile(file, []byte("encrypted image"), 0o644)

	if err := g.CheckRepo(ctx); err != nil {
		t.Fatal(err)
	}
	g.Repo = "o/missing"
	if err := g.CheckRepo(ctx); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing repo: %v", err)
	}
	g.Repo = "o/r"
	if _, err := g.GetRelease(ctx, "app-1.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	// An interrupted publish left a draft behind; it must be replaced.
	stale, _ := g.CreateDraft(ctx, "app-1.0", "app 1.0", "")
	rel, err := g.CreateDraft(ctx, "app-1.0", "app 1.0", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.releases[stale.ID]; ok || len(f.releases) != 1 {
		t.Fatal("stale draft not deleted")
	}
	if err := g.UploadAsset(ctx, rel, "app.sif.age", file, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	pub, err := g.Publish(ctx, rel)
	if err != nil || pub.Draft || !pub.Immutable {
		t.Fatalf("publish: %+v %v", pub, err)
	}
	if err := g.UploadAsset(ctx, pub, "late", file, "text/plain"); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("upload to an immutable release: %v", err)
	}
	b, err := g.DownloadAsset(ctx, "app-1.0", "app.sif.age", 1<<20)
	if err != nil || string(b) != "encrypted image" {
		t.Fatalf("download: %q %v", b, err)
	}
	if _, err := g.DownloadAsset(ctx, "app-1.0", "app.sif.age", 3); err == nil {
		t.Fatal("size limit not enforced")
	}
	if got := g.AssetURL("app-1.0", "app.sif.age"); got != f.srv.URL+"/web/o/r/releases/download/app-1.0/app.sif.age" {
		t.Fatalf("asset URL %s", got)
	}
}

func TestChannelFiles(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	g := f.client()
	p := ChannelPath("app", "stable")
	if _, _, err := g.ReadFile(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	// The first write creates the branch as an orphan.
	if err := g.WriteFile(ctx, p, []byte("v1"), "", "first"); err != nil {
		t.Fatal(err)
	}
	b, sha, err := g.ReadFile(ctx, p)
	if err != nil || string(b) != "v1" {
		t.Fatalf("read: %q %v", b, err)
	}
	if err := g.WriteFile(ctx, p, []byte("v2"), sha, "second"); err != nil {
		t.Fatal(err)
	}
	// A writer holding a stale sha conflicts instead of clobbering.
	if err := g.WriteFile(ctx, p, []byte("v3"), sha, "stale"); err == nil {
		t.Fatal("stale write accepted")
	}
	if b, _, _ := g.ReadFile(ctx, p); string(b) != "v2" {
		t.Fatalf("got %q", b)
	}
	g.Token = "wrong"
	if err := g.WriteFile(ctx, p, []byte("x"), "", "x"); err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("bad token: %v", err)
	}
}

func TestFetch(t *testing.T) {
	ctx := context.Background()
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials leaked to the asset host")
		}
		io.WriteString(w, "image bytes")
	}))
	defer assets.Close()
	// Like github.com/.../releases/download/..., redirect to another host.
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ch/stable.json":
			io.WriteString(w, `{"ok":true}`)
		case "/download/app.sif.age":
			http.Redirect(w, r, assets.URL+"/signed?x=1", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer web.Close()

	if b, err := FetchBytes(ctx, web.URL+"/ch/stable.json", 100); err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := FetchBytes(ctx, web.URL+"/ch/stable.json", 5); err == nil {
		t.Fatal("limit not enforced")
	}
	if _, err := FetchBytes(ctx, web.URL+"/ch/beta.json", 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	obj, _ := Resolve(web.URL+"/ch/stable.json", "../download/app.sif.age")
	if b, err := FetchBytes(ctx, obj, 100); err != nil || string(b) != "image bytes" {
		t.Fatalf("redirected download: %q %v", b, err)
	}
	if abs, _ := Resolve(web.URL+"/ch/stable.json", "https://cdn.example/x"); abs != "https://cdn.example/x" {
		t.Fatalf("absolute URL rewritten: %s", abs)
	}
}
