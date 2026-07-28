package adminclient

import (
	"context"
	"net/http"
	"net/url"

	adminapi "github.com/agent-guide/agent-gateway/pkg/admin"
	mcproute "github.com/agent-guide/agent-gateway/pkg/gateway/mcproute"
	basemcp "github.com/agent-guide/agent-gateway/pkg/mcp"
	mcpruntime "github.com/agent-guide/agent-gateway/pkg/mcp/runtime"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
)

type MCPServiceConfig = mcpservice.MCPServiceConfig
type MCPServiceView = adminapi.MCPServiceView
type MCPRouteConfig = mcproute.MCPRouteConfig
type MCPRouteView = adminapi.MCPRouteView
type MCPRuntimeView = adminapi.MCPDispatcherRuntimeView
type MCPRuntimeInFlightRequest = mcpruntime.InFlightRequest
type MCPRuntimeProgressNotification = mcpruntime.ProgressNotification
type MCPRuntimeCompletedRequest = mcpruntime.CompletedRequest
type MCPServiceSessionView = adminapi.GatewaySessionView
type MCPTool = basemcp.Tool
type MCPToolCallRequest = adminapi.MCPToolCallRequest
type MCPToolResult = basemcp.ToolResult
type MCPResource = basemcp.Resource
type MCPResourceTemplate = basemcp.ResourceTemplate
type MCPResourceReadRequest = adminapi.MCPResourceReadRequest
type MCPResourceReadResult = basemcp.ResourceReadResult
type MCPPrompt = basemcp.Prompt
type MCPPromptGetRequest = adminapi.MCPPromptGetRequest
type MCPPromptResult = basemcp.PromptResult
type MCPServiceCapabilities = map[string]any

func (c *Client) ListMCPServices(ctx context.Context) ([]MCPServiceView, error) {
	var resp itemsResponse[MCPServiceView]
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/services", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) CreateMCPService(ctx context.Context, cfg MCPServiceConfig) (*MCPServiceView, error) {
	var resp MCPServiceView
	if err := c.do(ctx, http.MethodPost, "/admin/mcp/services", cfg, &resp, true, http.StatusCreated); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetMCPService(ctx context.Context, id string) (*MCPServiceView, error) {
	var resp MCPServiceView
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/services/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) UpdateMCPService(ctx context.Context, id string, cfg MCPServiceConfig) (*MCPServiceView, error) {
	var resp MCPServiceView
	if err := c.do(ctx, http.MethodPut, "/admin/mcp/services/"+url.PathEscape(id), cfg, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DeleteMCPService(ctx context.Context, id string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.do(ctx, http.MethodDelete, "/admin/mcp/services/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetMCPServiceSession(ctx context.Context, id string) (*MCPServiceSessionView, error) {
	var resp struct {
		Session *MCPServiceSessionView `json:"session"`
	}
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/services/"+url.PathEscape(id)+"/sessions", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Session, nil
}

func (c *Client) GetMCPServiceCapabilities(ctx context.Context, id string) (*MCPServiceCapabilities, error) {
	var resp MCPServiceCapabilities
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/services/"+url.PathEscape(id)+"/capabilities", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListMCPServiceTools(ctx context.Context, id string) ([]MCPTool, error) {
	var resp itemsResponse[MCPTool]
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/services/"+url.PathEscape(id)+"/tools", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) CallMCPServiceTool(ctx context.Context, id string, req MCPToolCallRequest) (*MCPToolResult, error) {
	var resp MCPToolResult
	if err := c.do(ctx, http.MethodPost, "/admin/mcp/services/"+url.PathEscape(id)+"/tools/call", req, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListMCPServiceResources(ctx context.Context, id string) ([]MCPResource, error) {
	var resp itemsResponse[MCPResource]
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/services/"+url.PathEscape(id)+"/resources", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) ListMCPServiceResourceTemplates(ctx context.Context, id string) ([]MCPResourceTemplate, error) {
	var resp itemsResponse[MCPResourceTemplate]
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/services/"+url.PathEscape(id)+"/resource-templates", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) ReadMCPServiceResource(ctx context.Context, id string, req MCPResourceReadRequest) (*MCPResourceReadResult, error) {
	var resp MCPResourceReadResult
	if err := c.do(ctx, http.MethodPost, "/admin/mcp/services/"+url.PathEscape(id)+"/resources/read", req, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListMCPServicePrompts(ctx context.Context, id string) ([]MCPPrompt, error) {
	var resp itemsResponse[MCPPrompt]
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/services/"+url.PathEscape(id)+"/prompts", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) GetMCPServicePrompt(ctx context.Context, id string, req MCPPromptGetRequest) (*MCPPromptResult, error) {
	var resp MCPPromptResult
	if err := c.do(ctx, http.MethodPost, "/admin/mcp/services/"+url.PathEscape(id)+"/prompts/get", req, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListMCPRoutes(ctx context.Context) ([]MCPRouteView, error) {
	var resp itemsResponse[MCPRouteView]
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/routes", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) GetMCPRoute(ctx context.Context, id string) (*MCPRouteView, error) {
	var resp MCPRouteView
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/routes/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) CreateMCPRoute(ctx context.Context, cfg MCPRouteConfig) (*MCPRouteView, error) {
	body, err := withoutManagedTimestamps(cfg)
	if err != nil {
		return nil, err
	}
	var resp MCPRouteView
	if err := c.do(ctx, http.MethodPost, "/admin/mcp/routes", body, &resp, true, http.StatusCreated); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) UpdateMCPRoute(ctx context.Context, id string, cfg MCPRouteConfig) (*MCPRouteView, error) {
	body, err := withoutManagedTimestamps(cfg)
	if err != nil {
		return nil, err
	}
	var resp MCPRouteView
	if err := c.do(ctx, http.MethodPut, "/admin/mcp/routes/"+url.PathEscape(id), body, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DeleteMCPRoute(ctx context.Context, id string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.do(ctx, http.MethodDelete, "/admin/mcp/routes/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetMCPRuntime(ctx context.Context) (*MCPRuntimeView, error) {
	var resp MCPRuntimeView
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/runtime", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListMCPRuntimeInFlight(ctx context.Context) ([]MCPRuntimeInFlightRequest, error) {
	var resp itemsResponse[MCPRuntimeInFlightRequest]
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/runtime/inflight", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) ListMCPRuntimeProgress(ctx context.Context) ([]MCPRuntimeProgressNotification, error) {
	var resp itemsResponse[MCPRuntimeProgressNotification]
	if err := c.do(ctx, http.MethodGet, "/admin/mcp/runtime/progress", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) ListMCPRuntimeHistory(ctx context.Context, routeID string) ([]MCPRuntimeCompletedRequest, error) {
	var resp itemsResponse[MCPRuntimeCompletedRequest]
	path := "/admin/mcp/runtime/history"
	if routeID != "" {
		path += "?route_id=" + url.QueryEscape(routeID)
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}
