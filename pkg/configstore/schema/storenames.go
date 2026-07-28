package schema

const (
	StoreProviders     = "providers"
	StoreCredentials   = "credentials"
	StoreRoutes        = "routes"
	StoreVirtualKeys   = "virtual_keys"
	StoreManagedModels = "managed_models"
	StoreMCPServices   = "mcp_services"
	// StoreACPServices names the removed pre-M5 family for legacy detection and
	// migration tests only. RegisterDefaultStores deliberately does not expose it.
	StoreACPServices = "acp_services"
	StoreAgents      = "agents"
)
