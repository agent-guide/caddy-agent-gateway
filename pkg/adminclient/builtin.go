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
type BuiltinInFlightTurn = builtinhost.InFlightTurnView

// BuiltinCancelResult is the response of a builtin turn cancel request.
type BuiltinCancelResult struct {
	Cancelled bool   `json:"cancelled"`
	Mode      string `json:"mode"`
}

// GetBuiltinRuntime reads the ADK host runtime view: per-agent
// materialization state, pending interactive tool permissions, and in-flight
// turns.
func (c *Client) GetBuiltinRuntime(ctx context.Context) (*BuiltinRuntimeView, error) {
	var resp BuiltinRuntimeView
	if err := c.do(ctx, http.MethodGet, "/admin/builtin/runtime", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListBuiltinInFlight lists the running builtin turns an operator can cancel.
func (c *Client) ListBuiltinInFlight(ctx context.Context) ([]BuiltinInFlightTurn, error) {
	var resp itemsResponse[BuiltinInFlightTurn]
	if err := c.do(ctx, http.MethodGet, "/admin/builtin/runtime/inflight", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// CancelBuiltinTurn cancels the running turn for one (agentID, sessionID).
// mode is "force" (default when empty) or "graceful".
func (c *Client) CancelBuiltinTurn(ctx context.Context, agentID, sessionID, mode string) (*BuiltinCancelResult, error) {
	path := "/admin/builtin/runtime/turns/" + url.PathEscape(agentID) + "/" + url.PathEscape(sessionID)
	if mode != "" {
		path += "?mode=" + url.QueryEscape(mode)
	}
	var resp BuiltinCancelResult
	if err := c.do(ctx, http.MethodDelete, path, nil, &resp, true, http.StatusOK); err != nil {
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
