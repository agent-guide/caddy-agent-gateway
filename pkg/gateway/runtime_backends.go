package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	builtinhost "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"go.uber.org/zap"
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

// ACPTurnServer is the native ACP turn execution surface the ACP backend
// drives. The production implementation is the process-pool runtime manager;
// tests inject a fake through BootstrapOptions.ACPRuntime.
type ACPTurnServer interface {
	ServeConfiguredTurn(context.Context, string, acpruntime.RuntimeConfig, acpruntime.TurnRequest, acpruntime.EventSink) error
}

// acpAgentConfig is one canonical, identity-free ACP execution config entry
// keyed by agent_id. It is produced once at definition refresh by translating
// the legacy bound-service record (the only M4 config source); turn dispatch,
// capability discovery, and session/transcript reads consume only this shape.
type acpAgentConfig struct {
	config      acpruntime.RuntimeConfig
	fingerprint string
	disabled    bool
	configError string
}

// ACPBackend is the permanent Agent runtime adapter over the native ACP
// manager. Legacy ACP routes decide whether to enter this boundary.
type ACPBackend struct {
	// services is read only during listener preparation/direct refresh;
	// per-request paths never touch the service store.
	services       acpServiceSource
	runtime        ACPTurnServer
	runs           *runtimeapi.RunRegistry
	permissions    *runtimeapi.PermissionBroker
	logger         *zap.Logger
	continuationMu sync.Mutex
	continuations  map[string]acpPermissionContinuation

	configMu sync.RWMutex
	configs  map[string]acpAgentConfig
}

// PrepareRuntimeConfigs builds the canonical Agent ACP config snapshot before
// the definition swap lock is acquired. Its returned commit performs only
// bounded in-memory publication/retirement; process, permission-continuation,
// and cancellation I/O is deferred to the returned cleanup.
func (b *ACPBackend) PrepareRuntimeConfigs(ctx context.Context, agents []agentpkg.Agent) agentpkg.DefinitionCommit {
	if b == nil || b.services == nil {
		return nil
	}
	next := make(map[string]acpAgentConfig, len(agents))
	for _, a := range agents {
		serviceID := a.ACPServiceID()
		if serviceID == "" {
			continue
		}
		cfg, err := b.services.Get(ctx, serviceID)
		if err != nil {
			message := "referenced ACP service config is missing"
			next[a.ID] = acpAgentConfig{configError: message}
			b.logConfigError(a.ID, serviceID, message, err)
			continue
		}
		runtimeCfg := runtimeConfigFromService(cfg)
		fingerprint, err := runtimeCfg.Fingerprint(a.ID)
		if err != nil {
			message := "ACP runtime config fingerprint is invalid"
			next[a.ID] = acpAgentConfig{disabled: cfg.Disabled, configError: message}
			b.logConfigError(a.ID, serviceID, message, err)
			continue
		}
		next[a.ID] = acpAgentConfig{config: runtimeCfg, fingerprint: fingerprint, disabled: cfg.Disabled}
	}
	return func() agentpkg.DefinitionCleanup {
		b.configMu.Lock()
		prev := b.configs
		var retireCleanups []func()
		deferredRetirer, deferred := b.runtime.(interface {
			RetireOwnerDeferred(string, string) (int, func())
		})
		retirer, legacy := b.runtime.(interface{ RetireOwner(string, string) int })
		retire := func(agentID, keep string) {
			if deferred {
				_, cleanup := deferredRetirer.RetireOwnerDeferred(agentID, keep)
				retireCleanups = append(retireCleanups, cleanup)
			} else if legacy {
				retireCleanups = append(retireCleanups, func() { retirer.RetireOwner(agentID, keep) })
			}
		}
		// Establish accepted fingerprints before publishing the new Agent
		// generation. Disabled or invalid configs accept no fingerprint.
		for agentID, entry := range next {
			keep := entry.fingerprint
			if entry.disabled || entry.configError != "" {
				keep = ""
			}
			retire(agentID, keep)
		}
		var permissionCleanups []func(context.Context) int
		var cancelAgents []string
		for agentID, prevEntry := range prev {
			nextEntry, exists := next[agentID]
			changed := !exists || nextEntry.fingerprint != prevEntry.fingerprint || nextEntry.disabled != prevEntry.disabled || nextEntry.configError != prevEntry.configError
			if !exists {
				retire(agentID, "")
			}
			if changed && b.permissions != nil {
				permissionCleanups = append(permissionCleanups, b.permissions.ClaimAgent(agentID))
			}
			if !exists || (changed && (nextEntry.disabled || nextEntry.configError != "")) {
				cancelAgents = append(cancelAgents, agentID)
			}
		}
		b.configs = next
		b.configMu.Unlock()

		return func(cleanupCtx context.Context) {
			for _, agentID := range cancelAgents {
				if err := b.runs.CancelAgent(cleanupCtx, agentID); err != nil && b.logger != nil {
					b.logger.Error("cancel retired Agent runs", zap.String("agent_id", agentID), zap.Error(err))
				}
			}
			permissionCtx := runtimeapi.WithPermissionSource(cleanupCtx, "config_fingerprint_retirement")
			for _, cleanup := range permissionCleanups {
				cleanup(permissionCtx)
			}
			for _, cleanup := range retireCleanups {
				cleanup()
			}
		}
	}
}

// RefreshRuntimeConfigs applies a refresh directly for bootstrap and focused
// backend tests. Definition commits use PrepareRuntimeConfigs instead.
func (b *ACPBackend) RefreshRuntimeConfigs(ctx context.Context, agents []agentpkg.Agent) {
	commit := b.PrepareRuntimeConfigs(ctx, agents)
	if commit == nil {
		return
	}
	if cleanup := commit(); cleanup != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cleanup(cleanupCtx)
	}
}

func (b *ACPBackend) logConfigError(agentID, serviceID, message string, err error) {
	if b.logger == nil {
		return
	}
	b.logger.Error(message, zap.String("agent_id", agentID), zap.String("service_id", serviceID), zap.Error(err))
}

// agentRuntimeConfig reads the canonical snapshot entry for one Agent. The
// returned config is cloned so no turn can alias snapshot-owned maps/slices.
func (b *ACPBackend) agentRuntimeConfig(agentID string) (acpAgentConfig, error) {
	if b == nil {
		return acpAgentConfig{}, runtimeapi.NewError(runtimeapi.ErrorBackendUnavailable, "acp runtime is unavailable")
	}
	b.configMu.RLock()
	entry, ok := b.configs[strings.TrimSpace(agentID)]
	b.configMu.RUnlock()
	if !ok {
		return acpAgentConfig{}, runtimeapi.NewError(runtimeapi.ErrorBackendUnavailable, "acp runtime config is not loaded for this agent")
	}
	if entry.configError != "" {
		return acpAgentConfig{}, runtimeapi.NewError(runtimeapi.ErrorBackendUnavailable, entry.configError)
	}
	entry.config = cloneRuntimeConfig(entry.config)
	return entry, nil
}

func runtimeConfigFromService(cfg acpservice.ServiceConfig) acpruntime.RuntimeConfig {
	return acpruntime.RuntimeConfig{
		AgentType: cfg.AgentType, CWD: cfg.CWD, AllowedRoots: append([]string(nil), cfg.AllowedRoots...),
		DefaultModel: cfg.DefaultModel, Env: cloneStringMap(cfg.Env), ConfigOverrides: cloneStringMap(cfg.ConfigOverrides),
		IdleTTL: cfg.IdleTTL, MaxInstances: cfg.MaxInstances, PermissionMode: cfg.PermissionMode, Codex: cloneCodexConfig(cfg.Codex),
	}
}

func cloneRuntimeConfig(cfg acpruntime.RuntimeConfig) acpruntime.RuntimeConfig {
	cfg.AllowedRoots = append([]string(nil), cfg.AllowedRoots...)
	cfg.Env = cloneStringMap(cfg.Env)
	cfg.ConfigOverrides = cloneStringMap(cfg.ConfigOverrides)
	cfg.Codex = cloneCodexConfig(cfg.Codex)
	return cfg
}

type acpPermissionContinuation struct {
	runtime interface {
		ResolvePermission(acpruntime.PermissionDecision) error
	}
	requestID string
}

func (b *ACPBackend) ValidateContinuationDecision(token string, info runtimeapi.PendingPermission, d runtimeapi.PermissionDecision) error {
	if _, ok := b.acpContinuation(token, false); !ok {
		return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
	}
	if len(d.Decisions) != 0 {
		return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "acp permission decisions do not accept per-action decisions")
	}
	switch strings.TrimSpace(d.Outcome) {
	case "selected":
		if strings.TrimSpace(d.OptionID) == "" {
			return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "option_id is required for selected acp permission outcome")
		}
		if len(info.Options) > 0 && !slices.ContainsFunc(info.Options, func(option runtimeapi.PermissionOption) bool {
			return option.OptionID == strings.TrimSpace(d.OptionID)
		}) {
			return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "option_id was not advertised by the acp agent")
		}
	case "cancelled":
	default:
		return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "invalid acp permission outcome")
	}
	return nil
}

func (b *ACPBackend) ResolveContinuation(_ context.Context, token string, d runtimeapi.PermissionDecision, _ time.Time) error {
	continuation, ok := b.acpContinuation(token, true)
	if !ok {
		return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
	}
	err := continuation.runtime.ResolvePermission(acpruntime.PermissionDecision{RequestID: continuation.requestID, Outcome: d.Outcome, OptionID: d.OptionID})
	if errors.Is(err, acpruntime.ErrPermissionNotFound) {
		return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission request not found")
	}
	return mapACPError(err)
}
func (b *ACPBackend) ExpireContinuation(_ context.Context, token string) error {
	continuation, ok := b.acpContinuation(token, true)
	if !ok {
		return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
	}
	err := continuation.runtime.ResolvePermission(acpruntime.PermissionDecision{RequestID: continuation.requestID, Outcome: "cancelled"})
	if errors.Is(err, acpruntime.ErrPermissionNotFound) {
		return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
	}
	return mapACPError(err)
}

func (b *ACPBackend) acpContinuation(token string, take bool) (acpPermissionContinuation, bool) {
	b.continuationMu.Lock()
	defer b.continuationMu.Unlock()
	continuation, ok := b.continuations[strings.TrimSpace(token)]
	if ok && take {
		delete(b.continuations, strings.TrimSpace(token))
	}
	return continuation, ok
}

func (b *ACPBackend) storeACPContinuation(token string, continuation acpPermissionContinuation) {
	b.continuationMu.Lock()
	b.continuations[token] = continuation
	b.continuationMu.Unlock()
}

type RuntimeControls struct {
	Runs        *runtimeapi.RunRegistry
	Permissions *runtimeapi.PermissionBroker
	Logger      *zap.Logger
}

func NewACPBackend(services acpServiceSource, runtime ACPTurnServer, controls ...RuntimeControls) *ACPBackend {
	b := &ACPBackend{services: services, runtime: runtime, continuations: map[string]acpPermissionContinuation{}, configs: map[string]acpAgentConfig{}}
	if len(controls) > 0 {
		b.runs, b.permissions, b.logger = controls[0].Runs, controls[0].Permissions, controls[0].Logger
	}
	return b
}

func (*ACPBackend) RuntimeType() string { return agentpkg.RuntimeTypeACP }

func (b *ACPBackend) Capabilities(ctx context.Context, a agentpkg.Agent) (runtimeapi.Capabilities, error) {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeACP); err != nil {
		return runtimeapi.Capabilities{}, err
	}
	if b == nil || b.services == nil || b.runtime == nil {
		return runtimeapi.Capabilities{}, runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "acp runtime is not executable")
	}
	_ = ctx
	entry, err := b.agentRuntimeConfig(a.ID)
	if err != nil {
		return runtimeapi.Capabilities{}, err
	}
	if entry.disabled {
		return runtimeapi.Capabilities{}, runtimeapi.NewError(runtimeapi.ErrorAgentDisabled, "acp runtime is disabled")
	}
	return runtimeapi.Capabilities{
		Executable: true, Turn: runtimeapi.TurnCapabilities{Streaming: true},
		Sessions:     runtimeapi.SessionCapabilities{Resume: true, List: true, Transcript: true, Durable: true},
		Permissions:  runtimeapi.PermissionCapabilities{Interactive: entry.config.PermissionMode == "interactive", ResumeMode: runtimeapi.PermissionResumeActiveStream},
		Cancellation: runtimeapi.CancelCapabilities{Force: true},
		Events:       []string{runtimeapi.EventSession, runtimeapi.EventDelta, runtimeapi.EventReasoning, runtimeapi.EventContent, runtimeapi.EventPlan, runtimeapi.EventToolCall, runtimeapi.EventUsage, runtimeapi.EventAvailableCommands, runtimeapi.EventSessionInfo, runtimeapi.EventMode, runtimeapi.EventConfigOptions, runtimeapi.EventPermission, runtimeapi.EventDone, runtimeapi.EventError},
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
	entry, err := b.agentRuntimeConfig(a.ID)
	if err != nil {
		return err
	}
	if entry.disabled {
		return runtimeapi.NewError(runtimeapi.ErrorAgentDisabled, "acp runtime is disabled")
	}
	ctx = bridgeRuntimeIdentities(ctx)
	runtimeCfg := entry.config
	nativeReq := acpruntime.TurnRequest{
		RunID:    req.RunID,
		ThreadID: opts.ThreadID, SessionID: req.SessionID, Input: req.Input, CWD: opts.CWD,
		Model: opts.Model, FreshSession: opts.FreshSession, ConfigOverrides: cloneStringMap(opts.ConfigOverrides),
	}
	terminalState, stopReason, suspended := runtimeapi.RunStateCompleted, "", false
	if b.runs != nil {
		if err := b.runs.Begin(a.ID, a.Runtime.Type, req.RunID, req.SessionID, func(cancelCtx context.Context, mode runtimeapi.CancelMode) error {
			if mode != runtimeapi.CancelModeForce {
				return runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "acp graceful cancellation is not supported")
			}
			canceller, ok := b.runtime.(interface{ CancelRun(string, string) error })
			if !ok {
				return runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "acp cancellation is not supported")
			}
			if b.permissions != nil {
				b.permissions.DrainRun(runtimeapi.WithPermissionSource(context.Background(), "run_cancel"), a.ID, req.RunID)
			}
			var err error
			if runtimeapi.IsAgentRetirementCancellation(cancelCtx) {
				if requester, supported := b.runtime.(interface{ RequestCancelRun(string, string) error }); supported {
					err = requester.RequestCancelRun(a.ID, req.RunID)
				} else {
					err = canceller.CancelRun(a.ID, req.RunID)
				}
			} else {
				err = canceller.CancelRun(a.ID, req.RunID)
			}
			// The common registry publishes immediately before ServeConfiguredTurn
			// enters the native registry. A cancellation in that narrow window is
			// known to target a live common run even though native lookup cannot see
			// it yet, so preserve the same retryable contract as native pre-bind.
			if errors.Is(err, acpruntime.ErrRunNotFound) {
				return runtimeapi.NewError(runtimeapi.ErrorBackendUnavailable, "run cancellation is not ready; retry")
			}
			return mapACPCancelError(err)
		}); err != nil {
			return err
		}
		defer func() {
			if !suspended {
				b.runs.Complete(a.ID, req.RunID, terminalState, stopReason)
			}
		}()
	}
	err = b.runtime.ServeConfiguredTurn(ctx, a.ID, runtimeCfg, nativeReq, func(ev acpruntime.TurnEvent) error {
		registeredPermission := false
		if ev.SessionID != "" && b.runs != nil {
			b.runs.SetSession(a.ID, req.RunID, ev.SessionID)
		}
		if ev.Event == runtimeapi.EventDone {
			stopReason = ev.StopReason
			suspended = ev.StopReason == runtimeapi.StopReasonPermissionRequired
			if ev.StopReason == runtimeapi.StopReasonCancelled {
				terminalState = runtimeapi.RunStateCancelled
			}
		}
		if ev.Event == runtimeapi.EventError {
			terminalState = runtimeapi.RunStateFailed
		}
		if ev.Event == runtimeapi.EventPermission && b.permissions != nil {
			if resolver, ok := b.runtime.(interface {
				ResolvePermission(acpruntime.PermissionDecision) error
			}); ok {
				token, tokenErr := runtimeapi.NewContinuationToken()
				if tokenErr != nil {
					return runtimeapi.WrapError(runtimeapi.ErrorTurnFailed, "generate permission continuation token", tokenErr)
				}
				b.storeACPContinuation(token, acpPermissionContinuation{runtime: resolver, requestID: ev.RequestID})
				_, regErr := b.permissions.Register(runtimeapi.PendingPermission{RequestID: ev.RequestID, AgentID: a.ID, RuntimeType: a.Runtime.Type, RunID: req.RunID, SessionID: ev.SessionID, ExpiresAt: ev.PermissionExpiresAt, Actions: acpPermissionActions(ev.Data), Options: acpPermissionOptions(ev.Data), ResumeMode: runtimeapi.PermissionResumeActiveStream}, token, b)
				if regErr != nil {
					b.acpContinuation(token, true)
					return regErr
				}
				registeredPermission = true
			}
		}
		if emitErr := emit(commonACPEvent(ev)); emitErr != nil {
			if registeredPermission {
				_ = b.permissions.Expire(runtimeapi.WithPermissionSource(context.Background(), "permission_delivery_failed"), a.ID, ev.RequestID)
			}
			return emitErr
		}
		return nil
	})
	if errors.Is(err, acpruntime.ErrTurnCancelled) {
		terminalState, stopReason = runtimeapi.RunStateCancelled, runtimeapi.StopReasonCancelled
	}
	if err != nil && terminalState != runtimeapi.RunStateCancelled {
		terminalState = runtimeapi.RunStateFailed
	}
	return mapACPError(err)
}

func (b *ACPBackend) CancelRun(ctx context.Context, a agentpkg.Agent, req runtimeapi.CancelRequest) (runtimeapi.CancelResult, error) {
	if req.Mode != runtimeapi.CancelModeForce {
		return runtimeapi.CancelResult{}, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "acp graceful cancellation is not supported")
	}
	if b.runs == nil {
		return runtimeapi.CancelResult{}, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "run cancellation is not configured")
	}
	return b.runs.Cancel(ctx, a.ID, req)
}

func (b *ACPBackend) ResolvePermission(ctx context.Context, a agentpkg.Agent, decision runtimeapi.PermissionDecision) error {
	if b.permissions == nil {
		return runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "permission resolution is not configured")
	}
	return b.permissions.Resolve(ctx, a.ID, decision)
}

func (b *ACPBackend) ListSessions(ctx context.Context, a agentpkg.Agent, req runtimeapi.ListSessionsRequest) (runtimeapi.ListSessionsResponse, error) {
	provider, ok := b.runtime.(interface {
		ListConfiguredSessions(context.Context, string, acpruntime.RuntimeConfig, acpruntime.ListSessionsRequest) (acpruntime.ListSessionsResponse, error)
	})
	if !ok {
		return runtimeapi.ListSessionsResponse{}, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "session listing is not supported")
	}
	cfg, err := b.runtimeConfig(ctx, a)
	if err != nil {
		return runtimeapi.ListSessionsResponse{}, err
	}
	resp, err := provider.ListConfiguredSessions(ctx, a.ID, cfg, acpruntime.ListSessionsRequest{CWD: req.CWD, Cursor: req.Cursor})
	if err != nil {
		return runtimeapi.ListSessionsResponse{}, mapACPError(err)
	}
	out := runtimeapi.ListSessionsResponse{NextCursor: resp.NextCursor, Sessions: make([]runtimeapi.Session, 0, len(resp.Sessions))}
	for _, s := range resp.Sessions {
		out.Sessions = append(out.Sessions, runtimeapi.Session{SessionID: s.SessionID, Title: s.Title, UpdatedAt: s.UpdatedAt, Details: s.Meta})
	}
	return out, nil
}

func (b *ACPBackend) LoadTranscript(ctx context.Context, a agentpkg.Agent, req runtimeapi.TranscriptRequest) (runtimeapi.TranscriptResponse, error) {
	provider, ok := b.runtime.(interface {
		LoadConfiguredTranscript(context.Context, string, acpruntime.RuntimeConfig, acpruntime.TranscriptRequest) (acpruntime.TranscriptResponse, error)
	})
	if !ok {
		return runtimeapi.TranscriptResponse{}, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "transcript loading is not supported")
	}
	cfg, err := b.runtimeConfig(ctx, a)
	if err != nil {
		return runtimeapi.TranscriptResponse{}, err
	}
	resp, err := provider.LoadConfiguredTranscript(ctx, a.ID, cfg, acpruntime.TranscriptRequest{SessionID: req.SessionID, CWD: req.CWD})
	if err != nil {
		return runtimeapi.TranscriptResponse{}, mapACPError(err)
	}
	out := runtimeapi.TranscriptResponse{SessionID: resp.SessionID, Messages: make([]runtimeapi.TranscriptMessage, 0, len(resp.Messages))}
	for _, m := range resp.Messages {
		out.Messages = append(out.Messages, runtimeapi.TranscriptMessage{Role: m.Role, Text: m.Text})
	}
	return out, nil
}

func (b *ACPBackend) RuntimeSummary(_ context.Context, a agentpkg.Agent) (runtimeapi.RuntimeSummary, error) {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeACP); err != nil {
		return runtimeapi.RuntimeSummary{}, err
	}
	if b == nil || b.services == nil || b.runtime == nil {
		return runtimeapi.RuntimeSummary{}, runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "acp runtime is not executable")
	}
	b.configMu.RLock()
	entry, configured := b.configs[a.ID]
	b.configMu.RUnlock()
	if !configured || entry.configError != "" {
		message := entry.configError
		if message == "" {
			message = "ACP runtime config is not loaded"
		}
		details, _ := json.Marshal(map[string]string{"reason": "config_missing", "message": message})
		return runtimeapi.RuntimeSummary{Type: a.Runtime.Type, Executable: false, Healthy: false, State: runtimeapi.RuntimeStateUnhealthy, Details: details}, nil
	}
	if entry.disabled {
		return runtimeapi.RuntimeSummary{Type: a.Runtime.Type, Executable: false, Healthy: false, State: runtimeapi.RuntimeStateDisabled}, nil
	}
	summary := runtimeapi.RuntimeSummary{Type: a.Runtime.Type, Executable: true, Healthy: true, State: runtimeapi.RuntimeStateReady}
	if inspector, ok := b.runtime.(interface {
		ListActiveRuns(string) []acpruntime.ActiveRunInfo
	}); ok {
		summary.ActiveRuns = len(inspector.ListActiveRuns(a.ID))
	}
	if b.permissions != nil {
		summary.PendingPermissions = len(b.permissions.List(a.ID))
	}
	if inspector, ok := b.runtime.(interface {
		ListOwnerInstances(string) []acpruntime.PooledInstanceInfo
	}); ok {
		details := struct {
			Instances []acpruntime.PooledInstanceInfo `json:"instances"`
		}{Instances: []acpruntime.PooledInstanceInfo{}}
		sessions := map[string]struct{}{}
		for _, inst := range inspector.ListOwnerInstances(a.ID) {
			details.Instances = append(details.Instances, inst)
			if inst.SessionID != "" {
				sessions[inst.SessionID] = struct{}{}
			}
			if !inst.Alive {
				summary.Healthy = false
				summary.State = runtimeapi.RuntimeStateDegraded
			}
			at := inst.LastUsed
			if summary.LastActivityAt == nil || at.After(*summary.LastActivityAt) {
				summary.LastActivityAt = &at
			}
		}
		summary.SessionCount = len(sessions)
		summary.Details, _ = json.Marshal(details)
	}
	return summary, nil
}
func (b *ACPBackend) Health(_ context.Context, a agentpkg.Agent) (runtimeapi.Health, error) {
	err := validateBackendAgent(a, agentpkg.RuntimeTypeACP)
	if err == nil && (b == nil || b.services == nil || b.runtime == nil) {
		err = runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "acp runtime is not executable")
	}
	if err == nil {
		b.configMu.RLock()
		entry, configured := b.configs[a.ID]
		b.configMu.RUnlock()
		switch {
		case !configured || entry.configError != "":
			message := entry.configError
			if message == "" {
				message = "ACP runtime config is not loaded"
			}
			details, _ := json.Marshal(map[string]string{"reason": "config_missing"})
			return runtimeapi.Health{Healthy: false, State: runtimeapi.RuntimeStateUnhealthy, CheckedAt: time.Now().UTC(), Message: message, Details: details}, nil
		case entry.disabled:
			return runtimeapi.Health{Healthy: false, State: runtimeapi.RuntimeStateDisabled, CheckedAt: time.Now().UTC(), Message: "ACP runtime is disabled"}, nil
		}
	}
	state := runtimeapi.RuntimeStateReady
	healthy := err == nil
	if err != nil {
		state = runtimeapi.RuntimeStateUnhealthy
	}
	return runtimeapi.Health{Healthy: healthy, State: state, CheckedAt: time.Now().UTC()}, err
}

func (b *ACPBackend) runtimeConfig(_ context.Context, a agentpkg.Agent) (acpruntime.RuntimeConfig, error) {
	entry, err := b.agentRuntimeConfig(a.ID)
	if err != nil {
		return acpruntime.RuntimeConfig{}, err
	}
	if entry.disabled {
		return acpruntime.RuntimeConfig{}, runtimeapi.NewError(runtimeapi.ErrorAgentDisabled, "acp runtime is disabled")
	}
	return entry.config, nil
}

// BuiltinBackend is the permanent Agent runtime adapter over the in-process
// ADK Host. Cursor state lives beside the Host's process-lifetime checkpoint.
type BuiltinBackend struct {
	host           builtinTurnServer
	runs           *runtimeapi.RunRegistry
	permissions    *runtimeapi.PermissionBroker
	continuationMu sync.Mutex
	continuations  map[string]builtinPermissionContinuation
	decidedMu      sync.Mutex
	decided        map[string]decidedPermission
}

type decidedPermission struct {
	decision  runtimeapi.PermissionDecision
	agentID   string
	runID     string
	expiresAt time.Time
}

type builtinPermissionContinuation struct {
	agentID, runID string
	requestID      string
}

func (b *BuiltinBackend) ResolveContinuation(_ context.Context, token string, d runtimeapi.PermissionDecision, expiresAt time.Time) error {
	continuation, ok := b.builtinContinuation(token, true)
	if !ok {
		return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
	}
	b.decidedMu.Lock()
	b.decided[continuation.requestID] = decidedPermission{decision: d, agentID: continuation.agentID, runID: continuation.runID, expiresAt: expiresAt}
	b.decidedMu.Unlock()
	return nil
}
func (b *BuiltinBackend) ValidateContinuationDecision(token string, info runtimeapi.PendingPermission, d runtimeapi.PermissionDecision) error {
	if _, ok := b.builtinContinuation(token, false); !ok {
		return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
	}
	if strings.TrimSpace(d.OptionID) != "" {
		return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "builtin permission decisions do not accept option_id")
	}
	outcome := strings.TrimSpace(d.Outcome)
	if outcome != "" && outcome != "cancel" {
		return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "invalid builtin permission outcome")
	}
	known := make(map[string]struct{}, len(info.Actions))
	for _, action := range info.Actions {
		known[action.ActionID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(d.Decisions))
	for _, decision := range d.Decisions {
		actionID := strings.TrimSpace(decision.ActionID)
		if actionID == "" || (decision.Outcome != "allow" && decision.Outcome != "deny") {
			return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "invalid builtin per-action permission decision")
		}
		if _, duplicate := seen[actionID]; duplicate {
			return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "duplicate builtin per-action permission decision")
		}
		if _, exists := known[actionID]; !exists {
			return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "builtin permission decision references an unknown action")
		}
		seen[actionID] = struct{}{}
	}
	return nil
}
func (b *BuiltinBackend) ExpireContinuation(_ context.Context, token string) error {
	continuation, ok := b.builtinContinuation(token, true)
	if !ok {
		return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
	}
	removed := false
	if host, ok := b.host.(interface{ ExpirePermission(string, string) bool }); ok {
		removed = host.ExpirePermission(continuation.agentID, continuation.requestID)
	}
	b.decidedMu.Lock()
	delete(b.decided, continuation.requestID)
	b.decidedMu.Unlock()
	if !removed {
		return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
	}
	return nil
}

func (b *BuiltinBackend) builtinContinuation(token string, take bool) (builtinPermissionContinuation, bool) {
	b.continuationMu.Lock()
	defer b.continuationMu.Unlock()
	continuation, ok := b.continuations[strings.TrimSpace(token)]
	if ok && take {
		delete(b.continuations, strings.TrimSpace(token))
	}
	return continuation, ok
}

func (b *BuiltinBackend) storeBuiltinContinuation(token string, continuation builtinPermissionContinuation) {
	b.continuationMu.Lock()
	b.continuations[token] = continuation
	b.continuationMu.Unlock()
}

type builtinTurnServer interface {
	ServeTurn(context.Context, string, builtinhost.TurnRequest, builtinhost.EventSink) error
	LoadContinuationCursor(string, string) (builtinhost.ContinuationCursor, bool)
	StoreContinuationCursor(string, string, builtinhost.ContinuationCursor) bool
}

func NewBuiltinBackend(host builtinTurnServer, controls ...RuntimeControls) *BuiltinBackend {
	b := &BuiltinBackend{host: host, continuations: map[string]builtinPermissionContinuation{}, decided: map[string]decidedPermission{}}
	if len(controls) > 0 {
		b.runs, b.permissions = controls[0].Runs, controls[0].Permissions
	}
	return b
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
		Sessions:     runtimeapi.SessionCapabilities{Resume: true},
		Permissions:  runtimeapi.PermissionCapabilities{Interactive: a.Runtime.Builtin.Permissions.Interactive(), ResumeMode: runtimeapi.PermissionResumeNewStream},
		Cancellation: runtimeapi.CancelCapabilities{Force: true, Graceful: true},
		Events:       []string{runtimeapi.EventSession, runtimeapi.EventDelta, runtimeapi.EventContent, runtimeapi.EventToolCall, runtimeapi.EventUsage, runtimeapi.EventPermission, runtimeapi.EventDone, runtimeapi.EventError},
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
	if req.Permission != nil && b.permissions != nil {
		now := time.Now().UTC()
		b.decidedMu.Lock()
		entry, already := b.decided[req.Permission.RequestID]
		if already && entry.agentID == a.ID && entry.runID == req.RunID {
			delete(b.decided, req.Permission.RequestID)
		} else {
			already = false
		}
		b.decidedMu.Unlock()
		if already && !now.Before(entry.expiresAt) {
			already = false
			if host, ok := b.host.(interface{ ExpirePermission(string, string) bool }); ok {
				host.ExpirePermission(a.ID, req.Permission.RequestID)
			}
			return runtimeapi.NewError(runtimeapi.ErrorPermissionExpired, "permission continuation expired")
		}
		decided := entry.decision
		if !already {
			if err := b.permissions.Resolve(runtimeapi.WithPermissionSource(ctx, "builtin_route"), a.ID, *req.Permission); err != nil {
				return err
			}
			b.decidedMu.Lock()
			entry, already = b.decided[req.Permission.RequestID]
			if already && entry.agentID == a.ID && entry.runID == req.RunID {
				delete(b.decided, req.Permission.RequestID)
			} else {
				already = false
			}
			b.decidedMu.Unlock()
			if already && !now.Before(entry.expiresAt) {
				already = false
				if host, ok := b.host.(interface{ ExpirePermission(string, string) bool }); ok {
					host.ExpirePermission(a.ID, req.Permission.RequestID)
				}
				return runtimeapi.NewError(runtimeapi.ErrorPermissionExpired, "permission continuation expired")
			}
			decided = entry.decision
		}
		if !already {
			return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation not found")
		}
		req.Permission = &decided
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
	terminalState, stopReason, suspended := runtimeapi.RunStateCompleted, "", false
	if b.runs != nil {
		bindRun := b.runs.Begin
		if req.Permission != nil {
			bindRun = b.runs.Rebind
		}
		if err := bindRun(a.ID, a.Runtime.Type, req.RunID, req.SessionID, func(_ context.Context, mode runtimeapi.CancelMode) error {
			c, ok := b.host.(interface {
				CancelRun(string, string, builtinhost.CancelMode) (bool, error)
			})
			if !ok {
				return runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "builtin cancellation is not supported")
			}
			nativeMode := builtinhost.CancelModeForce
			if mode == runtimeapi.CancelModeGraceful {
				nativeMode = builtinhost.CancelModeGraceful
			} else if mode != runtimeapi.CancelModeForce {
				return runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "invalid cancel mode")
			}
			drained := 0
			if b.permissions != nil {
				drained = b.permissions.DrainRun(runtimeapi.WithPermissionSource(context.Background(), "run_cancel"), a.ID, req.RunID)
			}
			discarded := b.discardRunContinuations(a.ID, req.RunID)
			cancelled, err := c.CancelRun(a.ID, req.RunID, nativeMode)
			if err != nil {
				return mapBuiltinError(err)
			}
			if !cancelled {
				if drained > 0 || discarded > 0 {
					return nil
				}
				return runtimeapi.NewError(runtimeapi.ErrorRunNotFound, "run not found")
			}
			return nil
		}); err != nil {
			if req.Permission != nil && errors.Is(err, runtimeapi.ErrRunNotFound) {
				if host, ok := b.host.(interface{ ExpirePermission(string, string) bool }); ok {
					host.ExpirePermission(a.ID, req.Permission.RequestID)
				}
				if b.permissions != nil {
					b.permissions.RecordContinuationLost(runtimeapi.WithPermissionSource(ctx, "builtin_resume"), a.ID, req.Permission.RequestID)
				}
				return runtimeapi.NewError(runtimeapi.ErrorPermissionNotFound, "permission continuation is no longer available")
			}
			return err
		}
		defer func() {
			if !suspended {
				b.runs.Complete(a.ID, req.RunID, terminalState, stopReason)
			}
		}()
	}
	err := b.host.ServeTurn(ctx, a.ID, nativeReq, func(ev builtinhost.TurnEvent) error {
		registeredPermission := false
		if ev.SessionID != "" && b.runs != nil {
			b.runs.SetSession(a.ID, req.RunID, ev.SessionID)
		}
		if ev.Event == runtimeapi.EventDone {
			stopReason = ev.StopReason
			suspended = ev.StopReason == runtimeapi.StopReasonPermissionRequired
			if ev.StopReason == runtimeapi.StopReasonCancelled {
				terminalState = runtimeapi.RunStateCancelled
			}
		}
		if ev.Event == runtimeapi.EventError {
			terminalState = runtimeapi.RunStateFailed
		}
		if ev.Event == runtimeapi.EventPermission && b.permissions != nil {
			actions := []runtimeapi.PermissionAction{}
			var payload struct {
				Calls []struct {
					CallID string `json:"call_id"`
					Name   string `json:"name"`
				} `json:"calls"`
			}
			_ = json.Unmarshal(ev.Data, &payload)
			for _, call := range payload.Calls {
				actions = append(actions, runtimeapi.PermissionAction{ActionID: call.CallID, Name: call.Name})
			}
			ttl := 10 * time.Minute
			if permissions := a.Runtime.Builtin.Permissions; permissions != nil && permissions.TimeoutSeconds > 0 {
				seconds := permissions.TimeoutSeconds
				ttl = time.Duration(seconds) * time.Second
			}
			token, tokenErr := runtimeapi.NewContinuationToken()
			if tokenErr != nil {
				b.expireBuiltinRequests(a.ID, []string{ev.RequestID})
				return runtimeapi.WrapError(runtimeapi.ErrorTurnFailed, "generate permission continuation token", tokenErr)
			}
			b.storeBuiltinContinuation(token, builtinPermissionContinuation{agentID: a.ID, runID: req.RunID, requestID: ev.RequestID})
			_, regErr := b.permissions.Register(runtimeapi.PendingPermission{RequestID: ev.RequestID, AgentID: a.ID, RuntimeType: a.Runtime.Type, RunID: req.RunID, SessionID: ev.SessionID, TTL: ttl, Actions: actions, ResumeMode: runtimeapi.PermissionResumeNewStream}, token, b)
			if regErr != nil {
				b.builtinContinuation(token, true)
				b.expireBuiltinRequests(a.ID, []string{ev.RequestID})
				return regErr
			}
			registeredPermission = true
		}
		if emitErr := emit(commonBuiltinEvent(ev)); emitErr != nil {
			if registeredPermission {
				_ = b.permissions.Expire(runtimeapi.WithPermissionSource(context.Background(), "permission_delivery_failed"), a.ID, ev.RequestID)
			}
			return emitErr
		}
		return nil
	})
	if err != nil && terminalState != runtimeapi.RunStateCancelled {
		terminalState = runtimeapi.RunStateFailed
	}
	return mapBuiltinError(err)
}

func acpPermissionActions(data json.RawMessage) []runtimeapi.PermissionAction {
	var payload struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
		} `json:"toolCall"`
	}
	if json.Unmarshal(data, &payload) != nil || strings.TrimSpace(payload.ToolCall.ToolCallID) == "" {
		return nil
	}
	return []runtimeapi.PermissionAction{{ActionID: payload.ToolCall.ToolCallID, Name: payload.ToolCall.Title}}
}

func acpPermissionOptions(data json.RawMessage) []runtimeapi.PermissionOption {
	var payload struct {
		Options []struct {
			OptionID string `json:"optionId"`
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			Name     string `json:"name"`
		} `json:"options"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	options := make([]runtimeapi.PermissionOption, 0, len(payload.Options))
	for _, option := range payload.Options {
		id := strings.TrimSpace(option.OptionID)
		if id == "" {
			id = strings.TrimSpace(option.ID)
		}
		if id != "" {
			options = append(options, runtimeapi.PermissionOption{OptionID: id, Kind: strings.TrimSpace(option.Kind), Name: strings.TrimSpace(option.Name)})
		}
	}
	return options
}

func (b *BuiltinBackend) CancelRun(ctx context.Context, a agentpkg.Agent, req runtimeapi.CancelRequest) (runtimeapi.CancelResult, error) {
	if req.Mode != runtimeapi.CancelModeForce && req.Mode != runtimeapi.CancelModeGraceful {
		return runtimeapi.CancelResult{}, runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "invalid cancel mode")
	}
	if b.runs == nil {
		return runtimeapi.CancelResult{}, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "run cancellation is not configured")
	}
	return b.runs.Cancel(ctx, a.ID, req)
}
func (b *BuiltinBackend) ResolvePermission(ctx context.Context, a agentpkg.Agent, decision runtimeapi.PermissionDecision) error {
	if b.permissions == nil {
		return runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "permission resolution is not configured")
	}
	return b.permissions.Resolve(ctx, a.ID, decision)
}

// DiscardAgentContinuations removes Admin-decided new-stream continuations
// when an Agent definition is updated or deleted. Pending broker entries are
// drained separately; these entries have already been claimed by Admin.
func (b *BuiltinBackend) DiscardAgentContinuations(agentID string) {
	if b == nil {
		return
	}
	var requestIDs []string
	b.decidedMu.Lock()
	for requestID, entry := range b.decided {
		if entry.agentID == strings.TrimSpace(agentID) {
			delete(b.decided, requestID)
			requestIDs = append(requestIDs, requestID)
		}
	}
	b.decidedMu.Unlock()
	b.expireBuiltinRequests(agentID, requestIDs)
}

// DiscardAllContinuations removes every Admin-decided continuation during
// gateway shutdown after the common broker has drained still-pending records.
func (b *BuiltinBackend) DiscardAllContinuations() {
	if b == nil {
		return
	}
	byAgent := map[string][]string{}
	b.decidedMu.Lock()
	for requestID, entry := range b.decided {
		delete(b.decided, requestID)
		byAgent[entry.agentID] = append(byAgent[entry.agentID], requestID)
	}
	b.decidedMu.Unlock()
	for agentID, requestIDs := range byAgent {
		b.expireBuiltinRequests(agentID, requestIDs)
	}
}

func (b *BuiltinBackend) discardRunContinuations(agentID, runID string) int {
	var requestIDs []string
	b.decidedMu.Lock()
	for requestID, entry := range b.decided {
		if entry.agentID == strings.TrimSpace(agentID) && entry.runID == strings.TrimSpace(runID) {
			delete(b.decided, requestID)
			requestIDs = append(requestIDs, requestID)
		}
	}
	b.decidedMu.Unlock()
	b.expireBuiltinRequests(agentID, requestIDs)
	return len(requestIDs)
}

func (b *BuiltinBackend) expireBuiltinRequests(agentID string, requestIDs []string) {
	host, ok := b.host.(interface{ ExpirePermission(string, string) bool })
	if !ok {
		return
	}
	for _, requestID := range requestIDs {
		host.ExpirePermission(agentID, requestID)
	}
}
func (b *BuiltinBackend) RuntimeSummary(_ context.Context, a agentpkg.Agent) (runtimeapi.RuntimeSummary, error) {
	stateProvider, ok := b.host.(interface {
		State(string) builtinhost.EntryState
	})
	if !ok {
		return runtimeapi.RuntimeSummary{}, runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "builtin runtime is unavailable")
	}
	state := stateProvider.State(a.ID)
	runtimeState := runtimeapi.RuntimeStateReady
	if !state.Materialized {
		runtimeState = runtimeapi.RuntimeStateUnknown
	}
	summary := runtimeapi.RuntimeSummary{Type: a.Runtime.Type, Executable: true, Healthy: true, State: runtimeState, ActiveRuns: state.InflightTurns, SessionCount: state.LiveSessions}
	summary.Details, _ = json.Marshal(state)
	if b.permissions != nil {
		summary.PendingPermissions = len(b.permissions.List(a.ID))
	}
	if !state.MaterializedAt.IsZero() {
		summary.LastActivityAt = &state.MaterializedAt
	}
	return summary, nil
}
func (b *BuiltinBackend) Health(_ context.Context, a agentpkg.Agent) (runtimeapi.Health, error) {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeBuiltin); err != nil {
		return runtimeapi.Health{Healthy: false, State: runtimeapi.RuntimeStateUnhealthy, CheckedAt: time.Now().UTC()}, err
	}
	return runtimeapi.Health{Healthy: true, State: runtimeapi.RuntimeStateReady, CheckedAt: time.Now().UTC()}, nil
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
	case errors.Is(err, acpruntime.ErrTurnCancelled):
		return runtimeapi.WrapError(runtimeapi.ErrorTurnCancelled, "acp turn cancelled", err)
	case errors.Is(err, acpruntime.ErrRuntimeConfigRetired):
		return runtimeapi.WrapError(runtimeapi.ErrorBackendUnavailable, "acp runtime config was retired", err)
	case strings.Contains(err.Error(), "does not advertise session/list"), strings.Contains(err.Error(), "does not advertise session/load"):
		return runtimeapi.WrapError(runtimeapi.ErrorCapabilityNotSupported, "acp capability is not supported", err)
	default:
		return runtimeapi.NormalizeError(err)
	}
}

func mapACPCancelError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, acpruntime.ErrRunNotFound) {
		return runtimeapi.NewError(runtimeapi.ErrorRunNotFound, "run not found")
	}
	if errors.Is(err, acpruntime.ErrRunNotReady) {
		return runtimeapi.NewError(runtimeapi.ErrorBackendUnavailable, "run cancellation is not ready; retry")
	}
	return mapACPError(err)
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
