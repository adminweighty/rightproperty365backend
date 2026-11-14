// Package storage keeps files: onboarding documents and profile logos.
//
// Objects are private: nothing here produces a public URL. A file is only
// ever read back through an API route that streams it — an authenticated one
// for a document; for a logo, which is public content, the public route of
// the directory, which decides on every request whether the profile (and so
// its logo) is public.
//
// Keys are chosen by the caller and must contain no user input; NewKey makes
// a random one.
package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Driver names (STORAGE_DRIVER).
const (
	DriverLocal = "local"
	DriverS3    = "s3"
)

// ErrNotFound is returned by Open when the object does not exist.
var ErrNotFound = errors.New("storage: object not found")

// Storage is the port.
type Storage interface {
	// Put stores size bytes from r under key, replacing any object there.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Open returns the object's content, or ErrNotFound.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// Name is the driver name, for the startup log line.
	Name() string
}

var keyRe = regexp.MustCompile(`^[a-z0-9]+(/[a-z0-9]+)*$`)

// NewKey returns "<prefix>/<32 random hex digits>". The prefix is a fixed
// word chosen by the code ("onboarding", "logos"), never anything a user sent.
func NewKey(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate object key: %w", err)
	}
	return prefix + "/" + hex.EncodeToString(b[:]), nil
}

func checkKey(key string) error {
	if len(key) > 200 || !keyRe.MatchString(key) {
		return errors.New("storage: invalid object key")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Local filesystem (development)
// ---------------------------------------------------------------------------

// Local stores objects as files under one directory. Development only.
type Local struct{ dir string }

// NewLocal returns the filesystem adapter rooted at dir (created on demand).
func NewLocal(dir string) (*Local, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("storage: local directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve directory: %w", err)
	}
	return &Local{dir: abs}, nil
}

// Name implements Storage.
func (l *Local) Name() string { return DriverLocal }

// Dir returns the directory objects are written under.
func (l *Local) Dir() string { return l.dir }

func (l *Local) path(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	return filepath.Join(l.dir, filepath.FromSlash(key)), nil
}

// Put implements Storage. The file is written under a temporary name and
// renamed, so a reader never sees half a file.
func (l *Local) Put(_ context.Context, key string, r io.Reader, size int64, _ string) error {
	path, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("storage: create directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return fmt.Errorf("storage: create file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	written, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("storage: write file: %w", err)
	}
	if written != size {
		return fmt.Errorf("storage: wrote %d bytes, expected %d", written, size)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("storage: store file: %w", err)
	}
	return nil
}

// Open implements Storage.
func (l *Local) Open(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: open file: %w", err)
	}
	return f, nil
}

// Delete implements Storage.
func (l *Local) Delete(_ context.Context, key string) error {
	path, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: delete file: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// S3-compatible (MinIO)
// ---------------------------------------------------------------------------

// S3Config is how to reach an S3-compatible store such as MinIO.
type S3Config struct {
	// Endpoint is an http(s) URL, e.g. https://minio.internal:9000.
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	// UsePathStyle addresses objects as <endpoint>/<bucket>/<key> (MinIO)
	// instead of <bucket>.<endpoint>/<key>.
	UsePathStyle bool
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

// S3 stores objects in one private bucket. It never makes an object public
// and never produces a presigned URL.
//
// NOTE: tested against a fake HTTP transport only. It has never been run
// against MinIO or any other S3 service.
type S3 struct {
	client *minio.Client
	bucket string
}

// NewS3 builds the adapter. It does not contact the service.
func NewS3(cfg S3Config) (*S3, error) {
	endpoint, secure := cfg.Endpoint, false
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		endpoint, secure = strings.TrimPrefix(endpoint, "https://"), true
	case strings.HasPrefix(endpoint, "http://"):
		endpoint = strings.TrimPrefix(endpoint, "http://")
	default:
		return nil, errors.New("storage: S3 endpoint must be an http(s) URL")
	}
	endpoint = strings.TrimSuffix(endpoint, "/")
	if endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("storage: S3 endpoint, bucket, access key and secret key are all required")
	}
	lookup := minio.BucketLookupDNS
	if cfg.UsePathStyle {
		lookup = minio.BucketLookupPath
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       region, // set, so the client never asks the service for the bucket's region
		BucketLookup: lookup,
		Transport:    cfg.Transport,
		MaxRetries:   1,
	})
	if err != nil {
		// The client's error can quote the endpoint; do not pass it on.
		return nil, errors.New("storage: invalid S3 configuration")
	}
	return &S3{client: client, bucket: cfg.Bucket}, nil
}

// Name implements Storage.
func (s *S3) Name() string { return DriverS3 }

// s3Error keeps the service's error code and drops everything else: the
// message can carry the endpoint, the bucket and request identifiers.
func s3Error(op string, err error) error {
	code := minio.ToErrorResponse(err).Code
	if code == "" {
		code = "request failed"
	}
	return fmt.Errorf("storage: %s: %s", op, code)
}

// Put implements Storage.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if _, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType}); err != nil {
		return s3Error("put object", err)
	}
	return nil
}

// Open implements Storage.
func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, s3Error("get object", err)
	}
	// GetObject is lazy: Stat performs the request, so a missing object is
	// reported here and not on the first Read.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if code := minio.ToErrorResponse(err).Code; code == "NoSuchKey" || code == "NoSuchBucket" {
			return nil, ErrNotFound
		}
		return nil, s3Error("get object", err)
	}
	return obj, nil
}

// Delete implements Storage.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil
		}
		return s3Error("delete object", err)
	}
	return nil
}
