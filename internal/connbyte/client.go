// Package connbyte is a small client for the ConnByte DNS API (https://api.connbyte.com/).
//
// Only the endpoints needed by the external-dns webhook are implemented:
// /ns/zone/list, /ns/record/list, /ns/record/create and /ns/record/delete.
// All endpoints take a JSON body via POST and are authenticated with a bearer token.
package connbyte

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the public ConnByte API endpoint.
const DefaultBaseURL = "https://api.connbyte.com"

const maxResponseBytes = 32 << 20

// Client talks to the ConnByte DNS API.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	userAgent  string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client used for API calls.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// New returns a client for the given base URL (empty means DefaultBaseURL) and API token.
func New(baseURL, token string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("connbyte: API token is empty")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: &http.Client{Timeout: 20 * time.Second},
		userAgent:  "external-dns-connbyte-webhook",
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Zone is a DNS zone of the account.
type Zone struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Title     string `json:"title"`
	Zone      string `json:"zone"`
}

// Record is a DNS record as returned by /ns/record/list.
type Record struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Content  string   `json:"content"`
	TTL      int      `json:"ttl"`
	Priority int      `json:"priority"`
	Disabled flexBool `json:"disabled"`
}

// IsDisabled reports whether the record is disabled.
func (r Record) IsDisabled() bool { return bool(r.Disabled) }

// RecordInput is the body of /ns/record/create.
// Name is relative to the zone; an empty name is the zone apex.
type RecordInput struct {
	ZoneID   string `json:"zoneId"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	TTL      int    `json:"ttl"`
	Priority int    `json:"priority"`
	Disabled bool   `json:"disabled"`
}

// Status is the status block of every API response.
type Status struct {
	Code    int    `json:"code"`
	Title   string `json:"title"`
	Message string `json:"message"`
	ID      string `json:"id"`
	LogID   string `json:"logId"`
	Path    string `json:"path"`
}

type response[T any] struct {
	Status Status              `json:"status"`
	Data   T                   `json:"data"`
	Field  []map[string]string `json:"field"`
}

// APIError is returned when the API answers with an error status.
type APIError struct {
	HTTPStatus int
	Status     Status
	Field      []map[string]string
}

func (e *APIError) Error() string {
	var b strings.Builder
	code := e.Status.Code
	if code == 0 {
		code = e.HTTPStatus
	}
	fmt.Fprintf(&b, "connbyte: %s: %d %s", e.Status.Path, code, e.Status.Title)
	if e.Status.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Status.Message)
	}
	for _, f := range e.Field {
		for k, v := range f {
			fmt.Fprintf(&b, " [%s: %s]", k, v)
		}
	}
	if e.Status.LogID != "" {
		fmt.Fprintf(&b, " (logId %s)", e.Status.LogID)
	}
	return b.String()
}

// Temporary reports whether retrying the request later may succeed.
func (e *APIError) Temporary() bool {
	return e.HTTPStatus >= 500 || e.HTTPStatus == http.StatusTooManyRequests
}

// ListZones returns all zones of the account.
func (c *Client) ListZones(ctx context.Context) ([]Zone, error) {
	var items []struct {
		Zone Zone `json:"zone"`
	}
	if err := c.call(ctx, "/ns/zone/list", struct{}{}, &items); err != nil {
		return nil, err
	}
	out := make([]Zone, 0, len(items))
	for _, it := range items {
		out = append(out, it.Zone)
	}
	return out, nil
}

// ListRecords returns all records of a zone.
func (c *Client) ListRecords(ctx context.Context, zoneID string) ([]Record, error) {
	var items []struct {
		Record Record `json:"record"`
	}
	if err := c.call(ctx, "/ns/record/list", map[string]string{"zoneId": zoneID}, &items); err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(items))
	for _, it := range items {
		out = append(out, it.Record)
	}
	return out, nil
}

// CreateRecord creates a record and returns its ID.
func (c *Client) CreateRecord(ctx context.Context, in RecordInput) (int64, error) {
	var res struct {
		ID     int64  `json:"id"`
		Record string `json:"record"`
	}
	if err := c.call(ctx, "/ns/record/create", in, &res); err != nil {
		return 0, err
	}
	return res.ID, nil
}

// DeleteRecord deletes a record of a zone.
func (c *Client) DeleteRecord(ctx context.Context, zoneID string, recordID int64) error {
	body := struct {
		ZoneID   string `json:"zoneId"`
		RecordID int64  `json:"recordId"`
	}{zoneID, recordID}
	return c.call(ctx, "/ns/record/delete", body, nil)
}

func (c *Client) call(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("connbyte: %s: encode request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("connbyte: %s: create request: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connbyte: %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("connbyte: %s: read response: %w", path, err)
	}

	var res response[json.RawMessage]
	if err := json.Unmarshal(raw, &res); err != nil {
		if resp.StatusCode/100 != 2 {
			return &APIError{HTTPStatus: resp.StatusCode, Status: Status{Path: path, Title: http.StatusText(resp.StatusCode)}}
		}
		return fmt.Errorf("connbyte: %s: decode response: %w", path, err)
	}
	if resp.StatusCode/100 != 2 || (res.Status.Code != 0 && res.Status.Code/100 != 2) {
		if res.Status.Path == "" {
			res.Status.Path = path
		}
		return &APIError{HTTPStatus: resp.StatusCode, Status: res.Status, Field: res.Field}
	}
	if out == nil || len(res.Data) == 0 || string(res.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(res.Data, out); err != nil {
		return fmt.Errorf("connbyte: %s: decode data: %w", path, err)
	}
	return nil
}

// flexBool accepts JSON booleans and numbers (the API returns 0/1 for "disabled").
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	switch strings.TrimSpace(string(data)) {
	case "true", "1", `"1"`, `"true"`:
		*b = true
	case "false", "0", "null", `""`, `"0"`, `"false"`:
		*b = false
	default:
		return fmt.Errorf("connbyte: invalid boolean %s", data)
	}
	return nil
}
