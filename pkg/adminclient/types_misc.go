package adminclient

type boolStatusResponse struct {
	Status       string `json:"status"`
	Enabled      bool   `json:"enabled"`
	ProviderType string `json:"provider_type,omitempty"`
}
