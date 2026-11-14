// Package elastic builds the Elasticsearch client.
//
// It uses the official Go client, github.com/elastic/go-elasticsearch/v9 (the
// current stable major; a client major talks to the same server major). Only
// the client's transport is used: the listings module builds its own request
// bodies and reads the responses, so the queries are plain JSON that can be
// tested without a cluster.
//
// The API key is handed to the client once and travels only in the
// Authorization header it adds. Nothing here logs a request, a header or the key.
package elastic

import (
	"errors"
	"net/http"
	"time"

	elasticsearch "github.com/elastic/go-elasticsearch/v9"
)

// Config is how to reach the cluster.
type Config struct {
	// URL is the cluster address, e.g. https://search.internal:9200.
	URL string
	// APIKey is the base64 "id:api_key" value Elasticsearch issues. Empty
	// means no authentication (local development with security disabled).
	APIKey string
	// Timeout bounds every request at the HTTP level.
	Timeout time.Duration
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

// Client performs requests against Elasticsearch.
type Client struct{ es *elasticsearch.Client }

// New builds a client. It does not contact the cluster.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("elasticsearch: URL is empty")
	}
	transport := cfg.Transport
	if transport == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.ResponseHeaderTimeout = cfg.Timeout
		transport = base
	}
	es, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{cfg.URL},
		APIKey:    cfg.APIKey,
		Transport: transport,
		// A search has a one-second budget: a failed request falls back to the
		// database instead of being retried here. Indexing is retried by the outbox.
		DisableRetry: true,
	})
	if err != nil {
		// The client's error can quote the address; do not pass it on.
		return nil, errors.New("elasticsearch: invalid client configuration")
	}
	return &Client{es: es}, nil
}

// Perform sends a request. The request URL needs only a path and query; the
// client adds the cluster address and the credentials.
func (c *Client) Perform(req *http.Request) (*http.Response, error) {
	return c.es.Perform(req)
}
