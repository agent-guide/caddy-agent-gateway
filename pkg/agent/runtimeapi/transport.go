package runtimeapi

import (
	"errors"
	"net/http"
)

type PublicErrorPayload struct {
	ErrorType ErrorCode `json:"error_type"`
	Message   string    `json:"message"`
}

// HTTPStatus maps a normalized runtime error to its pre-stream status.
func HTTPStatus(err error) int {
	err = NormalizeError(err)
	code, ok := ErrorCodeOf(err)
	if !ok {
		return http.StatusBadGateway
	}
	switch code {
	case ErrorAgentNotFound, ErrorSessionNotFound, ErrorRunNotFound, ErrorPermissionNotFound:
		return http.StatusNotFound
	case ErrorPermissionRequired:
		return http.StatusConflict
	case ErrorPermissionExpired:
		return http.StatusGone
	case ErrorSessionBusy, ErrorSessionLimitExceeded, ErrorTurnLimitExceeded:
		return http.StatusTooManyRequests
	case ErrorRuntimeNotExecutable, ErrorCapabilityNotSupported:
		return http.StatusNotImplemented
	case ErrorBackendUnavailable:
		return http.StatusServiceUnavailable
	case ErrorBackendTimeout:
		return http.StatusGatewayTimeout
	case ErrorTurnFailed:
		return http.StatusBadGateway
	case ErrorTurnCancelled:
		return 499
	default:
		return http.StatusBadRequest
	}
}

// PublicError returns a fixed safe message for external JSON/SSE responses.
// Causes and caller-provided normalized messages are deliberately excluded.
func PublicError(err error) PublicErrorPayload {
	err = NormalizeError(err)
	code, ok := ErrorCodeOf(err)
	if !ok {
		code = ErrorTurnFailed
	}
	return PublicErrorPayload{ErrorType: code, Message: publicErrorMessages[code]}
}

var publicErrorMessages = map[ErrorCode]string{
	ErrorAgentNotFound:          "agent not found",
	ErrorAgentDisabled:          "agent is disabled",
	ErrorRuntimeNotExecutable:   "agent runtime is not executable",
	ErrorCapabilityNotSupported: "capability is not supported",
	ErrorInvalidRequest:         "invalid request",
	ErrorUnsupportedOption:      "runtime option is not supported",
	ErrorSessionNotFound:        "session not found",
	ErrorSessionBusy:            "session is busy",
	ErrorSessionLimitExceeded:   "session limit exceeded",
	ErrorRunNotFound:            "run not found",
	ErrorPermissionRequired:     "permission is required",
	ErrorPermissionNotFound:     "permission request not found",
	ErrorPermissionExpired:      "permission request expired",
	ErrorTurnLimitExceeded:      "turn limit exceeded",
	ErrorTurnCancelled:          "turn cancelled",
	ErrorBackendUnavailable:     "runtime backend is unavailable",
	ErrorBackendTimeout:         "runtime backend timed out",
	ErrorTurnFailed:             "turn failed",
}

func IsNormalized(err error) bool {
	var normalized *Error
	return errors.As(err, &normalized)
}
