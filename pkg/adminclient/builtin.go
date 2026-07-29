package adminclient

import (
	"context"
	"net/http"

	builtinhost "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
)

type BuiltinRuntimeView = builtinhost.RuntimeView
type BuiltinInFlightTurn = builtinhost.InFlightTurnView

func (c *Client) GetBuiltinRuntime(ctx context.Context) (*BuiltinRuntimeView, error) {
	var resp BuiltinRuntimeView
	if err := c.do(ctx, http.MethodGet, "/admin/builtin/runtime", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListBuiltinInFlight(ctx context.Context) ([]BuiltinInFlightTurn, error) {
	var resp itemsResponse[BuiltinInFlightTurn]
	if err := c.do(ctx, http.MethodGet, "/admin/builtin/runtime/inflight", nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}
