package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/credential"
)

type credentialContextKey struct{}

func WithCredential(ctx context.Context, cred *credential.Credential) context.Context {
	if cred == nil {
		return ctx
	}
	return context.WithValue(ctx, credentialContextKey{}, cred)
}

func CredentialFromContext(ctx context.Context) (*credential.Credential, bool) {
	cred, ok := ctx.Value(credentialContextKey{}).(*credential.Credential)
	if !ok || cred == nil {
		return nil, false
	}
	return cred, true
}

func APIKeyFromContextOrConfig(ctx context.Context, configAPIKey string) string {
	if cred, ok := CredentialFromContext(ctx); ok && strings.TrimSpace(cred.APIKey()) != "" {
		return strings.TrimSpace(cred.APIKey())
	}
	return strings.TrimSpace(configAPIKey)
}

// CheckResponse returns a StatusError if the HTTP response status is not 2xx.
// It reads up to 4 KB of the body to include in the error message.
func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &UpstreamError{
		Status:     resp.StatusCode,
		StatusText: resp.Status,
		Body:       string(body),
	}
}

// RetryProviderCall retries fn up to NetworkConfig.MaxRetries times on retryable
// errors (429, 5xx). Non-retryable 4xx errors are returned immediately.
// Do NOT use this for streaming; retry semantics are undefined once a stream starts.
func RetryProviderCall[T any](config NetworkConfig, fn func() (T, error)) (T, error) {
	maxRetries := config.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	delay := time.Duration(config.RetryDelaySeconds) * time.Second
	if delay <= 0 {
		delay = time.Second
	}
	var zero T
	var last error
	for i := 0; i <= maxRetries; i++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		last = statuserr.Wrap(err, http.StatusBadGateway)
		if !isRetryable(last) {
			return zero, last
		}
		if i < maxRetries {
			time.Sleep(delay * time.Duration(i+1))
		}
	}
	return zero, last
}

func isRetryable(err error) bool {
	var se statuserr.StatusError
	if errors.As(err, &se) {
		code := se.StatusCode()
		return code == http.StatusTooManyRequests || (code >= 500 && code < 600)
	}
	return true // network errors without status code are retryable
}
