package adminclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
)

const (
	defaultBaseURL = "http://localhost:8019"
)

type Config struct {
	BaseURL    string
	BasicAuth  string
	Token      string
	Headers    []string
	HTTPClient *http.Client
}

type Client struct {
	baseURL   string
	basicUser string
	basicPass string
	token     string
	headers   []string
	client    *http.Client
}

type Error struct {
	StatusCode int
	Message    string
	Body       []byte
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("admin API error %d: %s", e.StatusCode, e.Message)
	}
	if len(e.Body) != 0 {
		return fmt.Sprintf("admin API error %d: %s", e.StatusCode, strings.TrimSpace(string(e.Body)))
	}
	return fmt.Sprintf("admin API error %d", e.StatusCode)
}

func New(cfg Config) *Client {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	basicUser, basicPass := parseBasicAuth(cfg.BasicAuth)
	return &Client{
		baseURL:   baseURL,
		basicUser: basicUser,
		basicPass: basicPass,
		token:     strings.TrimSpace(cfg.Token),
		headers:   append([]string(nil), cfg.Headers...),
		client:    httpClient,
	}
}

func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

func parseBasicAuth(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	user, pass, ok := strings.Cut(raw, ":")
	if !ok {
		return strings.TrimSpace(raw), ""
	}
	return strings.TrimSpace(user), pass
}

type StatusResponse struct {
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
	Key    string `json:"key,omitempty"`
}

func (c *Client) do(ctx context.Context, method, path string, reqBody any, out any, auth bool, okStatuses ...int) error {
	if c == nil {
		return fmt.Errorf("admin client is nil")
	}
	fullURL := c.baseURL + path

	var bodyReader io.Reader
	if reqBody != nil {
		payload, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		if err := c.applyAuth(req); err != nil {
			return err
		}
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, fullURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if !statusAllowed(resp.StatusCode, okStatuses) {
		return &Error{
			StatusCode: resp.StatusCode,
			Message:    httpjson.ErrorMessage(raw),
			Body:       raw,
		}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response body: %w", err)
	}
	return nil
}

func withoutManagedTimestamps(value any) (map[string]any, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal config write request: %w", err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("decode config write request: %w", err)
	}
	delete(body, "created_at")
	delete(body, "updated_at")
	return body, nil
}

func (c *Client) applyAuth(req *http.Request) error {
	if c == nil {
		return fmt.Errorf("admin client is nil")
	}
	hasBasic := c.basicUser != "" || c.basicPass != ""
	if hasBasic && c.token != "" {
		// Both set the Authorization header; refuse rather than silently
		// letting one clobber the other.
		return fmt.Errorf("admin client cannot use both Basic Auth and a bearer token; configure only one")
	}
	switch {
	case hasBasic:
		req.SetBasicAuth(c.basicUser, c.basicPass)
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for _, raw := range c.headers {
		name, value, ok := strings.Cut(raw, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return fmt.Errorf("invalid admin header %q, want Name: value", raw)
		}
		req.Header.Set(name, strings.TrimSpace(value))
	}
	return nil
}

func statusAllowed(statusCode int, okStatuses []int) bool {
	for _, ok := range okStatuses {
		if statusCode == ok {
			return true
		}
	}
	return false
}
