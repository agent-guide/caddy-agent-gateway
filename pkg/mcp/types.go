package mcp

import "context"

// TransportType defines the MCP transport type.
type TransportType string

const (
	TransportStdio          TransportType = "stdio"
	TransportSSE            TransportType = "sse"
	TransportStreamableHTTP TransportType = "streamable_http"
)

// ClientStatus represents the connection status of an MCP client.
type ClientStatus string

const (
	ClientStatusInactive   ClientStatus = "inactive"
	ClientStatusConnecting ClientStatus = "connecting"
	ClientStatusConnected  ClientStatus = "connected"
	ClientStatusError      ClientStatus = "error"
)

// Tool represents a tool exposed by an MCP server.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

type ToolListResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// ToolResult is the result of calling an MCP tool.
type ToolResult struct {
	Content           any            `json:"content,omitempty"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError"`
	Meta              map[string]any `json:"_meta,omitempty"`
}

// Resource represents a resource exposed by an MCP server.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type ResourceListResult struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

type ResourceTemplate struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	URITemplate string `json:"uriTemplate"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type ResourceTemplateListResult struct {
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
	NextCursor        string             `json:"nextCursor,omitempty"`
}

// ResourceContent is the content of a resource.
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	Blob     []byte `json:"blob,omitempty"`
}

type ResourceReadResult struct {
	Contents []ResourceContent `json:"contents"`
}

// Prompt represents a prompt template exposed by an MCP server.
type Prompt struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type PromptListResult struct {
	Prompts    []Prompt `json:"prompts"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// PromptResult is the result of getting a prompt.
type PromptResult struct {
	Description string `json:"description,omitempty"`
	Messages    []any  `json:"messages"`
}

type CompletionReference struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	URI  string `json:"uri,omitempty"`
}

type CompletionArgument struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Completion struct {
	Values  []string `json:"values"`
	Total   *int     `json:"total,omitempty"`
	HasMore bool     `json:"hasMore,omitempty"`
}

type CompletionResult struct {
	Completion Completion     `json:"completion"`
	Meta       map[string]any `json:"_meta,omitempty"`
}

// Client represents a connection to an MCP server.
type Client interface {
	ID() string
	Name() string
	Status() ClientStatus
	Connect(ctx context.Context) error
	Disconnect(ctx context.Context) error
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (*ToolResult, error)
	ListResources(ctx context.Context) ([]Resource, error)
	ReadResource(ctx context.Context, uri string) (*ResourceReadResult, error)
	ListPrompts(ctx context.Context) ([]Prompt, error)
	GetPrompt(ctx context.Context, name string, args map[string]any) (*PromptResult, error)
	ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, error)
	Complete(ctx context.Context, ref CompletionReference, argument CompletionArgument, args map[string]string) (*CompletionResult, error)
}
