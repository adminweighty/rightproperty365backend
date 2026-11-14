package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestNewKeyIsRandomAndWellFormed(t *testing.T) {
	a, err := NewKey("onboarding")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewKey("onboarding")
	if a == b || !strings.HasPrefix(a, "onboarding/") || len(a) != len("onboarding/")+32 || checkKey(a) != nil {
		t.Fatalf("keys = %q, %q", a, b)
	}
}

func TestLocalRoundTrip(t *testing.T) {
	ctx := context.Background()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := NewKey("onboarding")
	content := []byte("%PDF-1.4 sample")
	if err := l.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/pdf"); err != nil {
		t.Fatal(err)
	}
	rc, err := l.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("content = %q", got)
	}
	// A short write is refused and leaves nothing behind.
	other, _ := NewKey("onboarding")
	if err := l.Put(ctx, other, bytes.NewReader(content), int64(len(content))+1, ""); err == nil {
		t.Fatal("a size mismatch must be refused")
	}
	if _, err := l.Open(ctx, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open after a failed put = %v, want ErrNotFound", err)
	}
	if err := l.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(ctx, key); err != nil {
		t.Fatalf("deleting a missing object must not fail: %v", err)
	}
	if _, err := l.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open after delete = %v", err)
	}
	// No temporary files are left in the directory tree.
	entries, _ := os.ReadDir(l.Dir() + "/onboarding")
	if len(entries) != 0 {
		t.Fatalf("left-over files: %v", entries)
	}
}

func TestKeysCannotEscapeTheDirectory(t *testing.T) {
	l, _ := NewLocal(t.TempDir())
	for _, key := range []string{"", "../secret", "onboarding/../../x", "/etc/passwd", `onboarding\x`, "onboarding/A", "a//b", "onboarding/x.pdf"} {
		if err := l.Put(context.Background(), key, strings.NewReader("x"), 1, ""); err == nil {
			t.Fatalf("key %q must be refused", key)
		}
		if _, err := l.Open(context.Background(), key); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("open %q = %v, want an invalid-key error", key, err)
		}
	}
}

// fakeS3 is an in-memory stand-in for an S3 service. NOTHING here talks to
// MinIO: the S3 adapter has never run against a real service.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	requests []string
	auth     []string
}

func (f *fakeS3) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Host+r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	resp := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{"Content-Type": {"application/xml"}}, Body: io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)), Request: r}
	}
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		// Over plain HTTP the client signs the body in chunks ("aws-chunked");
		// a real service decodes that, so the fake does too.
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			var plain []byte
			for rest := body; len(rest) > 0; {
				head, tail, ok := bytes.Cut(rest, []byte("\r\n"))
				if !ok {
					break
				}
				var n int
				fmt.Sscanf(string(head), "%x", &n)
				if n == 0 || n > len(tail) {
					break
				}
				plain = append(plain, tail[:n]...)
				rest = bytes.TrimPrefix(tail[n:], []byte("\r\n"))
			}
			body = plain
		}
		f.objects[r.URL.Path] = body
		out := resp(200, "")
		out.Header.Set("ETag", `"0123456789abcdef0123456789abcdef"`)
		return out, nil
	case http.MethodGet, http.MethodHead:
		body, ok := f.objects[r.URL.Path]
		if !ok {
			return resp(404, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`), nil
		}
		out := resp(200, string(body))
		out.Header.Set("Content-Type", "application/pdf")
		out.Header.Set("Content-Length", fmt.Sprint(len(body)))
		out.Header.Set("ETag", `"0123456789abcdef0123456789abcdef"`)
		out.Header.Set("Last-Modified", "Sat, 14 Nov 2026 09:00:00 GMT")
		if r.Method == http.MethodHead {
			out.Body = io.NopCloser(strings.NewReader(""))
		}
		return out, nil
	case http.MethodDelete:
		delete(f.objects, r.URL.Path)
		return resp(204, ""), nil
	}
	return resp(405, ""), nil
}

func testSecret() string { return "secret-" + strings.Repeat("k", 12) }

func TestS3AgainstAFakeTransport(t *testing.T) {
	ctx := context.Background()
	fake := &fakeS3{objects: map[string][]byte{}}
	s, err := NewS3(S3Config{Endpoint: "http://minio.internal.example:9000", Bucket: "rp365-private", AccessKey: "access", SecretKey: testSecret(),
		Region: "us-east-1", UsePathStyle: true, Transport: fake})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != DriverS3 {
		t.Fatalf("name = %q", s.Name())
	}
	key, _ := NewKey("onboarding")
	content := []byte("%PDF-1.4 sample document")
	if err := s.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/pdf"); err != nil {
		t.Fatal(err)
	}
	// Path-style addressing: <endpoint>/<bucket>/<key>, signed with SigV4.
	if got := fake.requests[0]; got != "PUT minio.internal.example:9000/rp365-private/"+key {
		t.Fatalf("put request = %q", got)
	}
	if !strings.HasPrefix(fake.auth[0], "AWS4-HMAC-SHA256 ") || strings.Contains(fake.auth[0], testSecret()) {
		t.Fatal("the request must be signed and must not carry the secret key")
	}
	rc, err := s.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("content = %q", got)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open after delete = %v, want ErrNotFound", err)
	}
	if err := s.Put(ctx, "../x", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("an invalid key must be refused before any request")
	}
}

func TestS3ConfigurationErrorsDoNotEchoSecrets(t *testing.T) {
	for _, cfg := range []S3Config{
		{},
		{Endpoint: "minio:9000", Bucket: "b", AccessKey: "a", SecretKey: testSecret()},
		{Endpoint: "https://minio.internal.example", Bucket: "", AccessKey: "a", SecretKey: testSecret()},
		{Endpoint: "https://minio.internal.example", Bucket: "b", AccessKey: "a"},
	} {
		_, err := NewS3(cfg)
		if err == nil {
			t.Fatalf("config %+v must be refused", cfg.Endpoint)
		}
		if strings.Contains(err.Error(), testSecret()) || strings.Contains(err.Error(), "minio.internal") {
			t.Fatalf("the error echoes configuration: %v", err)
		}
	}
}
