package usage

func Bool(v bool) *bool { return &v }
func Int(v int) *int    { return &v }

func mergeLLM(dst *LLMExtension, src LLMExtension) {
	if src.LLMAPI != "" {
		dst.LLMAPI = src.LLMAPI
	}
	if src.APIOperation != "" {
		dst.APIOperation = src.APIOperation
	}
	if src.ProviderID != "" {
		dst.ProviderID = src.ProviderID
	}
	if src.ProviderType != "" {
		dst.ProviderType = src.ProviderType
	}
	if src.LogicalModel != "" {
		dst.LogicalModel = src.LogicalModel
	}
	if src.UpstreamModel != "" {
		dst.UpstreamModel = src.UpstreamModel
	}
	if src.CredentialSource != "" {
		dst.CredentialSource = src.CredentialSource
	}
	if src.CredentialID != "" {
		dst.CredentialID = src.CredentialID
	}
	if src.Stream != nil {
		dst.Stream = src.Stream
	}
	if src.InputTokens != nil {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens != nil {
		dst.OutputTokens = src.OutputTokens
	}
	if src.TotalTokens != nil {
		dst.TotalTokens = src.TotalTokens
	}
	if src.UsageFinalized != nil {
		dst.UsageFinalized = src.UsageFinalized
	}
	if src.RequestToolCount != nil {
		dst.RequestToolCount = src.RequestToolCount
	}
	if len(src.RequestToolNames) > 0 {
		dst.RequestToolNames = append([]string(nil), src.RequestToolNames...)
	}
	if src.ToolCallCount != nil {
		dst.ToolCallCount = src.ToolCallCount
	}
	if len(src.ToolNames) > 0 {
		dst.ToolNames = append([]string(nil), src.ToolNames...)
	}
}

func mergeMCP(dst *MCPExtension, src MCPExtension) {
	if src.RequestID != "" {
		dst.RequestID = src.RequestID
	}
	if src.ServiceID != "" {
		dst.ServiceID = src.ServiceID
	}
	if src.Method != "" {
		dst.Method = src.Method
	}
	if src.ToolName != "" {
		dst.ToolName = src.ToolName
	}
	if src.PresentedToolName != "" {
		dst.PresentedToolName = src.PresentedToolName
	}
	if src.ExecutedToolName != "" {
		dst.ExecutedToolName = src.ExecutedToolName
	}
	if src.ExecutionMode != "" {
		dst.ExecutionMode = src.ExecutionMode
	}
	if src.PolicyAction != "" {
		dst.PolicyAction = src.PolicyAction
	}
	if src.ResourceURI != "" {
		dst.ResourceURI = src.ResourceURI
	}
	if src.PromptName != "" {
		dst.PromptName = src.PromptName
	}
	if src.CompletionRefType != "" {
		dst.CompletionRefType = src.CompletionRefType
	}
	if src.CompletionArgument != "" {
		dst.CompletionArgument = src.CompletionArgument
	}
	if src.ArgCount != nil {
		dst.ArgCount = src.ArgCount
	}
	if src.ResultStatus != "" {
		dst.ResultStatus = src.ResultStatus
	}
	if src.Cancelled != nil {
		dst.Cancelled = src.Cancelled
	}
	if src.ToolArgsJSON != "" {
		dst.ToolArgsJSON = src.ToolArgsJSON
	}
}

func mergeACP(dst *ACPExtension, src ACPExtension) {
	if src.ServiceID != "" {
		dst.ServiceID = src.ServiceID
	}
	if src.AgentType != "" {
		dst.AgentType = src.AgentType
	}
	if src.Operation != "" {
		dst.Operation = src.Operation
	}
	if src.ThreadID != "" {
		dst.ThreadID = src.ThreadID
	}
	if src.SessionID != "" {
		dst.SessionID = src.SessionID
	}
	if src.PermissionRequestID != "" {
		dst.PermissionRequestID = src.PermissionRequestID
	}
	if src.FreshSession != nil {
		dst.FreshSession = src.FreshSession
	}
	if len(src.EventCounts) > 0 {
		dst.EventCounts = src.EventCounts
	}
	if src.UsageJSON != "" {
		dst.UsageJSON = src.UsageJSON
	}
	if src.ResultStatus != "" {
		dst.ResultStatus = src.ResultStatus
	}
}
