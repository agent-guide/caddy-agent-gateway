package adminclient

import (
	"context"
	"net/http"
	"net/url"

	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	adminapi "github.com/agent-guide/agent-gateway/pkg/admin"
	acproute "github.com/agent-guide/agent-gateway/pkg/gateway/acproute"
)

type ACPServiceConfig = acpservice.ServiceConfig
type ACPServiceView = adminapi.ACPServiceView
type ACPRouteConfig = acproute.ACPRouteConfig
type ACPRouteView = adminapi.ACPRouteView
type ACPInFlightTurn = acpruntime.InFlightTurn
type ACPPooledInstanceInfo = acpruntime.PooledInstanceInfo
type ACPPendingPermissionInfo = acpruntime.PendingPermissionInfo
type ACPPermissionDecision = acpruntime.PermissionDecision
type ACPListSessionsResponse = acpruntime.ListSessionsResponse
type ACPTranscriptResponse = acpruntime.TranscriptResponse

// ACPRuntimeView mirrors the GET /admin/acp/runtime response shape.
type ACPRuntimeView struct {
	InFlight           []ACPInFlightTurn          `json:"in_flight"`
	Instances          []ACPPooledInstanceInfo    `json:"instances"`
	PendingPermissions []ACPPendingPermissionInfo `json:"pending_permissions"`
}

// ACPSessionListOptions are the optional query parameters for listing
// agent-side sessions of one ACP service.
type ACPSessionListOptions struct {
	CWD    string
	Cursor string
}

func (c *Client) ListACPServices(ctx context.Context) ([]ACPServiceView, error) {
	var resp itemsResponse[ACPServiceView]
	if err := c.do(ctx, http.MethodGet, "/admin/acp/services", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) CreateACPService(ctx context.Context, cfg ACPServiceConfig) (*ACPServiceView, error) {
	var resp ACPServiceView
	if err := c.do(ctx, http.MethodPost, "/admin/acp/services", cfg, &resp, true, http.StatusCreated); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetACPService(ctx context.Context, id string) (*ACPServiceView, error) {
	var resp ACPServiceView
	if err := c.do(ctx, http.MethodGet, "/admin/acp/services/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) UpdateACPService(ctx context.Context, id string, cfg ACPServiceConfig) (*ACPServiceView, error) {
	var resp ACPServiceView
	if err := c.do(ctx, http.MethodPut, "/admin/acp/services/"+url.PathEscape(id), cfg, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DeleteACPService(ctx context.Context, id string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.do(ctx, http.MethodDelete, "/admin/acp/services/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListACPSessions(ctx context.Context, id string, opts ACPSessionListOptions) (*ACPListSessionsResponse, error) {
	query := url.Values{}
	if opts.CWD != "" {
		query.Set("cwd", opts.CWD)
	}
	if opts.Cursor != "" {
		query.Set("cursor", opts.Cursor)
	}
	path := "/admin/acp/services/" + url.PathEscape(id) + "/sessions"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var resp ACPListSessionsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetACPSessionTranscript(ctx context.Context, id, sessionID, cwd string) (*ACPTranscriptResponse, error) {
	path := "/admin/acp/services/" + url.PathEscape(id) + "/sessions/" + url.PathEscape(sessionID) + "/transcript"
	if cwd != "" {
		path += "?" + url.Values{"cwd": []string{cwd}}.Encode()
	}
	var resp ACPTranscriptResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListACPRoutes(ctx context.Context) ([]ACPRouteView, error) {
	var resp itemsResponse[ACPRouteView]
	if err := c.do(ctx, http.MethodGet, "/admin/acp/routes", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) CreateACPRoute(ctx context.Context, cfg ACPRouteConfig) (*ACPRouteView, error) {
	var resp ACPRouteView
	if err := c.do(ctx, http.MethodPost, "/admin/acp/routes", cfg, &resp, true, http.StatusCreated); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetACPRoute(ctx context.Context, id string) (*ACPRouteView, error) {
	var resp ACPRouteView
	if err := c.do(ctx, http.MethodGet, "/admin/acp/routes/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) UpdateACPRoute(ctx context.Context, id string, cfg ACPRouteConfig) (*ACPRouteView, error) {
	var resp ACPRouteView
	if err := c.do(ctx, http.MethodPut, "/admin/acp/routes/"+url.PathEscape(id), cfg, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DeleteACPRoute(ctx context.Context, id string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.do(ctx, http.MethodDelete, "/admin/acp/routes/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetACPRuntime(ctx context.Context) (*ACPRuntimeView, error) {
	var resp ACPRuntimeView
	if err := c.do(ctx, http.MethodGet, "/admin/acp/runtime", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListACPRuntimeInFlight(ctx context.Context) ([]ACPInFlightTurn, error) {
	var resp itemsResponse[ACPInFlightTurn]
	if err := c.do(ctx, http.MethodGet, "/admin/acp/runtime/inflight", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) CloseACPThread(ctx context.Context, agentID, threadID string) (*ACPCloseThreadResponse, error) {
	path := "/admin/acp/runtime/agents/" + url.PathEscape(agentID) + "/threads/" + url.PathEscape(threadID)
	var resp ACPCloseThreadResponse
	if err := c.do(ctx, http.MethodDelete, path, nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ACPCloseThreadResponse mirrors the DELETE /admin/acp/runtime/threads/...
// response shape. Closed is the number of pooled scopes torn down.
type ACPCloseThreadResponse struct {
	Closed int `json:"closed"`
}

func (c *Client) ResolveACPPermission(ctx context.Context, decision ACPPermissionDecision) (*StatusResponse, error) {
	path := "/admin/acp/runtime/permissions/" + url.PathEscape(decision.RequestID)
	var resp StatusResponse
	if err := c.do(ctx, http.MethodPost, path, decision, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}
