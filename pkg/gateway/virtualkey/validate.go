package virtualkey

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

func (key VirtualKey) Validate() error {
	if key.Disabled {
		return fmt.Errorf("virtual key is disabled")
	}
	if !key.ExpiresAt.IsZero() && key.ExpiresAt.Before(time.Now()) {
		return fmt.Errorf("virtual key is expired")
	}

	return nil
}

// ValidateConfiguration validates policy fields independently of whether the
// key is currently enabled or expired.
func (key VirtualKey) ValidateConfiguration() error {
	if key.RateLimits == nil {
		return nil
	}
	for name, limit := range map[string]*RateLimit{
		"llm":   key.RateLimits.LLM,
		"mcp":   key.RateLimits.MCP,
		"agent": key.RateLimits.Agent,
	} {
		if limit == nil {
			continue
		}
		if limit.RequestsPerMinute <= 0 {
			return fmt.Errorf("rate_limits.%s.requests_per_minute must be greater than zero", name)
		}
		if limit.Burst <= 0 {
			return fmt.Errorf("rate_limits.%s.burst must be greater than zero", name)
		}
	}
	return nil
}

func (key VirtualKey) ValidateForRoute(routeID string) error {
	err := key.Validate()
	if err != nil {
		return err
	}
	if len(key.AllowedRouteIDs) > 0 && !slices.Contains(key.AllowedRouteIDs, routeID) {
		return fmt.Errorf("virtual key is not allowed to access this route")
	}
	return nil
}

// func ValidateForRoute(routeID string, requireVirtualKey bool, key *VirtualKey) (*VirtualKey, error) {
// 	if key == nil {
// 		if requireVirtualKey {
// 			return nil, statuserr.New(http.StatusUnauthorized, "virtual key is required")
// 		}
// 		return nil, nil
// 	}
// 	if key.Disabled {
// 		return nil, statuserr.New(http.StatusForbidden, "virtual key is disabled")
// 	}
// 	if !key.ExpiresAt.IsZero() && key.ExpiresAt.Before(time.Now()) {
// 		return nil, statuserr.New(http.StatusForbidden, "virtual key is expired")
// 	}
// 	if len(key.AllowedRouteIDs) > 0 && !slices.Contains(key.AllowedRouteIDs, routeID) {
// 		return nil, statuserr.New(http.StatusForbidden, "virtual key is not allowed to access this route")
// 	}
// 	return key, nil
// }

// ExtractAPIKeys returns every candidate virtual-key value carried by the
// request, in precedence order. A request may legitimately present a key in
// both the `x-api-key` header and the `Authorization: Bearer` header (Claude
// Code, for example, always sends both — the gateway virtual key in one and an
// unrelated upstream key in the other), so callers must try each candidate
// rather than trusting a single header. Empty and duplicate values are omitted.
func ExtractAPIKeys(r *http.Request) []string {
	if r == nil {
		return nil
	}
	var keys []string
	add := func(k string) {
		k = strings.TrimSpace(k)
		if k == "" || slices.Contains(keys, k) {
			return
		}
		keys = append(keys, k)
	}
	add(r.Header.Get("x-api-key"))
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		add(auth[7:])
	}
	return keys
}
