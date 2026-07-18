package provider

// ModelInfo describes a model available from a provider.
type ModelInfo struct {
	ID           string
	Name         string
	DisplayName  string
	Description  string
	Capabilities ModelCapabilities
}

// Usage contains token consumption information. TotalTokens is the
// upstream-reported total when available and InputTokens+OutputTokens
// otherwise. CachedTokens counts the prompt tokens served from a
// provider-side cache (a subset of InputTokens); ReasoningTokens counts
// completion tokens spent on reasoning (a subset of OutputTokens). The
// detail fields are zero when the upstream does not report them.
type Usage struct {
	InputTokens     int
	OutputTokens    int
	TotalTokens     int
	CachedTokens    int
	ReasoningTokens int
}
