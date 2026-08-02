package credential

// Filter identifies credentials for manager-side listing and storage operations.
type Filter struct {
	Type         string
	ProviderType string
	ProviderID   string
	Model        string
}
