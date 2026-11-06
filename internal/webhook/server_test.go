package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/webhook"
)

type fakeProvider struct {
	records []*webhook.Endpoint
	applied *webhook.Changes
	err     error
}

func (f *fakeProvider) Records(context.Context) ([]*webhook.Endpoint, error) { return f.records, f.err }
func (f *fakeProvider) ApplyChanges(_ context.Context, c *webhook.Changes) error {
	f.applied = c
	return f.err
}
func (f *fakeProvider) AdjustEndpoints(eps []*webhook.Endpoint) ([]*webhook.Endpoint, error) {
	return eps, f.err
}
func (f *fakeProvider) DomainFilter() webhook.DomainFilter {
	return webhook.DomainFilter{Include: []string{"example.com"}}
}

func newServer(f *fakeProvider) *httptest.Server {
	return httptest.NewServer(webhook.Handler(f, slog.New(slog.NewTextHandler(io.Discard, nil)), 0))
}

func do(t *testing.T, method, url, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", webhook.MediaType)
	req.Header.Set("Content-Type", webhook.MediaType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestNegotiate(t *testing.T) {
	s := newServer(&fakeProvider{})
	defer s.Close()
	resp, body := do(t, http.MethodGet, s.URL+"/", "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != webhook.MediaType {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if strings.TrimSpace(body) != `{"include":["example.com"]}` {
		t.Fatalf("unexpected body %s", body)
	}
}

func TestRecordsGetAndPost(t *testing.T) {
	f := &fakeProvider{records: []*webhook.Endpoint{{DNSName: "www.example.com", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.1"}}}}
	s := newServer(f)
	defer s.Close()

	resp, body := do(t, http.MethodGet, s.URL+"/records", "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != webhook.MediaType {
		t.Fatalf("GET /records: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if strings.TrimSpace(body) != `[{"dnsName":"www.example.com","targets":["192.0.2.1"],"recordType":"A","recordTTL":300}]` {
		t.Fatalf("unexpected body %s", body)
	}

	changes := `{"create":[{"dnsName":"a.example.com","targets":["192.0.2.2"],"recordType":"A","labels":{"owner":"x"}}],"delete":[{"dnsName":"b.example.com","targets":["192.0.2.3"],"recordType":"A"}]}`
	resp, _ = do(t, http.MethodPost, s.URL+"/records", changes)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /records: %d", resp.StatusCode)
	}
	if f.applied == nil || len(f.applied.Create) != 1 || f.applied.Create[0].Labels["owner"] != "x" || len(f.applied.Delete) != 1 {
		t.Fatalf("changes not decoded: %+v", f.applied)
	}
}

func TestEmptyRecordsIsArray(t *testing.T) {
	s := newServer(&fakeProvider{})
	defer s.Close()
	_, body := do(t, http.MethodGet, s.URL+"/records", "")
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("want [], got %s", body)
	}
}

func TestErrors(t *testing.T) {
	f := &fakeProvider{err: errors.New("boom")}
	s := newServer(f)
	defer s.Close()

	if resp, _ := do(t, http.MethodGet, s.URL+"/records", ""); resp.StatusCode != 500 {
		t.Errorf("provider error must be 5xx so external-dns retries, got %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodPost, s.URL+"/records", "{not json"); resp.StatusCode != 400 {
		t.Errorf("bad JSON must be 400, got %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodPut, s.URL+"/records", "{}"); resp.StatusCode != 405 {
		t.Errorf("PUT must be 405, got %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, s.URL+"/adjustendpoints", ""); resp.StatusCode != 405 {
		t.Errorf("GET /adjustendpoints must be 405, got %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, s.URL+"/nope", ""); resp.StatusCode != 404 {
		t.Errorf("unknown path must be 404, got %d", resp.StatusCode)
	}
	big := `[{"dnsName":"` + strings.Repeat("a", webhook.DefaultMaxBodySize) + `"}]`
	if resp, _ := do(t, http.MethodPost, s.URL+"/adjustendpoints", big); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body must be 413, got %d", resp.StatusCode)
	}
}

func TestAdjustEndpoints(t *testing.T) {
	s := newServer(&fakeProvider{})
	defer s.Close()
	resp, body := do(t, http.MethodPost, s.URL+"/adjustendpoints", `[{"dnsName":"a.example.com","recordType":"A","targets":["192.0.2.1"]}]`)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != webhook.MediaType {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var eps []webhook.Endpoint
	if err := json.Unmarshal([]byte(body), &eps); err != nil || len(eps) != 1 {
		t.Fatalf("bad body %s: %v", body, err)
	}
}

func TestHealth(t *testing.T) {
	s := httptest.NewServer(webhook.HealthHandler())
	defer s.Close()
	if resp, body := do(t, http.MethodGet, s.URL+"/healthz", ""); resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("healthz: %d %s", resp.StatusCode, body)
	}
}
