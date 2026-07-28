package adminclient

import (
	"context"
	"net/http"
	"net/url"

	adminapi "github.com/agent-guide/agent-gateway/pkg/admin"
	"github.com/agent-guide/agent-gateway/pkg/gateway/agentroute"
)

type AgentRouteConfig = agentroute.AgentRouteConfig
type AgentRouteView = adminapi.AgentRouteView

func (c *Client) ListAgentRoutes(ctx context.Context) ([]AgentRouteView, error) {
	var resp itemsResponse[AgentRouteView]
	if err := c.do(ctx, http.MethodGet, "/admin/agents/routes", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}
func (c *Client) CreateAgentRoute(ctx context.Context, cfg AgentRouteConfig) (*AgentRouteView, error) {
	body, err := withoutManagedTimestamps(cfg)
	if err != nil {
		return nil, err
	}
	var resp AgentRouteView
	if err := c.do(ctx, http.MethodPost, "/admin/agents/routes", body, &resp, true, http.StatusCreated); err != nil {
		return nil, err
	}
	return &resp, nil
}
func (c *Client) GetAgentRoute(ctx context.Context, id string) (*AgentRouteView, error) {
	var resp AgentRouteView
	if err := c.do(ctx, http.MethodGet, "/admin/agents/routes/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}
func (c *Client) UpdateAgentRoute(ctx context.Context, id string, cfg AgentRouteConfig) (*AgentRouteView, error) {
	body, err := withoutManagedTimestamps(cfg)
	if err != nil {
		return nil, err
	}
	var resp AgentRouteView
	if err := c.do(ctx, http.MethodPut, "/admin/agents/routes/"+url.PathEscape(id), body, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}
func (c *Client) DeleteAgentRoute(ctx context.Context, id string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.do(ctx, http.MethodDelete, "/admin/agents/routes/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}
