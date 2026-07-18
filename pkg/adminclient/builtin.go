package adminclient

import (
	"context"
	"net/http"
	"net/url"

	adminapi "github.com/agent-guide/agent-gateway/pkg/admin"
	builtinhost "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	builtinroute "github.com/agent-guide/agent-gateway/pkg/gateway/builtinroute"
)

type BuiltinRouteConfig = builtinroute.BuiltinRouteConfig
type BuiltinRouteView = adminapi.BuiltinRouteView
type BuiltinRuntimeView = builtinhost.RuntimeView

// GetBuiltinRuntime reads the ADK host runtime view: per-agent
// materialization state plus pending interactive tool permissions.
func (c *Client) GetBuiltinRuntime(ctx context.Context) (*BuiltinRuntimeView, error) {
	var resp BuiltinRuntimeView
	if err := c.do(ctx, http.MethodGet, "/admin/builtin/runtime", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListBuiltinRoutes(ctx context.Context) ([]BuiltinRouteView, error) {
	var resp itemsResponse[BuiltinRouteView]
	if err := c.do(ctx, http.MethodGet, "/admin/builtin/routes", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) CreateBuiltinRoute(ctx context.Context, cfg BuiltinRouteConfig) (*BuiltinRouteView, error) {
	var resp BuiltinRouteView
	if err := c.do(ctx, http.MethodPost, "/admin/builtin/routes", cfg, &resp, true, http.StatusCreated); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetBuiltinRoute(ctx context.Context, id string) (*BuiltinRouteView, error) {
	var resp BuiltinRouteView
	if err := c.do(ctx, http.MethodGet, "/admin/builtin/routes/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) UpdateBuiltinRoute(ctx context.Context, id string, cfg BuiltinRouteConfig) (*BuiltinRouteView, error) {
	var resp BuiltinRouteView
	if err := c.do(ctx, http.MethodPut, "/admin/builtin/routes/"+url.PathEscape(id), cfg, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DeleteBuiltinRoute(ctx context.Context, id string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.do(ctx, http.MethodDelete, "/admin/builtin/routes/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}
