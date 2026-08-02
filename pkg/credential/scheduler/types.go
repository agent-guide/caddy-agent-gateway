package scheduler

import (
	"time"

	"github.com/agent-guide/agent-gateway/pkg/credential/model"
)

type Credential = model.Credential
type ManagedCredential = model.ManagedCredential
type QuotaState = model.QuotaState
type ModelState = model.ModelState
type Error = model.Error

type Result struct {
	CredentialID   string
	Model          string
	Success        bool
	RetryAfter     *time.Duration
	Error          *Error
	CredentialWide bool
}

type Filter struct {
	Type            string
	CredentialScope string
	Model           string
	Selector        string
}
