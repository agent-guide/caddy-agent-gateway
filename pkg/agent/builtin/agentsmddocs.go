package builtin

import (
	"context"
	"fmt"
	"os"

	"github.com/cloudwego/eino/adk/filesystem"

	"github.com/agent-guide/agent-gateway/pkg/agent"
)

// agentsMDDocs serves a builtin definition's inline virtual documents as the
// read-only file backend behind the agentsmd middleware. A miss wraps
// os.ErrNotExist so a dangling @import degrades to a load warning instead of
// failing the turn. The map is built once at materialization and never
// mutated, so it is safe for concurrent reads.
type agentsMDDocs struct {
	docs map[string]string
}

func newAgentsMDDocs(docs []agent.BuiltinAgentsMDDoc) *agentsMDDocs {
	m := make(map[string]string, len(docs))
	for _, doc := range docs {
		m[doc.Path] = doc.Content
	}
	return &agentsMDDocs{docs: m}
}

// Read returns the whole document. The agentsmd loader always reads full
// files (offset 1, no limit), so line slicing is not implemented.
func (b *agentsMDDocs) Read(_ context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	content, ok := b.docs[req.FilePath]
	if !ok {
		return nil, fmt.Errorf("agentsmd doc %q: %w", req.FilePath, os.ErrNotExist)
	}
	return &filesystem.FileContent{Content: content}, nil
}
