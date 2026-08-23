package anthropicmsg

import "github.com/agent-guide/agent-gateway/pkg/llm/provider"

func deriveAnthropicRequirements(req *MessagesRequest) provider.ProtocolRequirementSet {
	reasons := map[provider.ProtocolFeature][]provider.RequirementReason{}
	add := func(feature provider.ProtocolFeature, reason provider.RequirementReason) {
		reasons[feature] = append(reasons[feature], reason)
	}
	for _, tool := range req.Tools {
		if !tool.isServerTool() {
			continue
		}
		add(provider.FeatureAnthropicServerToolRequest, provider.ReasonAnthropicServerTool)
		add(provider.FeatureAnthropicNativeResponse, provider.ReasonAnthropicServerTool)
		add(provider.FeatureAnthropicNativeHistoryReplay, provider.ReasonAnthropicServerTool)
	}
	for _, message := range req.Messages {
		if len(nativeContentBlocks(message.Content)) > 0 {
			add(provider.FeatureAnthropicNativeResponse, provider.ReasonAnthropicOpaqueContent)
			add(provider.FeatureAnthropicNativeHistoryReplay, provider.ReasonAnthropicNativeHistory)
		}
		for _, block := range message.Content {
			switch block.Type {
			case "thinking":
				if block.Signature != "" && !provider.IsGatewayThinkingSignature(block.Signature) {
					add(provider.FeatureAnthropicReasoningReplay, provider.ReasonAnthropicSignedReasoning)
				}
			case "redacted_thinking":
				if block.Data != "" {
					add(provider.FeatureAnthropicReasoningReplay, provider.ReasonAnthropicSignedReasoning)
				}
			}
		}
	}
	set, err := provider.NewProtocolRequirementSet(reasons)
	if err != nil {
		panic(err)
	}
	return set
}
