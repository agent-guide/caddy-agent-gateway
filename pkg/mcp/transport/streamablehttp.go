package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// StreamableHTTPTransport implements a minimal MCP transport over HTTP.
type StreamableHTTPTransport struct {
	url       string
	client    *http.Client
	headers   http.Header
	sessionID string
	out       chan *Message
	closeOnce sync.Once
}

func NewStreamableHTTPTransport(url string, client *http.Client) *StreamableHTTPTransport {
	if client == nil {
		client = http.DefaultClient
	}
	return &StreamableHTTPTransport{
		url:     url,
		client:  client,
		headers: make(http.Header),
		out:     make(chan *Message, 16),
	}
}

func (t *StreamableHTTPTransport) SetHeader(key, value string) {
	if t == nil || key == "" || value == "" {
		return
	}
	t.headers.Set(key, value)
}

func (t *StreamableHTTPTransport) Connect(ctx context.Context) error {
	_ = ctx
	return nil
}

func (t *StreamableHTTPTransport) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		close(t.out)
	})
	return nil
}

func (t *StreamableHTTPTransport) Send(ctx context.Context, msg *Message) error {
	reply, err := t.Do(ctx, msg)
	if err != nil {
		return err
	}
	if reply != nil {
		t.out <- reply
	}
	return nil
}

func (t *StreamableHTTPTransport) Do(ctx context.Context, msg *Message) (*Message, error) {
	if t == nil {
		return nil, fmt.Errorf("streamable_http: transport is nil")
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("streamable_http: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("streamable_http: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for key, values := range t.headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if t.sessionID != "" {
		req.Header.Set("MCP-Session-Id", t.sessionID)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("streamable_http: send request: %w", err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("MCP-Session-Id"); sid != "" {
		t.sessionID = sid
	}
	if resp.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("streamable_http: unexpected status %d: %s", resp.StatusCode, string(body))
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if contentType == "text/event-stream" {
		reply, err := decodeSSEResponse(resp.Body)
		if err != nil {
			return nil, err
		}
		return reply, nil
	}

	var reply Message
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		if errors.Is(err, io.EOF) && msg.ID == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("streamable_http: decode response: %w", err)
	}
	return &reply, nil
}

// Call implements Caller by delegating to Do.
func (t *StreamableHTTPTransport) Call(ctx context.Context, msg *Message) (*Message, error) {
	return t.Do(ctx, msg)
}

func (t *StreamableHTTPTransport) Receive() <-chan *Message {
	if t == nil {
		return nil
	}
	return t.out
}

func (t *StreamableHTTPTransport) SessionID() string {
	if t == nil {
		return ""
	}
	return t.sessionID
}

func decodeSSEResponse(body io.Reader) (*Message, error) {
	scanner := bufio.NewScanner(body)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		if line != "" || data.Len() == 0 {
			continue
		}
		payload := data.String()
		data.Reset()
		if payload == "" {
			continue
		}
		var reply Message
		if err := json.Unmarshal([]byte(payload), &reply); err == nil && (reply.JSONRPC != "" || reply.ID != nil || reply.Result != nil || reply.Error != nil) {
			return &reply, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("streamable_http: read sse response: %w", err)
	}
	return nil, fmt.Errorf("streamable_http: sse response ended without json-rpc message")
}
