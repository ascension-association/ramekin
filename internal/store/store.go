// Package store wraps the S3-compatible bucket that holds releases.
//
// Layout under the configured prefix:
//
//	releases/<version>/<name>.sif.age   age-encrypted, signed SIF (object-locked)
//	releases/<version>/sbom.json        syft SBOM, rescanned by `ramekin watch`
//	releases/<version>/manifest.json    signed manifest copy (object-locked)
//	channels/<channel>.json             signed pointer devices poll (versioned)
//	status/<device>.json                optional device status reports
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Options struct {
	Endpoint  string // https://host[:port]
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
}

type Store struct {
	c      *minio.Client
	bucket string
	prefix string
}

func New(o Options) (*Store, error) {
	if o.Endpoint == "" || o.Bucket == "" {
		return nil, errors.New("store endpoint and bucket are required")
	}
	u, err := url.Parse(o.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("store endpoint %q must be a URL like https://host", o.Endpoint)
	}
	c, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(o.AccessKey, o.SecretKey, ""),
		Secure:       u.Scheme != "http",
		Region:       o.Region,
		BucketLookup: minio.BucketLookupAuto,
	})
	if err != nil {
		return nil, err
	}
	return &Store{c: c, bucket: o.Bucket, prefix: strings.Trim(o.Prefix, "/")}, nil
}

func (s *Store) Key(parts ...string) string {
	return path.Join(append([]string{s.prefix}, parts...)...)
}

func ReleaseObject(name, version string) []string {
	return []string{"releases", version, name + ".sif.age"}
}
func ReleaseSBOM(version string) []string     { return []string{"releases", version, "sbom.json"} }
func ReleaseManifest(version string) []string { return []string{"releases", version, "manifest.json"} }
func Channel(name string) []string            { return []string{"channels", name + ".json"} }
func Status(device string) []string           { return []string{"status", device + ".json"} }

// CheckBucket verifies that versioning and object locking are enabled.
func (s *Store) CheckBucket(ctx context.Context) error {
	v, err := s.c.GetBucketVersioning(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("bucket %s: %w", s.bucket, err)
	}
	if !v.Enabled() {
		return fmt.Errorf("bucket %s: versioning is not enabled", s.bucket)
	}
	lock, _, _, _, err := s.c.GetObjectLockConfig(ctx, s.bucket)
	if err != nil || lock != "Enabled" {
		return fmt.Errorf("bucket %s: object lock is not enabled (it must be chosen when the bucket is created)", s.bucket)
	}
	return nil
}

type PutOpts struct {
	ContentType string
	// RetainUntil, when non-zero, applies an object-lock retention.
	RetainUntil time.Time
	Mode        string // GOVERNANCE | COMPLIANCE
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, o PutOpts) error {
	po := minio.PutObjectOptions{ContentType: o.ContentType}
	if !o.RetainUntil.IsZero() {
		mode := minio.RetentionMode(strings.ToUpper(o.Mode))
		po.Mode = mode
		po.RetainUntilDate = o.RetainUntil
		// Object-lock PUTs require a content checksum.
		po.SendContentMd5 = true
	}
	_, err := s.c.PutObject(ctx, s.bucket, key, r, size, po)
	return err
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// GetObject is lazy; Stat forces the request so missing keys error here.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}

func (s *Store) GetBytes(ctx context.Context, key string) ([]byte, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// List returns keys under a prefix (relative to the store prefix).
func (s *Store) List(ctx context.Context, parts ...string) ([]string, error) {
	var keys []string
	for o := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.Key(parts...) + "/", Recursive: true}) {
		if o.Err != nil {
			return nil, o.Err
		}
		keys = append(keys, o.Key)
	}
	return keys, nil
}

func IsNotFound(err error) bool {
	r := minio.ToErrorResponse(err)
	return r.StatusCode == http.StatusNotFound || r.Code == "NoSuchKey"
}
