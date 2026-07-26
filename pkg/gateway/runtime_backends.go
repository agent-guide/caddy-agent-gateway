package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	builtinhost "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
)

type acpRuntimeOptionsV1 struct {
	ThreadID        string            `json:"thread_id"`
	CWD             string            `json:"cwd,omitempty"`
	Model           string            `json:"model,omitempty"`
	FreshSession    bool              `json:"fresh_session,omitempty"`
	ConfigOverrides map[string]string `json:"config_overrides,omitempty"`
}

type acpServiceSource interface {
	Get(context.Context, string) (acpservice.ServiceConfig, error)
}

type acpTurnServer interface {
	ServeConfiguredTurn(context.Context, string, acpruntime.RuntimeConfig, acpruntime.TurnRequest, acpruntime.EventSink) error
}

// ACPBackend is the permanent Agent runtime adapter over the native ACP
// manager. Legacy ACP routes decide whether to enter this boundary.
type ACPBackend struct {
	services acpServiceSource
	runtime  acpTurnServer
}

func NewACPBackend(services acpServiceSource, runtime acpTurnServer) *ACPBackend {
	return &ACPBackend{services: services, runtime: runtime}
}

func (*ACPBackend) RuntimeType() string { return agentpkg.RuntimeTypeACP }

func (b *ACPBackend) Capabilities(ctx context.Context, a agentpkg.Agent) (runtimeapi.Capabilities, error) {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeACP); err != nil {
		return runtimeapi.Capabilities{}, err
	}
	if b == nil || b.services == nil || b.runtime == nil {
		return runtimeapi.Capabilities{}, runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "acp runtime is not executable")
	}
	cfg, err := b.services.Get(ctx, a.ACPServiceID())
	if err != nil {
		return runtimeapi.Capabilities{}, mapACPError(err)
	}
	if cfg.Disabled {
		return runtimeapi.Capabilities{}, runtimeapi.NewError(runtimeapi.ErrorAgentDisabled, "acp runtime is disabled")
	}
	return runtimeapi.Capabilities{
		Executable: true, Turn: runtimeapi.TurnCapabilities{Streaming: true},
		// List/transcript remain on the legacy ACP route until M3 moves them
		// onto the optional runtimeapi capability interfaces.
		Sessions:    runtimeapi.SessionCapabilities{Resume: true, Durable: true},
		Permissions: runtimeapi.PermissionCapabilities{Interactive: cfg.PermissionMode == "interactive", ResumeMode: runtimeapi.PermissionResumeActiveStream},
		Events:      []string{runtimeapi.EventSession, runtimeapi.EventDelta, runtimeapi.EventReasoning, runtimeapi.EventContent, runtimeapi.EventPlan, runtimeapi.EventToolCall, runtimeapi.EventUsage, runtimeapi.EventAvailableCommands, runtimeapi.EventSessionInfo, runtimeapi.EventMode, runtimeapi.EventConfigOptions, runtimeapi.EventPermission, runtimeapi.EventDone, runtimeapi.EventError},
	}, nil
}

func (b *ACPBackend) ServeTurn(ctx context.Context, a agentpkg.Agent, req runtimeapi.TurnRequest, emit runtimeapi.EventSink) error {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeACP); err != nil {
		return err
	}
	if b == nil || b.services == nil || b.runtime == nil {
		return runtimeapi.NewError(runtimeapi.ErrorBackendUnavailable, "acp runtime is unavailable")
	}
	if err := validateRuntimeOptionsVersion(req.Options); err != nil {
		return err
	}
	var opts acpRuntimeOptionsV1
	if err := runtimeapi.DecodeRuntimeOptions(req.Options.Runtime, &opts); err != nil {
		return err
	}
	opts.ThreadID = strings.TrimSpace(opts.ThreadID)
	if opts.ThreadID == "" || strings.TrimSpace(req.Input) == "" {
		return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "thread_id and input are required")
	}
	cfg, err := b.services.Get(ctx, a.ACPServiceID())
	if err != nil {
		return mapACPError(err)
	}
	if cfg.Disabled {
		return runtimeapi.NewError(runtimeapi.ErrorAgentDisabled, "acp runtime is disabled")
	}
	ctx = bridgeRuntimeIdentities(ctx)
	runtimeCfg := acpruntime.RuntimeConfig{
		AgentType: cfg.AgentType, CWD: cfg.CWD, AllowedRoots: append([]string(nil), cfg.AllowedRoots...),
		DefaultModel: cfg.DefaultModel, Env: cloneStringMap(cfg.Env), ConfigOverrides: cloneStringMap(cfg.ConfigOverrides),
		IdleTTL: cfg.IdleTTL, MaxInstances: cfg.MaxInstances, PermissionMode: cfg.PermissionMode, Codex: cloneCodexConfig(cfg.Codex),
	}
	nativeReq := acpruntime.TurnRequest{
		ThreadID: opts.ThreadID, SessionID: req.SessionID, Input: req.Input, CWD: opts.CWD,
		Model: opts.Model, FreshSession: opts.FreshSession, ConfigOverrides: cloneStringMap(opts.ConfigOverrides),
	}
	err = b.runtime.ServeConfiguredTurn(ctx, a.ID, runtimeCfg, nativeReq, func(ev acpruntime.TurnEvent) error {
		return emit(commonACPEvent(ev))
	})
	return mapACPError(err)
}

// BuiltinBackend is the permanent Agent runtime adapter over the in-process
// ADK Host. Cursor state lives beside the Host's process-lifetime checkpoint.
type BuiltinBackend struct {
	host builtinTurnServer
}

type builtinTurnServer interface {
	ServeTurn(context.Context, string, builtinhost.TurnRequest, builtinhost.EventSink) error
	LoadContinuationCursor(string, string) (builtinhost.ContinuationCursor, bool)
	StoreContinuationCursor(string, string, builtinhost.ContinuationCursor) bool
}

func NewBuiltinBackend(host builtinTurnServer) *BuiltinBackend {
	return &BuiltinBackend{host: host}
}

func (*BuiltinBackend) RuntimeType() string { return agentpkg.RuntimeTypeBuiltin }

func (b *BuiltinBackend) Capabilities(_ context.Context, a agentpkg.Agent) (runtimeapi.Capabilities, error) {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeBuiltin); err != nil {
		return runtimeapi.Capabilities{}, err
	}
	if b == nil || b.host == nil {
		return runtimeapi.Capabilities{}, runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "builtin runtime is not executable")
	}
	return runtimeapi.Capabilities{
		Executable: true, Turn: runtimeapi.TurnCapabilities{Streaming: true},
		Sessions:    runtimeapi.SessionCapabilities{Resume: true},
		Permissions: runtimeapi.PermissionCapabilities{Interactive: a.Runtime.Builtin.Permissions.Interactive(), ResumeMode: runtimeapi.PermissionResumeNewStream},
		Events:      []string{runtimeapi.EventSession, runtimeapi.EventDelta, runtimeapi.EventContent, runtimeapi.EventToolCall, runtimeapi.EventUsage, runtimeapi.EventPermission, runtimeapi.EventDone, runtimeapi.EventError},
	}, nil
}

func (b *BuiltinBackend) ServeTurn(ctx context.Context, a agentpkg.Agent, req runtimeapi.TurnRequest, emit runtimeapi.EventSink) error {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeBuiltin); err != nil {
		return err
	}
	if b == nil || b.host == nil {
		return runtimeapi.NewError(runtimeapi.ErrorBackendUnavailable, "builtin runtime is unavailable")
	}
	if err := validateRuntimeOptionsVersion(req.Options); err != nil {
		return err
	}
	var noOptions struct{}
	if err := runtimeapi.DecodeRuntimeOptions(req.Options.Runtime, &noOptions); err != nil {
		return err
	}
	nativeReq := builtinhost.TurnRequest{RunID: req.RunID, SessionID: req.SessionID, Input: strings.TrimSpace(req.Input)}
	if req.Permission != nil {
		nativeReq.Permission = &builtinhost.TurnPermission{RequestID: req.Permission.RequestID, Outcome: req.Permission.Outcome}
		for _, decision := range req.Permission.Decisions {
			nativeReq.Permission.Decisions = append(nativeReq.Permission.Decisions, builtinhost.TurnPermissionDecision{CallID: decision.ActionID, Outcome: decision.Outcome})
		}
	}
	ctx = bridgeRuntimeIdentities(ctx)
	err := b.host.ServeTurn(ctx, a.ID, nativeReq, func(ev builtinhost.TurnEvent) error {
		return emit(commonBuiltinEvent(ev))
	})
	return mapBuiltinError(err)
}

func (b *BuiltinBackend) LoadContinuationCursor(_ context.Context, a agentpkg.Agent, requestID string) (runtimeapi.EventCursor, error) {
	cursor, ok := b.host.LoadContinuationCursor(a.ID, strings.TrimSpace(requestID))
	if !ok {
		return runtimeapi.EventCursor{}, runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
	}
	return runtimeapi.EventCursor{RunID: cursor.RunID, NextSequence: cursor.NextSequence, NextSegment: cursor.NextSegment}, nil
}

func (b *BuiltinBackend) StoreContinuationCursor(_ context.Context, a agentpkg.Agent, requestID string, cursor runtimeapi.EventCursor) error {
	if strings.TrimSpace(requestID) == "" || !runtimeapi.ValidRunID(cursor.RunID) {
		return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "invalid permission continuation cursor")
	}
	stored := b.host.StoreContinuationCursor(a.ID, strings.TrimSpace(requestID), builtinhost.ContinuationCursor{
		RunID: cursor.RunID, NextSequence: cursor.NextSequence, NextSegment: cursor.NextSegment,
	})
	if !stored {
		return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
	}
	return nil
}

func validateBackendAgent(a agentpkg.Agent, runtimeType string) error {
	if strings.TrimSpace(a.ID) == "" {
		return runtimeapi.NewError(runtimeapi.ErrorAgentNotFound, "agent not found")
	}
	if a.Disabled {
		return runtimeapi.NewError(runtimeapi.ErrorAgentDisabled, "agent is disabled")
	}
	if a.Runtime.Type != runtimeType {
		return runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "agent runtime does not match backend")
	}
	if runtimeType == agentpkg.RuntimeTypeACP && (a.Runtime.ACP == nil || strings.TrimSpace(a.Runtime.ACP.ServiceID) == "") {
		return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "runtime.acp.service_id is required")
	}
	if runtimeType == agentpkg.RuntimeTypeBuiltin && a.Runtime.Builtin == nil {
		return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "runtime.builtin is required")
	}
	return nil
}

func validateRuntimeOptionsVersion(options runtimeapi.TurnOptions) error {
	if options.Version != "" && options.Version != runtimeapi.TurnOptionsVersionV1 {
		return runtimeapi.NewError(runtimeapi.ErrorUnsupportedOption, "unsupported runtime options version")
	}
	return nil
}

func bridgeRuntimeIdentities(ctx context.Context) context.Context {
	ids, _ := runtimeapi.IdentitiesFromContext(ctx)
	dims, _ := usage.DimensionsFromContext(ctx)
	if ids.AgentID == "" {
		ids.AgentID = dims.AgentID
	}
	if ids.RuntimeType == "" {
		ids.RuntimeType = dims.RuntimeType
	}
	if ids.RunID == "" {
		ids.RunID = dims.RunID
	}
	if ids.TraceID == "" {
		ids.TraceID = dims.TraceID
	}
	if ids.SpanID == "" {
		ids.SpanID = dims.SpanID
	}
	if ids.ParentSpanID == "" {
		ids.ParentSpanID = dims.ParentSpanID
	}
	dims.AgentID, dims.RuntimeType, dims.RunID = ids.AgentID, ids.RuntimeType, ids.RunID
	dims.TraceID, dims.SpanID, dims.ParentSpanID = ids.TraceID, ids.SpanID, ids.ParentSpanID
	ctx = runtimeapi.WithIdentities(ctx, ids)
	return usage.ContextWithDimensions(ctx, dims)
}

func commonACPEvent(ev acpruntime.TurnEvent) runtimeapi.TurnEvent {
	data := ev.Data
	if len(data) == 0 && (ev.StopReason != "" || ev.Message != "") {
		data, _ = json.Marshal(struct {
			StopReason string `json:"stop_reason,omitempty"`
			Message    string `json:"message,omitempty"`
		}{ev.StopReason, ev.Message})
	}
	return runtimeapi.TurnEvent{Event: ev.Event, SessionID: ev.SessionID, RequestID: ev.RequestID, Text: ev.Text, Data: data}
}

func commonBuiltinEvent(ev builtinhost.TurnEvent) runtimeapi.TurnEvent {
	data := ev.Data
	if len(data) == 0 && (ev.StopReason != "" || ev.Message != "") {
		data, _ = json.Marshal(struct {
			StopReason string `json:"stop_reason,omitempty"`
			Message    string `json:"message,omitempty"`
		}{ev.StopReason, ev.Message})
	}
	return runtimeapi.TurnEvent{Event: ev.Event, RunID: ev.RunID, SessionID: ev.SessionID, RequestID: ev.RequestID, Text: ev.Text, Data: data}
}

func mapACPError(err error) error {
	if err == nil || runtimeapi.IsNormalized(err) {
		return err
	}
	switch {
	case errors.Is(err, acpservice.ErrServiceNotConfigured):
		return runtimeapi.WrapError(runtimeapi.ErrorBackendUnavailable, "acp service is unavailable", err)
	case errors.Is(err, acpruntime.ErrInvalidRequest):
		return runtimeapi.WrapError(runtimeapi.ErrorInvalidRequest, "invalid acp request", err)
	case errors.Is(err, acpruntime.ErrCapacityExceeded):
		return runtimeapi.WrapError(runtimeapi.ErrorTurnLimitExceeded, "acp runtime capacity exceeded", err)
	default:
		return runtimeapi.NormalizeError(err)
	}
}

func mapBuiltinError(err error) error {
	if err == nil || runtimeapi.IsNormalized(err) {
		return err
	}
	switch {
	case errors.Is(err, builtinhost.ErrAgentNotFound):
		return runtimeapi.WrapError(runtimeapi.ErrorAgentNotFound, "builtin agent not found", err)
	case errors.Is(err, builtinhost.ErrTurnLimitExceeded), errors.Is(err, builtinhost.ErrPermissionCapacity):
		return runtimeapi.WrapError(runtimeapi.ErrorTurnLimitExceeded, "builtin turn limit exceeded", err)
	case errors.Is(err, builtinhost.ErrSessionBusy):
		return runtimeapi.WrapError(runtimeapi.ErrorSessionBusy, "builtin session is busy", err)
	case errors.Is(err, builtinhost.ErrSessionLimitExceeded):
		return runtimeapi.WrapError(runtimeapi.ErrorSessionLimitExceeded, "builtin session limit exceeded", err)
	case errors.Is(err, builtinhost.ErrInvalidRequest):
		return runtimeapi.WrapError(runtimeapi.ErrorInvalidRequest, "invalid builtin request", err)
	default:
		return runtimeapi.NormalizeError(err)
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneCodexConfig(in *acpservice.CodexConfig) *acpservice.CodexConfig {
	if in == nil {
		return nil
	}
	out := *in
	out.AdapterArgs = append([]string(nil), in.AdapterArgs...)
	out.AppServerArgs = append([]string(nil), in.AppServerArgs...)
	return &out
}

var _ runtimeapi.Backend = (*ACPBackend)(nil)
var _ runtimeapi.Backend = (*BuiltinBackend)(nil)
var _ runtimeapi.ContinuationCursorBackend = (*BuiltinBackend)(nil)
