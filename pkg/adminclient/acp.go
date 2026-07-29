package adminclient

import (
	"context"
	"net/http"
	"net/url"

	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
)

type ACPInFlightTurn = acpruntime.InFlightTurn
type ACPPooledInstanceInfo = acpruntime.PooledInstanceInfo
type ACPPendingPermissionInfo = acpruntime.PendingPermissionInfo

type ACPRuntimeView struct {
	InFlight           []ACPInFlightTurn          `json:"in_flight"`
	Instances          []ACPPooledInstanceInfo    `json:"instances"`
	PendingPermissions []ACPPendingPermissionInfo `json:"pending_permissions"`
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

type ACPCloseThreadResponse struct {
	Closed int `json:"closed"`
}
