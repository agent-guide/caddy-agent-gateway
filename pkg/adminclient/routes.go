package adminclient

import (
	"context"
	"net/http"
	"net/url"
)

func (c *Client) ListLLMRoutes(ctx context.Context, opts LLMRouteListOptions) ([]LLMRoute, error) {
	query := url.Values{}
	if opts.Tag != "" {
		query.Set("tag", opts.Tag)
	}
	if opts.TagPrefix != "" {
		query.Set("tag_prefix", opts.TagPrefix)
	}
	var resp itemsResponse[LLMRoute]
	if err := c.do(ctx, http.MethodGet, withQuery("/admin/llm/routes", query), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) GetLLMRoute(ctx context.Context, id string) (*LLMRoute, error) {
	var resp LLMRoute
	if err := c.do(ctx, http.MethodGet, "/admin/llm/routes/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) CreateLLMRoute(ctx context.Context, route LLMRouteConfig) (*LLMRouteConfig, error) {
	body, err := withoutManagedTimestamps(route)
	if err != nil {
		return nil, err
	}
	var resp LLMRouteConfig
	if err := c.do(ctx, http.MethodPost, "/admin/llm/routes", body, &resp, true, http.StatusCreated); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) UpdateLLMRoute(ctx context.Context, id string, route LLMRouteConfig) (*LLMRouteConfig, error) {
	body, err := withoutManagedTimestamps(route)
	if err != nil {
		return nil, err
	}
	var resp LLMRouteConfig
	if err := c.do(ctx, http.MethodPut, "/admin/llm/routes/"+url.PathEscape(id), body, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DeleteLLMRoute(ctx context.Context, id string) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.do(ctx, http.MethodDelete, "/admin/llm/routes/"+url.PathEscape(id), nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) EnableLLMRoute(ctx context.Context, id string) (*LLMRoute, error) {
	return c.setLLMRouteEnabled(ctx, id, true)
}

func (c *Client) DisableLLMRoute(ctx context.Context, id string) (*LLMRoute, error) {
	return c.setLLMRouteEnabled(ctx, id, false)
}

func (c *Client) setLLMRouteEnabled(ctx context.Context, id string, enabled bool) (*LLMRoute, error) {
	action := "disable"
	if enabled {
		action = "enable"
	}
	var resp LLMRoute
	if err := c.do(ctx, http.MethodPost, "/admin/llm/routes/"+url.PathEscape(id)+"/"+action, nil, &resp, true, http.StatusOK); err != nil {
		return nil, err
	}
	return &resp, nil
}
