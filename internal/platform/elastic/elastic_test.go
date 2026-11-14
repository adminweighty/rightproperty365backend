package elastic

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A made-up key: it is only ever handed to the fake transport below.
const fakeAPIKey = "dGVzdC1pZDp0ZXN0LWtleS1ub3QtcmVhbA=="

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
		// The official client checks this header to make sure it is talking to Elasticsearch.
		Header: http.Header{"X-Elastic-Product": {"Elasticsearch"}, "Content-Type": {"application/json"}},
	}
}

// The official client is only exercised against a fake transport here: it has
// never been pointed at a real cluster from this repository.
func TestClientSendsTheRequestWithTheAPIKeyHeader(t *testing.T) {
	var seen *http.Request
	client, err := New(Config{URL: "https://search.example.internal:9200", APIKey: fakeAPIKey, Timeout: time.Second,
		Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
			seen = r
			return response(200, `{"ok":true}`), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/rp365-test-listings-current/_search", strings.NewReader(`{}`))
	resp, err := client.Perform(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || seen == nil {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if seen.URL.Scheme != "https" || seen.URL.Host != "search.example.internal:9200" || seen.URL.Path != "/rp365-test-listings-current/_search" {
		t.Fatalf("the client must add the cluster address to the path: %s", seen.URL.Redacted())
	}
	// The scheme name is case-insensitive; the client writes "APIKey".
	if got := seen.Header.Get("Authorization"); !strings.EqualFold(got, "ApiKey "+fakeAPIKey) {
		t.Fatal("the request must carry the API key in the Authorization header")
	}
	// The client value itself does not print the key.
	if strings.Contains(fmt.Sprintf("%v %+v", client, client), fakeAPIKey) {
		t.Fatal("formatting the client exposes the API key")
	}
}

func TestClientWithoutAnAPIKeySendsNoAuthorization(t *testing.T) {
	var auth string
	client, err := New(Config{URL: "http://localhost:9200", Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		auth = r.Header.Get("Authorization")
		return response(200, `{}`), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "/_cluster/health", nil)
	if resp, err := client.Perform(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	if auth != "" {
		t.Fatalf("Authorization = %q, want none", auth)
	}
}

func TestClientConfigurationErrorsDoNotEchoTheAddress(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("an empty URL must be refused")
	}
	_, err := New(Config{URL: "https://elastic:hunter2@search.example.internal:9200/%zz", APIKey: fakeAPIKey})
	if err != nil && (strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), fakeAPIKey) || strings.Contains(err.Error(), "search.example")) {
		t.Fatalf("the error echoes the address or the key: %v", err)
	}
}

// A failed request is not retried by the client: search falls back to the
// database instead, within its time budget.
func TestClientDoesNotRetry(t *testing.T) {
	calls := 0
	client, err := New(Config{URL: "http://localhost:9200", Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return response(503, `{"error":{"type":"unavailable"}}`), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "/x/_search", strings.NewReader(`{}`))
	resp, err := client.Perform(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 1 || resp.StatusCode != 503 {
		t.Fatalf("calls = %d status = %d, want one attempt", calls, resp.StatusCode)
	}
}
