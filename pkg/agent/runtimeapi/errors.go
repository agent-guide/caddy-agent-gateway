package runtimeapi

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorAgentNotFound          ErrorCode = "agent_not_found"
	ErrorAgentDisabled          ErrorCode = "agent_disabled"
	ErrorRuntimeNotExecutable   ErrorCode = "runtime_not_executable"
	ErrorCapabilityNotSupported ErrorCode = "capability_not_supported"
	ErrorInvalidRequest         ErrorCode = "invalid_request"
	ErrorUnsupportedOption      ErrorCode = "unsupported_option"
	ErrorSessionNotFound        ErrorCode = "session_not_found"
	ErrorSessionBusy            ErrorCode = "session_busy"
	ErrorSessionLimitExceeded   ErrorCode = "session_limit_exceeded"
	ErrorRunNotFound            ErrorCode = "run_not_found"
	ErrorPermissionRequired     ErrorCode = "permission_required"
	ErrorPermissionNotFound     ErrorCode = "permission_not_found"
	ErrorPermissionExpired      ErrorCode = "permission_expired"
	ErrorTurnLimitExceeded      ErrorCode = "turn_limit_exceeded"
	ErrorTurnCancelled          ErrorCode = "turn_cancelled"
	ErrorBackendUnavailable     ErrorCode = "backend_unavailable"
	ErrorBackendTimeout         ErrorCode = "backend_timeout"
	ErrorTurnFailed             ErrorCode = "turn_failed"
)

// Error is a normalized Agent runtime failure. Message is safe for public
// responses; Cause is available to trusted logs through Unwrap.
type Error struct {
	Code    ErrorCode
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return e.Message
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is compares normalized errors by code, allowing errors.Is against the
// package sentinel values below.
func (e *Error) Is(target error) bool {
	if e == nil {
		return target == nil
	}
	other, ok := target.(*Error)
	return ok && e.Code != "" && e.Code == other.Code
}

func NewError(code ErrorCode, message string) error {
	return &Error{Code: code, Message: message}
}

func WrapError(code ErrorCode, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func ErrorCodeOf(err error) (ErrorCode, bool) {
	var normalized *Error
	if !errors.As(err, &normalized) {
		return "", false
	}
	return normalized.Code, true
}

var (
	ErrAgentNotFound          = &Error{Code: ErrorAgentNotFound}
	ErrAgentDisabled          = &Error{Code: ErrorAgentDisabled}
	ErrRuntimeNotExecutable   = &Error{Code: ErrorRuntimeNotExecutable}
	ErrCapabilityNotSupported = &Error{Code: ErrorCapabilityNotSupported}
	ErrInvalidRequest         = &Error{Code: ErrorInvalidRequest}
	ErrUnsupportedOption      = &Error{Code: ErrorUnsupportedOption}
	ErrSessionNotFound        = &Error{Code: ErrorSessionNotFound}
	ErrSessionBusy            = &Error{Code: ErrorSessionBusy}
	ErrSessionLimitExceeded   = &Error{Code: ErrorSessionLimitExceeded}
	ErrRunNotFound            = &Error{Code: ErrorRunNotFound}
	ErrPermissionRequired     = &Error{Code: ErrorPermissionRequired}
	ErrPermissionNotFound     = &Error{Code: ErrorPermissionNotFound}
	ErrPermissionExpired      = &Error{Code: ErrorPermissionExpired}
	ErrTurnLimitExceeded      = &Error{Code: ErrorTurnLimitExceeded}
	ErrTurnCancelled          = &Error{Code: ErrorTurnCancelled}
	ErrBackendUnavailable     = &Error{Code: ErrorBackendUnavailable}
	ErrBackendTimeout         = &Error{Code: ErrorBackendTimeout}
	ErrTurnFailed             = &Error{Code: ErrorTurnFailed}
)

func runtimeNotExecutable(runtimeType string) error {
	return NewError(
		ErrorRuntimeNotExecutable,
		fmt.Sprintf("agent runtime %q is not executable", runtimeType),
	)
}
