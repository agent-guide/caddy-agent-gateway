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
	acphost "github.com/agent-guide/agent-gateway/pkg/acp/host"
	"github.com/agent-guide/agent-gateway/pkg/acp/hostconfig"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	builtinhost "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	agentruntime "github.com/agent-guide/agent-gateway/pkg/agent/runtime"
	"go.uber.org/zap"
)

type acpRuntimeOptionsV1 struct {
	ThreadID        string            `json:"thread_id"`
	CWD             string            `json:"cwd,omitempty"`
	Model           string            `json:"model,omitempty"`
	FreshSession    bool              `json:"fresh_session,omitempty"`
	ConfigOverrides map[string]string `json:"config_overrides,omitempty"`
}

// ACPTurnServer is the native ACP turn execution surface the ACP backend
// drives. The production implementation is the process-pool runtime manager;
// tests inject a fake through BootstrapOptions.ACPRuntime.
type ACPTurnServer interface {
	ServeConfiguredTurn(context.Context, string, hostconfig.Config, acphost.TurnRequest, acphost.EventSink) error
}

// acpAgentConfig is one canonical, identity-free ACP execution config entry
// keyed by agent_id. It is produced once at definition refresh from the
// Agent-owned runtime.acp config; turn dispatch, capability discovery, and
// session/transcript reads consume only this shape.
type acpAgentConfig struct {
	config      hostconfig.Config
	fingerprint string
	disabled    bool
	configError string
}

// ACPBackend is the permanent Agent runtime adapter over the native ACP
// manager.
type ACPBackend struct {
	runtime        ACPTurnServer
	runs           *agentruntime.RunRegistry
	permissions    *agentruntime.PermissionBroker
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
	if b == nil {
		return nil
	}
	next := make(map[string]acpAgentConfig, len(agents))
	for _, a := range agents {
		if a.Runtime.Type != agentpkg.RuntimeTypeACP || a.Runtime.ACP == nil {
			continue
		}
		runtimeCfg := runtimeConfigFromAgent(*a.Runtime.ACP)
		disabled := a.Disabled
		fingerprint, err := runtimeCfg.Fingerprint(a.ID)
		if err != nil {
			message := "ACP runtime config fingerprint is invalid"
			next[a.ID] = acpAgentConfig{disabled: disabled, configError: message}
			b.logConfigError(a.ID, message, err)
			continue
		}
		next[a.ID] = acpAgentConfig{config: runtimeCfg, fingerprint: fingerprint, disabled: disabled}
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
			permissionCtx := agentruntime.WithPermissionSource(cleanupCtx, "config_fingerprint_retirement")
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

func (b *ACPBackend) logConfigError(agentID, message string, err error) {
	if b.logger == nil {
		return
	}
	b.logger.Error(message, zap.String("agent_id", agentID), zap.Error(err))
}

// agentRuntimeConfig reads the canonical snapshot entry for one Agent. The
// returned config is cloned so no turn can alias snapshot-owned maps/slices.
func (b *ACPBackend) agentRuntimeConfig(agentID string) (acpAgentConfig, error) {
	if b == nil {
		return acpAgentConfig{}, agentruntime.NewError(agentruntime.ErrorBackendUnavailable, "acp runtime is unavailable")
	}
	b.configMu.RLock()
	entry, ok := b.configs[strings.TrimSpace(agentID)]
	b.configMu.RUnlock()
	if !ok {
		return acpAgentConfig{}, agentruntime.NewError(agentruntime.ErrorBackendUnavailable, "acp runtime config is not loaded for this agent")
	}
	if entry.configError != "" {
		return acpAgentConfig{}, agentruntime.NewError(agentruntime.ErrorBackendUnavailable, entry.configError)
	}
	entry.config = cloneRuntimeConfig(entry.config)
	return entry, nil
}

func runtimeConfigFromAgent(cfg agentpkg.ACPRuntime) hostconfig.Config {
	return hostconfig.Config{
		AgentType: cfg.AgentType, CWD: cfg.CWD, AllowedRoots: append([]string(nil), cfg.AllowedRoots...),
		DefaultModel: cfg.DefaultModel, Env: cloneStringMap(cfg.Env), ConfigOverrides: cloneStringMap(cfg.ConfigOverrides),
		IdleTTL: cfg.IdleTTL, MaxInstances: cfg.MaxInstances, PermissionMode: cfg.PermissionMode, Codex: cloneCodexConfig(cfg.Codex),
	}
}

func cloneRuntimeConfig(cfg hostconfig.Config) hostconfig.Config {
	cfg.AllowedRoots = append([]string(nil), cfg.AllowedRoots...)
	cfg.Env = cloneStringMap(cfg.Env)
	cfg.ConfigOverrides = cloneStringMap(cfg.ConfigOverrides)
	cfg.Codex = cloneCodexConfig(cfg.Codex)
	return cfg
}

type acpPermissionContinuation struct {
	runtime interface {
		ResolvePermission(acphost.PermissionDecision) error
	}
	requestID string
}

func (b *ACPBackend) ValidateContinuationDecision(token string, info agentruntime.PendingPermission, d agentruntime.PermissionDecision) error {
	if _, ok := b.acpContinuation(token, false); !ok {
		return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
	}
	if len(d.Decisions) != 0 {
		return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "acp permission decisions do not accept per-action decisions")
	}
	switch strings.TrimSpace(d.Outcome) {
	case "selected":
		if strings.TrimSpace(d.OptionID) == "" {
			return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "option_id is required for selected acp permission outcome")
		}
		if len(info.Options) > 0 && !slices.ContainsFunc(info.Options, func(option agentruntime.PermissionOption) bool {
			return option.OptionID == strings.TrimSpace(d.OptionID)
		}) {
			return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "option_id was not advertised by the acp agent")
		}
	case "cancelled":
	default:
		return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "invalid acp permission outcome")
	}
	return nil
}

func (b *ACPBackend) ResolveContinuation(_ context.Context, token string, d agentruntime.PermissionDecision, _ time.Time) error {
	continuation, ok := b.acpContinuation(token, true)
	if !ok {
		return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
	}
	err := continuation.runtime.ResolvePermission(acphost.PermissionDecision{RequestID: continuation.requestID, Outcome: d.Outcome, OptionID: d.OptionID})
	if errors.Is(err, acphost.ErrPermissionNotFound) {
		return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission request not found")
	}
	return mapACPError(err)
}
func (b *ACPBackend) ExpireContinuation(_ context.Context, token string) error {
	continuation, ok := b.acpContinuation(token, true)
	if !ok {
		return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
	}
	err := continuation.runtime.ResolvePermission(acphost.PermissionDecision{RequestID: continuation.requestID, Outcome: "cancelled"})
	if errors.Is(err, acphost.ErrPermissionNotFound) {
		return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
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
	Runs        *agentruntime.RunRegistry
	Permissions *agentruntime.PermissionBroker
	Logger      *zap.Logger
}

func NewACPBackend(runtime ACPTurnServer, controls ...RuntimeControls) *ACPBackend {
	b := &ACPBackend{runtime: runtime, continuations: map[string]acpPermissionContinuation{}, configs: map[string]acpAgentConfig{}}
	if len(controls) > 0 {
		b.runs, b.permissions, b.logger = controls[0].Runs, controls[0].Permissions, controls[0].Logger
	}
	return b
}

func (*ACPBackend) RuntimeType() string { return agentpkg.RuntimeTypeACP }

func (b *ACPBackend) Capabilities(ctx context.Context, a agentpkg.Agent) (agentruntime.Capabilities, error) {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeACP); err != nil {
		return agentruntime.Capabilities{}, err
	}
	if b == nil || b.runtime == nil {
		return agentruntime.Capabilities{}, agentruntime.NewError(agentruntime.ErrorRuntimeNotExecutable, "acp runtime is not executable")
	}
	_ = ctx
	entry, err := b.agentRuntimeConfig(a.ID)
	if err != nil {
		return agentruntime.Capabilities{}, err
	}
	if entry.disabled {
		return agentruntime.Capabilities{}, agentruntime.NewError(agentruntime.ErrorAgentDisabled, "acp runtime is disabled")
	}
	return agentruntime.Capabilities{
		Executable: true, Turn: agentruntime.TurnCapabilities{Streaming: true},
		Sessions:     agentruntime.SessionCapabilities{Resume: true, List: true, Transcript: true, Durable: true},
		Permissions:  agentruntime.PermissionCapabilities{Interactive: entry.config.PermissionMode == "interactive", ResumeMode: agentruntime.PermissionResumeActiveStream},
		Cancellation: agentruntime.CancelCapabilities{Force: true},
		Events:       []string{agentruntime.EventSession, agentruntime.EventDelta, agentruntime.EventReasoning, agentruntime.EventContent, agentruntime.EventPlan, agentruntime.EventToolCall, agentruntime.EventUsage, agentruntime.EventAvailableCommands, agentruntime.EventSessionInfo, agentruntime.EventMode, agentruntime.EventConfigOptions, agentruntime.EventPermission, agentruntime.EventDone, agentruntime.EventError},
	}, nil
}

func (b *ACPBackend) ServeTurn(ctx context.Context, a agentpkg.Agent, req agentruntime.TurnRequest, emit agentruntime.EventSink) error {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeACP); err != nil {
		return err
	}
	if b == nil || b.runtime == nil {
		return agentruntime.NewError(agentruntime.ErrorBackendUnavailable, "acp runtime is unavailable")
	}
	if err := validateRuntimeOptionsVersion(req.Options); err != nil {
		return err
	}
	var opts acpRuntimeOptionsV1
	if err := agentruntime.DecodeRuntimeOptions(req.Options.Runtime, &opts); err != nil {
		return err
	}
	opts.ThreadID = strings.TrimSpace(opts.ThreadID)
	if opts.ThreadID == "" || strings.TrimSpace(req.Input) == "" {
		return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "thread_id and input are required")
	}
	entry, err := b.agentRuntimeConfig(a.ID)
	if err != nil {
		return err
	}
	if entry.disabled {
		return agentruntime.NewError(agentruntime.ErrorAgentDisabled, "acp runtime is disabled")
	}
	ctx = bridgeRuntimeIdentities(ctx)
	runtimeCfg := entry.config
	nativeReq := acphost.TurnRequest{
		RunID:    req.RunID,
		ThreadID: opts.ThreadID, SessionID: req.SessionID, Input: req.Input, CWD: opts.CWD,
		Model: opts.Model, FreshSession: opts.FreshSession, ConfigOverrides: cloneStringMap(opts.ConfigOverrides),
	}
	terminalState, stopReason, suspended := agentruntime.RunStateCompleted, "", false
	if b.runs != nil {
		if err := b.runs.Begin(a.ID, a.Runtime.Type, req.RunID, req.SessionID, func(cancelCtx context.Context, mode agentruntime.CancelMode) error {
			if mode != agentruntime.CancelModeForce {
				return agentruntime.NewError(agentruntime.ErrorCapabilityNotSupported, "acp graceful cancellation is not supported")
			}
			canceller, ok := b.runtime.(interface{ CancelRun(string, string) error })
			if !ok {
				return agentruntime.NewError(agentruntime.ErrorCapabilityNotSupported, "acp cancellation is not supported")
			}
			if b.permissions != nil {
				b.permissions.DrainRun(agentruntime.WithPermissionSource(context.Background(), "run_cancel"), a.ID, req.RunID)
			}
			var err error
			if agentruntime.IsAgentRetirementCancellation(cancelCtx) {
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
			if errors.Is(err, acphost.ErrRunNotFound) {
				return agentruntime.NewError(agentruntime.ErrorBackendUnavailable, "run cancellation is not ready; retry")
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
	err = b.runtime.ServeConfiguredTurn(ctx, a.ID, runtimeCfg, nativeReq, func(ev acphost.TurnEvent) error {
		registeredPermission := false
		if ev.SessionID != "" && b.runs != nil {
			b.runs.SetSession(a.ID, req.RunID, ev.SessionID)
		}
		if ev.Event == agentruntime.EventDone {
			stopReason = ev.StopReason
			suspended = ev.StopReason == agentruntime.StopReasonPermissionRequired
			if ev.StopReason == agentruntime.StopReasonCancelled {
				terminalState = agentruntime.RunStateCancelled
			}
		}
		if ev.Event == agentruntime.EventError {
			terminalState = agentruntime.RunStateFailed
		}
		if ev.Event == agentruntime.EventPermission && b.permissions != nil {
			if resolver, ok := b.runtime.(interface {
				ResolvePermission(acphost.PermissionDecision) error
			}); ok {
				token, tokenErr := agentruntime.NewContinuationToken()
				if tokenErr != nil {
					return agentruntime.WrapError(agentruntime.ErrorTurnFailed, "generate permission continuation token", tokenErr)
				}
				b.storeACPContinuation(token, acpPermissionContinuation{runtime: resolver, requestID: ev.RequestID})
				_, regErr := b.permissions.Register(agentruntime.PendingPermission{RequestID: ev.RequestID, AgentID: a.ID, RuntimeType: a.Runtime.Type, RunID: req.RunID, SessionID: ev.SessionID, ExpiresAt: ev.PermissionExpiresAt, Actions: acpPermissionActions(ev.Data), Options: acpPermissionOptions(ev.Data), ResumeMode: agentruntime.PermissionResumeActiveStream}, token, b)
				if regErr != nil {
					b.acpContinuation(token, true)
					return regErr
				}
				registeredPermission = true
			}
		}
		if emitErr := emit(commonACPEvent(ev)); emitErr != nil {
			if registeredPermission {
				_ = b.permissions.Expire(agentruntime.WithPermissionSource(context.Background(), "permission_delivery_failed"), a.ID, ev.RequestID)
			}
			return emitErr
		}
		return nil
	})
	if errors.Is(err, acphost.ErrTurnCancelled) {
		terminalState, stopReason = agentruntime.RunStateCancelled, agentruntime.StopReasonCancelled
	}
	if err != nil && terminalState != agentruntime.RunStateCancelled {
		terminalState = agentruntime.RunStateFailed
	}
	return mapACPError(err)
}

func (b *ACPBackend) CancelRun(ctx context.Context, a agentpkg.Agent, req agentruntime.CancelRequest) (agentruntime.CancelResult, error) {
	if req.Mode != agentruntime.CancelModeForce {
		return agentruntime.CancelResult{}, agentruntime.NewError(agentruntime.ErrorCapabilityNotSupported, "acp graceful cancellation is not supported")
	}
	if b.runs == nil {
		return agentruntime.CancelResult{}, agentruntime.NewError(agentruntime.ErrorCapabilityNotSupported, "run cancellation is not configured")
	}
	return b.runs.Cancel(ctx, a.ID, req)
}

func (b *ACPBackend) ResolvePermission(ctx context.Context, a agentpkg.Agent, decision agentruntime.PermissionDecision) error {
	if b.permissions == nil {
		return agentruntime.NewError(agentruntime.ErrorCapabilityNotSupported, "permission resolution is not configured")
	}
	return b.permissions.Resolve(ctx, a.ID, decision)
}

func (b *ACPBackend) ListSessions(ctx context.Context, a agentpkg.Agent, req agentruntime.ListSessionsRequest) (agentruntime.ListSessionsResponse, error) {
	provider, ok := b.runtime.(interface {
		ListConfiguredSessions(context.Context, string, hostconfig.Config, acphost.ListSessionsRequest) (acphost.ListSessionsResponse, error)
	})
	if !ok {
		return agentruntime.ListSessionsResponse{}, agentruntime.NewError(agentruntime.ErrorCapabilityNotSupported, "session listing is not supported")
	}
	cfg, err := b.runtimeConfig(ctx, a)
	if err != nil {
		return agentruntime.ListSessionsResponse{}, err
	}
	resp, err := provider.ListConfiguredSessions(ctx, a.ID, cfg, acphost.ListSessionsRequest{CWD: req.CWD, Cursor: req.Cursor})
	if err != nil {
		return agentruntime.ListSessionsResponse{}, mapACPError(err)
	}
	out := agentruntime.ListSessionsResponse{NextCursor: resp.NextCursor, Sessions: make([]agentruntime.Session, 0, len(resp.Sessions))}
	for _, s := range resp.Sessions {
		out.Sessions = append(out.Sessions, agentruntime.Session{SessionID: s.SessionID, Title: s.Title, UpdatedAt: s.UpdatedAt, Details: s.Meta})
	}
	return out, nil
}

func (b *ACPBackend) LoadTranscript(ctx context.Context, a agentpkg.Agent, req agentruntime.TranscriptRequest) (agentruntime.TranscriptResponse, error) {
	provider, ok := b.runtime.(interface {
		LoadConfiguredTranscript(context.Context, string, hostconfig.Config, acphost.TranscriptRequest) (acphost.TranscriptResponse, error)
	})
	if !ok {
		return agentruntime.TranscriptResponse{}, agentruntime.NewError(agentruntime.ErrorCapabilityNotSupported, "transcript loading is not supported")
	}
	cfg, err := b.runtimeConfig(ctx, a)
	if err != nil {
		return agentruntime.TranscriptResponse{}, err
	}
	resp, err := provider.LoadConfiguredTranscript(ctx, a.ID, cfg, acphost.TranscriptRequest{SessionID: req.SessionID, CWD: req.CWD})
	if err != nil {
		return agentruntime.TranscriptResponse{}, mapACPError(err)
	}
	out := agentruntime.TranscriptResponse{SessionID: resp.SessionID, Messages: make([]agentruntime.TranscriptMessage, 0, len(resp.Messages))}
	for _, m := range resp.Messages {
		out.Messages = append(out.Messages, agentruntime.TranscriptMessage{Role: m.Role, Text: m.Text})
	}
	return out, nil
}

func (b *ACPBackend) RuntimeSummary(_ context.Context, a agentpkg.Agent) (agentruntime.RuntimeSummary, error) {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeACP); err != nil {
		return agentruntime.RuntimeSummary{}, err
	}
	if b == nil || b.runtime == nil {
		return agentruntime.RuntimeSummary{}, agentruntime.NewError(agentruntime.ErrorRuntimeNotExecutable, "acp runtime is not executable")
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
		return agentruntime.RuntimeSummary{Type: a.Runtime.Type, Executable: false, Healthy: false, State: agentruntime.RuntimeStateUnhealthy, Details: details}, nil
	}
	if entry.disabled {
		return agentruntime.RuntimeSummary{Type: a.Runtime.Type, Executable: false, Healthy: false, State: agentruntime.RuntimeStateDisabled}, nil
	}
	summary := agentruntime.RuntimeSummary{Type: a.Runtime.Type, Executable: true, Healthy: true, State: agentruntime.RuntimeStateReady}
	if inspector, ok := b.runtime.(interface {
		ListActiveRuns(string) []acphost.ActiveRunInfo
	}); ok {
		summary.ActiveRuns = len(inspector.ListActiveRuns(a.ID))
	}
	if b.permissions != nil {
		summary.PendingPermissions = len(b.permissions.List(a.ID))
	}
	if inspector, ok := b.runtime.(interface {
		ListOwnerInstances(string) []acphost.PooledInstanceInfo
	}); ok {
		details := struct {
			Instances []acphost.PooledInstanceInfo `json:"instances"`
		}{Instances: []acphost.PooledInstanceInfo{}}
		sessions := map[string]struct{}{}
		for _, inst := range inspector.ListOwnerInstances(a.ID) {
			details.Instances = append(details.Instances, inst)
			if inst.SessionID != "" {
				sessions[inst.SessionID] = struct{}{}
			}
			if !inst.Alive {
				summary.Healthy = false
				summary.State = agentruntime.RuntimeStateDegraded
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
func (b *ACPBackend) Health(_ context.Context, a agentpkg.Agent) (agentruntime.Health, error) {
	err := validateBackendAgent(a, agentpkg.RuntimeTypeACP)
	if err == nil && (b == nil || b.runtime == nil) {
		err = agentruntime.NewError(agentruntime.ErrorRuntimeNotExecutable, "acp runtime is not executable")
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
			return agentruntime.Health{Healthy: false, State: agentruntime.RuntimeStateUnhealthy, CheckedAt: time.Now().UTC(), Message: message, Details: details}, nil
		case entry.disabled:
			return agentruntime.Health{Healthy: false, State: agentruntime.RuntimeStateDisabled, CheckedAt: time.Now().UTC(), Message: "ACP runtime is disabled"}, nil
		}
	}
	state := agentruntime.RuntimeStateReady
	healthy := err == nil
	if err != nil {
		state = agentruntime.RuntimeStateUnhealthy
	}
	return agentruntime.Health{Healthy: healthy, State: state, CheckedAt: time.Now().UTC()}, err
}

func (b *ACPBackend) runtimeConfig(_ context.Context, a agentpkg.Agent) (hostconfig.Config, error) {
	entry, err := b.agentRuntimeConfig(a.ID)
	if err != nil {
		return hostconfig.Config{}, err
	}
	if entry.disabled {
		return hostconfig.Config{}, agentruntime.NewError(agentruntime.ErrorAgentDisabled, "acp runtime is disabled")
	}
	return entry.config, nil
}

// BuiltinBackend is the permanent Agent runtime adapter over the in-process
// ADK Host. Cursor state lives beside the Host's process-lifetime checkpoint.
type BuiltinBackend struct {
	host           builtinTurnServer
	runs           *agentruntime.RunRegistry
	permissions    *agentruntime.PermissionBroker
	continuationMu sync.Mutex
	continuations  map[string]builtinPermissionContinuation
	decidedMu      sync.Mutex
	decided        map[string]decidedPermission
}

type decidedPermission struct {
	decision  agentruntime.PermissionDecision
	agentID   string
	runID     string
	expiresAt time.Time
}

type builtinPermissionContinuation struct {
	agentID, runID string
	requestID      string
}

func (b *BuiltinBackend) ResolveContinuation(_ context.Context, token string, d agentruntime.PermissionDecision, expiresAt time.Time) error {
	continuation, ok := b.builtinContinuation(token, true)
	if !ok {
		return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
	}
	b.decidedMu.Lock()
	b.decided[continuation.requestID] = decidedPermission{decision: d, agentID: continuation.agentID, runID: continuation.runID, expiresAt: expiresAt}
	b.decidedMu.Unlock()
	return nil
}
func (b *BuiltinBackend) ValidateContinuationDecision(token string, info agentruntime.PendingPermission, d agentruntime.PermissionDecision) error {
	if _, ok := b.builtinContinuation(token, false); !ok {
		return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
	}
	if strings.TrimSpace(d.OptionID) != "" {
		return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "builtin permission decisions do not accept option_id")
	}
	outcome := strings.TrimSpace(d.Outcome)
	if outcome != "" && outcome != "cancel" {
		return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "invalid builtin permission outcome")
	}
	known := make(map[string]struct{}, len(info.Actions))
	for _, action := range info.Actions {
		known[action.ActionID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(d.Decisions))
	for _, decision := range d.Decisions {
		actionID := strings.TrimSpace(decision.ActionID)
		if actionID == "" || (decision.Outcome != "allow" && decision.Outcome != "deny") {
			return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "invalid builtin per-action permission decision")
		}
		if _, duplicate := seen[actionID]; duplicate {
			return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "duplicate builtin per-action permission decision")
		}
		if _, exists := known[actionID]; !exists {
			return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "builtin permission decision references an unknown action")
		}
		seen[actionID] = struct{}{}
	}
	return nil
}
func (b *BuiltinBackend) ExpireContinuation(_ context.Context, token string) error {
	continuation, ok := b.builtinContinuation(token, true)
	if !ok {
		return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
	}
	removed := false
	if host, ok := b.host.(interface{ ExpirePermission(string, string) bool }); ok {
		removed = host.ExpirePermission(continuation.agentID, continuation.requestID)
	}
	b.decidedMu.Lock()
	delete(b.decided, continuation.requestID)
	b.decidedMu.Unlock()
	if !removed {
		return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
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

func (b *BuiltinBackend) Capabilities(_ context.Context, a agentpkg.Agent) (agentruntime.Capabilities, error) {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeBuiltin); err != nil {
		return agentruntime.Capabilities{}, err
	}
	if b == nil || b.host == nil {
		return agentruntime.Capabilities{}, agentruntime.NewError(agentruntime.ErrorRuntimeNotExecutable, "builtin runtime is not executable")
	}
	return agentruntime.Capabilities{
		Executable: true, Turn: agentruntime.TurnCapabilities{Streaming: true},
		Sessions:     agentruntime.SessionCapabilities{Resume: true},
		Permissions:  agentruntime.PermissionCapabilities{Interactive: a.Runtime.Builtin.Permissions.Interactive(), ResumeMode: agentruntime.PermissionResumeNewStream},
		Cancellation: agentruntime.CancelCapabilities{Force: true, Graceful: true},
		Events:       []string{agentruntime.EventSession, agentruntime.EventDelta, agentruntime.EventContent, agentruntime.EventToolCall, agentruntime.EventUsage, agentruntime.EventPermission, agentruntime.EventDone, agentruntime.EventError},
	}, nil
}

func (b *BuiltinBackend) ServeTurn(ctx context.Context, a agentpkg.Agent, req agentruntime.TurnRequest, emit agentruntime.EventSink) error {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeBuiltin); err != nil {
		return err
	}
	if b == nil || b.host == nil {
		return agentruntime.NewError(agentruntime.ErrorBackendUnavailable, "builtin runtime is unavailable")
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
			return agentruntime.NewError(agentruntime.ErrorPermissionExpired, "permission continuation expired")
		}
		decided := entry.decision
		if !already {
			if err := b.permissions.Resolve(agentruntime.WithPermissionSource(ctx, "builtin_route"), a.ID, *req.Permission); err != nil {
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
				return agentruntime.NewError(agentruntime.ErrorPermissionExpired, "permission continuation expired")
			}
			decided = entry.decision
		}
		if !already {
			return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
		}
		req.Permission = &decided
	}
	var noOptions struct{}
	if err := agentruntime.DecodeRuntimeOptions(req.Options.Runtime, &noOptions); err != nil {
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
	terminalState, stopReason, suspended := agentruntime.RunStateCompleted, "", false
	if b.runs != nil {
		bindRun := b.runs.Begin
		if req.Permission != nil {
			bindRun = b.runs.Rebind
		}
		if err := bindRun(a.ID, a.Runtime.Type, req.RunID, req.SessionID, func(_ context.Context, mode agentruntime.CancelMode) error {
			c, ok := b.host.(interface {
				CancelRun(string, string, builtinhost.CancelMode) (bool, error)
			})
			if !ok {
				return agentruntime.NewError(agentruntime.ErrorCapabilityNotSupported, "builtin cancellation is not supported")
			}
			nativeMode := builtinhost.CancelModeForce
			if mode == agentruntime.CancelModeGraceful {
				nativeMode = builtinhost.CancelModeGraceful
			} else if mode != agentruntime.CancelModeForce {
				return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "invalid cancel mode")
			}
			drained := 0
			if b.permissions != nil {
				drained = b.permissions.DrainRun(agentruntime.WithPermissionSource(context.Background(), "run_cancel"), a.ID, req.RunID)
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
				return agentruntime.NewError(agentruntime.ErrorRunNotFound, "run not found")
			}
			return nil
		}); err != nil {
			if req.Permission != nil && errors.Is(err, agentruntime.ErrRunNotFound) {
				if host, ok := b.host.(interface{ ExpirePermission(string, string) bool }); ok {
					host.ExpirePermission(a.ID, req.Permission.RequestID)
				}
				if b.permissions != nil {
					b.permissions.RecordContinuationLost(agentruntime.WithPermissionSource(ctx, "builtin_resume"), a.ID, req.Permission.RequestID)
				}
				return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation is no longer available")
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
		if ev.Event == agentruntime.EventDone {
			stopReason = ev.StopReason
			suspended = ev.StopReason == agentruntime.StopReasonPermissionRequired
			if ev.StopReason == agentruntime.StopReasonCancelled {
				terminalState = agentruntime.RunStateCancelled
			}
		}
		if ev.Event == agentruntime.EventError {
			terminalState = agentruntime.RunStateFailed
		}
		if ev.Event == agentruntime.EventPermission && b.permissions != nil {
			actions := []agentruntime.PermissionAction{}
			var payload struct {
				Calls []struct {
					CallID string `json:"call_id"`
					Name   string `json:"name"`
				} `json:"calls"`
			}
			_ = json.Unmarshal(ev.Data, &payload)
			for _, call := range payload.Calls {
				actions = append(actions, agentruntime.PermissionAction{ActionID: call.CallID, Name: call.Name})
			}
			ttl := 10 * time.Minute
			if permissions := a.Runtime.Builtin.Permissions; permissions != nil && permissions.TimeoutSeconds > 0 {
				seconds := permissions.TimeoutSeconds
				ttl = time.Duration(seconds) * time.Second
			}
			token, tokenErr := agentruntime.NewContinuationToken()
			if tokenErr != nil {
				b.expireBuiltinRequests(a.ID, []string{ev.RequestID})
				return agentruntime.WrapError(agentruntime.ErrorTurnFailed, "generate permission continuation token", tokenErr)
			}
			b.storeBuiltinContinuation(token, builtinPermissionContinuation{agentID: a.ID, runID: req.RunID, requestID: ev.RequestID})
			_, regErr := b.permissions.Register(agentruntime.PendingPermission{RequestID: ev.RequestID, AgentID: a.ID, RuntimeType: a.Runtime.Type, RunID: req.RunID, SessionID: ev.SessionID, TTL: ttl, Actions: actions, ResumeMode: agentruntime.PermissionResumeNewStream}, token, b)
			if regErr != nil {
				b.builtinContinuation(token, true)
				b.expireBuiltinRequests(a.ID, []string{ev.RequestID})
				return regErr
			}
			registeredPermission = true
		}
		if emitErr := emit(commonBuiltinEvent(ev)); emitErr != nil {
			if registeredPermission {
				_ = b.permissions.Expire(agentruntime.WithPermissionSource(context.Background(), "permission_delivery_failed"), a.ID, ev.RequestID)
			}
			return emitErr
		}
		return nil
	})
	if err != nil && terminalState != agentruntime.RunStateCancelled {
		terminalState = agentruntime.RunStateFailed
	}
	return mapBuiltinError(err)
}

func acpPermissionActions(data json.RawMessage) []agentruntime.PermissionAction {
	var payload struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
		} `json:"toolCall"`
	}
	if json.Unmarshal(data, &payload) != nil || strings.TrimSpace(payload.ToolCall.ToolCallID) == "" {
		return nil
	}
	return []agentruntime.PermissionAction{{ActionID: payload.ToolCall.ToolCallID, Name: payload.ToolCall.Title}}
}

func acpPermissionOptions(data json.RawMessage) []agentruntime.PermissionOption {
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
	options := make([]agentruntime.PermissionOption, 0, len(payload.Options))
	for _, option := range payload.Options {
		id := strings.TrimSpace(option.OptionID)
		if id == "" {
			id = strings.TrimSpace(option.ID)
		}
		if id != "" {
			options = append(options, agentruntime.PermissionOption{OptionID: id, Kind: strings.TrimSpace(option.Kind), Name: strings.TrimSpace(option.Name)})
		}
	}
	return options
}

func (b *BuiltinBackend) CancelRun(ctx context.Context, a agentpkg.Agent, req agentruntime.CancelRequest) (agentruntime.CancelResult, error) {
	if req.Mode != agentruntime.CancelModeForce && req.Mode != agentruntime.CancelModeGraceful {
		return agentruntime.CancelResult{}, agentruntime.NewError(agentruntime.ErrorInvalidRequest, "invalid cancel mode")
	}
	if b.runs == nil {
		return agentruntime.CancelResult{}, agentruntime.NewError(agentruntime.ErrorCapabilityNotSupported, "run cancellation is not configured")
	}
	return b.runs.Cancel(ctx, a.ID, req)
}
func (b *BuiltinBackend) ResolvePermission(ctx context.Context, a agentpkg.Agent, decision agentruntime.PermissionDecision) error {
	if b.permissions == nil {
		return agentruntime.NewError(agentruntime.ErrorCapabilityNotSupported, "permission resolution is not configured")
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
func (b *BuiltinBackend) RuntimeSummary(_ context.Context, a agentpkg.Agent) (agentruntime.RuntimeSummary, error) {
	stateProvider, ok := b.host.(interface {
		State(string) builtinhost.EntryState
	})
	if !ok {
		return agentruntime.RuntimeSummary{}, agentruntime.NewError(agentruntime.ErrorRuntimeNotExecutable, "builtin runtime is unavailable")
	}
	state := stateProvider.State(a.ID)
	runtimeState := agentruntime.RuntimeStateReady
	if !state.Materialized {
		runtimeState = agentruntime.RuntimeStateUnknown
	}
	summary := agentruntime.RuntimeSummary{Type: a.Runtime.Type, Executable: true, Healthy: true, State: runtimeState, ActiveRuns: state.InflightTurns, SessionCount: state.LiveSessions}
	summary.Details, _ = json.Marshal(state)
	if b.permissions != nil {
		summary.PendingPermissions = len(b.permissions.List(a.ID))
	}
	if !state.MaterializedAt.IsZero() {
		summary.LastActivityAt = &state.MaterializedAt
	}
	return summary, nil
}
func (b *BuiltinBackend) Health(_ context.Context, a agentpkg.Agent) (agentruntime.Health, error) {
	if err := validateBackendAgent(a, agentpkg.RuntimeTypeBuiltin); err != nil {
		return agentruntime.Health{Healthy: false, State: agentruntime.RuntimeStateUnhealthy, CheckedAt: time.Now().UTC()}, err
	}
	return agentruntime.Health{Healthy: true, State: agentruntime.RuntimeStateReady, CheckedAt: time.Now().UTC()}, nil
}

func (b *BuiltinBackend) LoadContinuationCursor(_ context.Context, a agentpkg.Agent, requestID string) (agentruntime.EventCursor, error) {
	cursor, ok := b.host.LoadContinuationCursor(a.ID, strings.TrimSpace(requestID))
	if !ok {
		return agentruntime.EventCursor{}, agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
	}
	return agentruntime.EventCursor{RunID: cursor.RunID, NextSequence: cursor.NextSequence, NextSegment: cursor.NextSegment}, nil
}

func (b *BuiltinBackend) StoreContinuationCursor(_ context.Context, a agentpkg.Agent, requestID string, cursor agentruntime.EventCursor) error {
	if strings.TrimSpace(requestID) == "" || !agentruntime.ValidRunID(cursor.RunID) {
		return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "invalid permission continuation cursor")
	}
	stored := b.host.StoreContinuationCursor(a.ID, strings.TrimSpace(requestID), builtinhost.ContinuationCursor{
		RunID: cursor.RunID, NextSequence: cursor.NextSequence, NextSegment: cursor.NextSegment,
	})
	if !stored {
		return agentruntime.NewError(agentruntime.ErrorPermissionNotFound, "permission continuation not found")
	}
	return nil
}

func validateBackendAgent(a agentpkg.Agent, runtimeType string) error {
	if strings.TrimSpace(a.ID) == "" {
		return agentruntime.NewError(agentruntime.ErrorAgentNotFound, "agent not found")
	}
	if a.Disabled {
		return agentruntime.NewError(agentruntime.ErrorAgentDisabled, "agent is disabled")
	}
	if a.Runtime.Type != runtimeType {
		return agentruntime.NewError(agentruntime.ErrorRuntimeNotExecutable, "agent runtime does not match backend")
	}
	if runtimeType == agentpkg.RuntimeTypeACP && a.Runtime.ACP == nil {
		return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "runtime.acp is required")
	}
	if runtimeType == agentpkg.RuntimeTypeBuiltin && a.Runtime.Builtin == nil {
		return agentruntime.NewError(agentruntime.ErrorInvalidRequest, "runtime.builtin is required")
	}
	return nil
}

func validateRuntimeOptionsVersion(options agentruntime.TurnOptions) error {
	if options.Version != "" && options.Version != agentruntime.TurnOptionsVersionV1 {
		return agentruntime.NewError(agentruntime.ErrorUnsupportedOption, "unsupported runtime options version")
	}
	return nil
}

func bridgeRuntimeIdentities(ctx context.Context) context.Context {
	ids, _ := agentruntime.IdentitiesFromContext(ctx)
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
	ctx = agentruntime.WithIdentities(ctx, ids)
	return usage.ContextWithDimensions(ctx, dims)
}

func commonACPEvent(ev acphost.TurnEvent) agentruntime.TurnEvent {
	data := ev.Data
	if len(data) == 0 && (ev.StopReason != "" || ev.Message != "") {
		data, _ = json.Marshal(struct {
			StopReason string `json:"stop_reason,omitempty"`
			Message    string `json:"message,omitempty"`
		}{ev.StopReason, ev.Message})
	}
	return agentruntime.TurnEvent{Event: ev.Event, SessionID: ev.SessionID, RequestID: ev.RequestID, Text: ev.Text, Data: data}
}

func commonBuiltinEvent(ev builtinhost.TurnEvent) agentruntime.TurnEvent {
	data := ev.Data
	if len(data) == 0 && (ev.StopReason != "" || ev.Message != "") {
		data, _ = json.Marshal(struct {
			StopReason string `json:"stop_reason,omitempty"`
			Message    string `json:"message,omitempty"`
		}{ev.StopReason, ev.Message})
	}
	return agentruntime.TurnEvent{Event: ev.Event, RunID: ev.RunID, SessionID: ev.SessionID, RequestID: ev.RequestID, Text: ev.Text, Data: data}
}

func mapACPError(err error) error {
	if err == nil || agentruntime.IsNormalized(err) {
		return err
	}
	switch {
	case errors.Is(err, acphost.ErrInvalidRequest):
		return agentruntime.WrapError(agentruntime.ErrorInvalidRequest, "invalid acp request", err)
	case errors.Is(err, acphost.ErrCapacityExceeded):
		return agentruntime.WrapError(agentruntime.ErrorTurnLimitExceeded, "acp runtime capacity exceeded", err)
	case errors.Is(err, acphost.ErrTurnCancelled):
		return agentruntime.WrapError(agentruntime.ErrorTurnCancelled, "acp turn cancelled", err)
	case errors.Is(err, acphost.ErrRuntimeConfigRetired):
		return agentruntime.WrapError(agentruntime.ErrorBackendUnavailable, "acp runtime config was retired", err)
	case strings.Contains(err.Error(), "does not advertise session/list"), strings.Contains(err.Error(), "does not advertise session/load"):
		return agentruntime.WrapError(agentruntime.ErrorCapabilityNotSupported, "acp capability is not supported", err)
	default:
		return agentruntime.NormalizeError(err)
	}
}

func mapACPCancelError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, acphost.ErrRunNotFound) {
		return agentruntime.NewError(agentruntime.ErrorRunNotFound, "run not found")
	}
	if errors.Is(err, acphost.ErrRunNotReady) {
		return agentruntime.NewError(agentruntime.ErrorBackendUnavailable, "run cancellation is not ready; retry")
	}
	return mapACPError(err)
}

func mapBuiltinError(err error) error {
	if err == nil || agentruntime.IsNormalized(err) {
		return err
	}
	switch {
	case errors.Is(err, builtinhost.ErrAgentNotFound):
		return agentruntime.WrapError(agentruntime.ErrorAgentNotFound, "builtin agent not found", err)
	case errors.Is(err, builtinhost.ErrTurnLimitExceeded), errors.Is(err, builtinhost.ErrPermissionCapacity):
		return agentruntime.WrapError(agentruntime.ErrorTurnLimitExceeded, "builtin turn limit exceeded", err)
	case errors.Is(err, builtinhost.ErrSessionBusy):
		return agentruntime.WrapError(agentruntime.ErrorSessionBusy, "builtin session is busy", err)
	case errors.Is(err, builtinhost.ErrSessionLimitExceeded):
		return agentruntime.WrapError(agentruntime.ErrorSessionLimitExceeded, "builtin session limit exceeded", err)
	case errors.Is(err, builtinhost.ErrInvalidRequest):
		return agentruntime.WrapError(agentruntime.ErrorInvalidRequest, "invalid builtin request", err)
	default:
		return agentruntime.NormalizeError(err)
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

func cloneCodexConfig(in *hostconfig.CodexConfig) *hostconfig.CodexConfig {
	if in == nil {
		return nil
	}
	out := *in
	out.AdapterArgs = append([]string(nil), in.AdapterArgs...)
	out.AppServerArgs = append([]string(nil), in.AppServerArgs...)
	return &out
}

var _ agentruntime.Backend = (*ACPBackend)(nil)
var _ agentruntime.Backend = (*BuiltinBackend)(nil)
var _ agentruntime.ContinuationCursorBackend = (*BuiltinBackend)(nil)
