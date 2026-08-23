package anthropicmsg

import (
	"github.com/cloudwego/eino/schema"
)

// responseItem is the ordered semantic unit shared by response transports.
// Native relay retains its raw payload as authority while normalized batch
// materializes this sequence and streaming reduces equivalent items into SSE.
type responseItem struct {
	Block          ContentBlockResponse
	SourceToolCall *schema.ToolCall
}

func responseItemsFromMessage(msg *schema.Message) []responseItem {
	blocks := contentBlocksFromMessage(msg)
	items := make([]responseItem, 0, len(blocks))
	toolIndex := 0
	for _, block := range blocks {
		item := responseItem{Block: block}
		if block.Type == "tool_use" && msg != nil && toolIndex < len(msg.ToolCalls) {
			call := msg.ToolCalls[toolIndex]
			item.SourceToolCall = &call
			toolIndex++
		}
		items = append(items, item)
	}
	return items
}

func reduceResponseItems(items []responseItem) []ContentBlockResponse {
	blocks := make([]ContentBlockResponse, 0, len(items))
	for _, item := range items {
		blocks = append(blocks, item.Block)
	}
	return blocks
}
