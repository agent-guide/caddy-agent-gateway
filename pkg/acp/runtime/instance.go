package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	baseacp "github.com/agent-guide/agent-gateway/pkg/acp"
	"github.com/agent-guide/agent-gateway/pkg/acp/agentspi"
	"github.com/agent-guide/agent-gateway/pkg/acp/runtime/acpupdate"
	"github.com/agent-guide/agent-gateway/pkg/acp/runtimeconfig"
	acptransport "github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

type instance struct {
	cfg   runtimeconfig.Config
	cwd   string
	model string
	agent agentspi.Agent
	t     acptransport.Transport

	// sessionID is the host-bound id (session events, scope adoption,
	// permission events); rawSessionID is the protocol id used on the wire
	// (session/prompt, session/cancel, session/set_config_option). They differ
	// only for agents implementing StableSessionResolver/SessionLoadResolver;
	// stablePending marks a deferred stable-id resolution that is re-attempted
	// after each prompt.
	sessionID     string
	rawSessionID  string
	stablePending bool

	meta      sessionMetaCache
	metaUnsub func()

	// permissions is the manager-level broker for interactive permission
	// requests; turnEmit is the active turn's event sink (set for the duration
	// of a prompt so the permission handler can reach the turn client).
	permissions *permissionBroker
	emitMu      sync.Mutex
	turnEmit    EventSink
}

func (i *instance) setTurnSink(emit EventSink) {
	i.emitMu.Lock()
	i.turnEmit = emit
	i.emitMu.Unlock()
}

func (i *instance) turnSink() EventSink {
	i.emitMu.Lock()
	defer i.emitMu.Unlock()
	return i.turnEmit
}

// sessionMetaCache keeps the latest structured session state pushed by the
// agent (config options, slash commands, title, mode, usage) so a turn that
// joins an already-pooled instance can be told the current state up front,
// and so operators can inspect pooled sessions through the runtime Admin API.
// Each entry stores the raw ACP update object of its kind.
type sessionMetaCache struct {
	mu            sync.Mutex
	configOptions json.RawMessage
	commands      json.RawMessage
	sessionInfo   json.RawMessage
	mode          json.RawMessage
	usage         json.RawMessage
}

func (c *sessionMetaCache) store(kind acpupdate.Kind, data json.RawMessage) {
	if len(data) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch kind {
	case acpupdate.KindConfigOptions:
		c.configOptions = data
	case acpupdate.KindCommands:
		c.commands = data
	case acpupdate.KindSessionInfo:
		c.sessionInfo = data
	case acpupdate.KindMode:
		c.mode = data
	case acpupdate.KindUsage:
		c.usage = data
	}
}

func (c *sessionMetaCache) snapshot() SessionMetadata {
	c.mu.Lock()
	defer c.mu.Unlock()
	return SessionMetadata{
		ConfigOptions:     c.configOptions,
		AvailableCommands: c.commands,
		SessionInfo:       c.sessionInfo,
		Mode:              c.mode,
		Usage:             c.usage,
	}
}

// turnStartEvents builds the cached-state events replayed at the start of each
// turn, in a stable order. Live updates during the turn supersede them; a
// rare duplicate (an update racing the turn start) is harmless because every
// entry is a full snapshot of its kind.
func (c *sessionMetaCache) turnStartEvents() []TurnEvent {
	snap := c.snapshot()
	var events []TurnEvent
	for _, entry := range []struct {
		event string
		data  json.RawMessage
	}{
		{string(acpupdate.KindConfigOptions), snap.ConfigOptions},
		{string(acpupdate.KindCommands), snap.AvailableCommands},
		{string(acpupdate.KindSessionInfo), snap.SessionInfo},
		{string(acpupdate.KindMode), snap.Mode},
		{string(acpupdate.KindUsage), snap.Usage},
	} {
		if len(entry.data) > 0 {
			events = append(events, TurnEvent{Event: entry.event, Data: entry.data})
		}
	}
	return events
}

func newInstance(ctx context.Context, cfg runtimeconfig.Config, req TurnRequest, permissions *permissionBroker, observers ...func(string, json.RawMessage)) (*instance, error) {
	cwd := strings.TrimSpace(req.CWD)
	if cwd == "" {
		cwd = cfg.CWD
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = cfg.DefaultModel
	}
	agent, err := agentspi.New(cfg.AgentType, agentspi.OpenRequest{Config: cfg, CWD: cwd})
	if err != nil {
		return nil, err
	}
	inst := &instance{cfg: cfg, cwd: cwd, model: model, sessionID: strings.TrimSpace(req.SessionID), agent: agent, permissions: permissions}
	t, err := agent.Open(ctx, acptransport.Handlers{Permission: inst.permissionFunc()})
	if err != nil {
		return nil, err
	}
	inst.t = t
	if len(observers) > 0 && observers[0] != nil {
		inst.t = observedTransport{Transport: t, notify: observers[0]}
	}
	// Subscribe for the whole instance lifetime so state pushed outside a turn
	// is not lost — the real opencode binary pushes available_commands_update
	// right after session/new, before any prompt.
	metaCh, metaUnsub := t.Updates(64)
	inst.metaUnsub = metaUnsub
	go inst.consumeMetadata(metaCh)
	// Bound the setup handshake so a wedged agent does not hang the turn until
	// the client disconnects. Streaming (prompt) is intentionally not bounded.
	setupCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()
	if err := inst.initialize(setupCtx, req.FreshSession); err != nil {
		_ = inst.close()
		return nil, err
	}
	return inst, nil
}

// observedTransport is a narrow wire tap used by native-agent acceptance
// tests. The observer runs only after the JSON-RPC notification was written to
// the real transport, so tests can assert the session/cancel frame itself.
type observedTransport struct {
	acptransport.Transport
	notify func(string, json.RawMessage)
}

func (t observedTransport) Notify(method string, params any) error {
	if err := t.Transport.Notify(method, params); err != nil {
		return err
	}
	raw, _ := json.Marshal(params)
	t.notify(method, raw)
	return nil
}

// consumeMetadata feeds the session metadata cache from a dedicated updates
// subscription. It exits when the subscription channel is closed (instance
// close unsubscribes).
func (i *instance) consumeMetadata(updates <-chan acptransport.Message) {
	for msg := range updates {
		if msg.Method != "session/update" {
			continue
		}
		for _, ev := range acpupdate.Parse(msg.Params) {
			i.meta.store(ev.Kind, ev.Data)
		}
	}
}

func (i *instance) initialize(ctx context.Context, fresh bool) error {
	if _, err := i.t.Request(ctx, "initialize", i.agent.InitializeParams()); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if i.sessionID != "" && !fresh {
		loadID := i.sessionID
		boundID := i.sessionID
		if resolver, ok := i.agent.(agentspi.SessionLoadResolver); ok {
			resolvedLoadID, resolvedBoundID, err := resolver.ResolveLoadSessionID(ctx, i.t, i.cwd, i.sessionID)
			if err != nil {
				return fmt.Errorf("resolve load session id: %w", err)
			}
			if strings.TrimSpace(resolvedLoadID) != "" {
				loadID = strings.TrimSpace(resolvedLoadID)
			}
			if strings.TrimSpace(resolvedBoundID) != "" {
				boundID = strings.TrimSpace(resolvedBoundID)
			}
		}
		if _, err := i.t.Request(ctx, "session/load", i.agent.SessionLoadParams(loadID)); err != nil {
			return fmt.Errorf("session/load: %w", err)
		}
		i.rawSessionID = loadID
		i.sessionID = boundID
		return nil
	}
	result, err := i.t.Request(ctx, "session/new", i.agent.SessionNewParams(i.model))
	if err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	raw := parseSessionID(result)
	if raw == "" {
		return fmt.Errorf("session/new returned empty sessionId")
	}
	i.rawSessionID = raw
	i.sessionID = raw
	if resolver, ok := i.agent.(agentspi.StableSessionResolver); ok {
		bound, err := resolver.ResolveBoundSessionID(ctx, i.t, i.cwd, raw)
		if err != nil {
			return fmt.Errorf("resolve bound session id: %w", err)
		}
		if bound = strings.TrimSpace(bound); bound != "" {
			i.sessionID = bound
		} else {
			// Not resolvable yet (the session settles with the first prompt);
			// re-resolve after each prompt and emit a late session event.
			i.stablePending = true
		}
	}
	i.cacheNewSessionConfigOptions(result)
	if err := i.applySessionModel(ctx); err != nil {
		return err
	}
	return i.applyConfigOverrides(ctx, i.cfg.ConfigOverrides)
}

// protocolSessionID is the id used on the wire for the live session. It falls
// back to the host-bound id for instances constructed without a raw id.
func (i *instance) protocolSessionID() string {
	if i.rawSessionID != "" {
		return i.rawSessionID
	}
	return i.sessionID
}

// resolveStableSessionID re-attempts a deferred stable-id resolution after a
// completed prompt. On success it rebinds the host-visible id and emits a late
// session event so the client addresses follow-up turns by the stable id; on
// error or an empty id it stays pending for the next turn.
func (i *instance) resolveStableSessionID(ctx context.Context, emit EventSink) {
	if !i.stablePending {
		return
	}
	resolver, ok := i.agent.(agentspi.StableSessionResolver)
	if !ok {
		i.stablePending = false
		return
	}
	bound, err := resolver.ResolveBoundSessionID(ctx, i.t, i.cwd, i.rawSessionID)
	if err != nil || strings.TrimSpace(bound) == "" {
		return
	}
	i.stablePending = false
	bound = strings.TrimSpace(bound)
	if bound == i.sessionID {
		return
	}
	i.sessionID = bound
	if emit != nil {
		_ = emit(TurnEvent{Event: "session", SessionID: bound})
	}
}

// cacheNewSessionConfigOptions seeds the config-options cache from the
// session/new result (the real opencode binary returns the full configOptions
// list there, including currentValue). The cached object is synthesized in the
// config_option_update shape so turn-start replay events match live ones.
func (i *instance) cacheNewSessionConfigOptions(result json.RawMessage) {
	var payload struct {
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if err := json.Unmarshal(result, &payload); err != nil || len(payload.ConfigOptions) == 0 {
		return
	}
	synthesized, err := json.Marshal(map[string]json.RawMessage{
		"sessionUpdate": json.RawMessage(`"config_option_update"`),
		"configOptions": payload.ConfigOptions,
	})
	if err != nil {
		return
	}
	i.meta.store(acpupdate.KindConfigOptions, synthesized)
}

func listAgentSessions(ctx context.Context, cfg runtimeconfig.Config, req ListSessionsRequest) (ListSessionsResponse, error) {
	openCWD := strings.TrimSpace(req.CWD)
	if openCWD == "" {
		openCWD = cfg.CWD
	}
	agent, err := agentspi.New(cfg.AgentType, agentspi.OpenRequest{Config: cfg, CWD: openCWD})
	if err != nil {
		return ListSessionsResponse{}, err
	}
	lister, ok := agent.(agentspi.SessionLister)
	if !ok {
		return ListSessionsResponse{}, fmt.Errorf("acp agent %q does not implement session/list", agent.Name())
	}
	t, err := agent.Open(ctx, acptransport.Handlers{Permission: permissionHandler(cfg.PermissionMode)})
	if err != nil {
		return ListSessionsResponse{}, err
	}
	defer func() { _ = t.Close() }()

	setupCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()
	initResult, err := t.Request(setupCtx, "initialize", agent.InitializeParams())
	if err != nil {
		return ListSessionsResponse{}, fmt.Errorf("initialize: %w", err)
	}
	if !supportsSessionList(initResult) {
		return ListSessionsResponse{}, fmt.Errorf("acp agent %q does not advertise session/list", agent.Name())
	}
	// The cwd filter is applied gateway-side instead of being forwarded to the
	// agent: stored-cwd shapes differ per agent (the real opencode binary
	// stores canonical symlink-resolved cwds, the real codex-acp adapter stores
	// the session/new cwd verbatim), so an agent-side exact match silently
	// drops sessions for one agent or the other. Comparing both sides through
	// canonicalCWD is agent-agnostic.
	raw, err := t.Request(ctx, "session/list", lister.SessionListParams("", req.Cursor))
	if err != nil {
		return ListSessionsResponse{}, fmt.Errorf("session/list: %w", err)
	}
	out, err := parseListSessionsResponse(raw)
	if err != nil {
		return ListSessionsResponse{}, err
	}
	if filter := canonicalCWD(req.CWD); filter != "" {
		kept := make([]SessionInfo, 0, len(out.Sessions))
		for _, session := range out.Sessions {
			if canonicalCWD(session.CWD) == filter {
				kept = append(kept, session)
			}
		}
		out.Sessions = kept
	}
	if out.Sessions == nil {
		out.Sessions = []SessionInfo{}
	}
	return out, nil
}

// canonicalCWD resolves symlinks in a cwd so both sides of the gateway-side
// session/list filter compare in canonical form (macOS /tmp -> /private/tmp).
// Unresolvable paths are passed through unchanged.
func canonicalCWD(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		return resolved
	}
	return cwd
}

func supportsSessionList(raw json.RawMessage) bool {
	var payload struct {
		AgentCapabilities struct {
			SessionCapabilities map[string]json.RawMessage `json:"sessionCapabilities"`
			ListSessions        json.RawMessage            `json:"listSessions"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	if _, ok := payload.AgentCapabilities.SessionCapabilities["list"]; ok {
		return true
	}
	return len(payload.AgentCapabilities.ListSessions) > 0
}

func parseListSessionsResponse(raw json.RawMessage) (ListSessionsResponse, error) {
	var payload struct {
		Sessions []struct {
			SessionID string          `json:"sessionId"`
			CWD       string          `json:"cwd"`
			Title     string          `json:"title"`
			UpdatedAt string          `json:"updatedAt"`
			Meta      json.RawMessage `json:"_meta"`
		} `json:"sessions"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ListSessionsResponse{}, fmt.Errorf("decode session/list response: %w", err)
	}
	out := ListSessionsResponse{NextCursor: strings.TrimSpace(payload.NextCursor)}
	out.Sessions = make([]SessionInfo, 0, len(payload.Sessions))
	for _, item := range payload.Sessions {
		sessionID := strings.TrimSpace(item.SessionID)
		cwd := strings.TrimSpace(item.CWD)
		if sessionID == "" || cwd == "" {
			return ListSessionsResponse{}, fmt.Errorf("session/list returned a session without sessionId or cwd")
		}
		info := SessionInfo{
			SessionID: sessionID,
			CWD:       cwd,
			Title:     item.Title,
			Meta:      item.Meta,
		}
		if ts := strings.TrimSpace(item.UpdatedAt); ts != "" {
			updatedAt, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return ListSessionsResponse{}, fmt.Errorf("session/list returned invalid updatedAt for %q: %w", sessionID, err)
			}
			info.UpdatedAt = &updatedAt
		}
		out.Sessions = append(out.Sessions, info)
	}
	return out, nil
}

// applyConfigOverrides replays config options via session/set_config_option
// (`{sessionId, configId, value}`, verified against the ACP v1 schema and the
// real opencode binary). A rejected option fails the setup/turn rather than
// being silently dropped.
func (i *instance) applyConfigOverrides(ctx context.Context, overrides map[string]string) error {
	for id, value := range overrides {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := i.t.Request(ctx, "session/set_config_option", map[string]any{
			"sessionId": i.protocolSessionID(),
			"configId":  id,
			"value":     value,
		}); err != nil {
			return fmt.Errorf("set_config_option %q: %w", id, err)
		}
	}
	return nil
}

// applySessionModel applies the configured model to a freshly created session.
// ACP defines no standard model-selection method, so the model is applied only
// when the agent declares how through SessionModelSelector; otherwise the
// configured model is left to the agent/adapter default rather than being
// smuggled into session/new as a non-standard field.
func (i *instance) applySessionModel(ctx context.Context) error {
	if i.model == "" {
		return nil
	}
	selector, ok := i.agent.(agentspi.SessionModelSelector)
	if !ok {
		return nil
	}
	if _, err := selector.SelectSessionModel(ctx, i.t, i.protocolSessionID(), i.model, nil); err != nil {
		return fmt.Errorf("select session model: %w", err)
	}
	return nil
}

// terminalDrainTimeout bounds how long the driver waits for a terminal update
// after the prompt result, for agents that signal completion out of band.
const terminalDrainTimeout = 2 * time.Second

// postResultIdleGrace is how long the post-result drain waits for further
// updates before concluding the turn, for agents without a terminal-update
// signal. The real opencode binary races the session/prompt response against
// the final agent_message_chunk updates (observed live: the reply chunks can
// arrive after the result), so returning on the result with only a buffered
// drain drops the reply tail. It is a var so tests can shorten it.
var postResultIdleGrace = 250 * time.Millisecond

// initializeTimeout bounds the setup handshake (initialize + session/new|load +
// model selection). It is a var so tests can shorten it.
var initializeTimeout = 30 * time.Second

func (i *instance) prompt(ctx context.Context, req TurnRequest, emit EventSink) (string, error) {
	// Serialize sink writes (the interactive permission handler emits from the
	// transport goroutine) and register the sink for the duration of the turn
	// so permission requests can reach this turn's client.
	emit = lockedEventSink(emit)
	i.setTurnSink(emit)
	defer i.setTurnSink(nil)
	if emit != nil && i.sessionID != "" {
		if err := emit(TurnEvent{Event: "session", SessionID: i.sessionID}); err != nil {
			return "", err
		}
	}
	// Replay the cached session state (config options, slash commands, title,
	// mode, usage) so a client joining a pooled instance sees the current state
	// even though the originating updates were delivered during an earlier turn
	// or during setup.
	if emit != nil {
		for _, ev := range i.meta.turnStartEvents() {
			if err := emit(ev); err != nil {
				return "", err
			}
		}
	}
	// Per-turn config overrides apply to the (shared) session before the prompt.
	if err := i.applyConfigOverrides(ctx, req.ConfigOverrides); err != nil {
		return "", err
	}
	updates, unsubscribe := i.t.Updates(256)
	defer unsubscribe()

	done := make(chan promptResult, 1)
	go func() {
		raw, err := i.t.Request(ctx, "session/prompt", i.agent.PromptParams(i.protocolSessionID(), req.Input, firstNonEmpty(strings.TrimSpace(req.Model), i.model)))
		done <- promptResult{stopReason: parseStopReason(raw), err: err}
	}()

	emitUpdate := func(msg acptransport.Message) error {
		if emit == nil || msg.Method != "session/update" {
			return nil
		}
		for _, ev := range acpupdate.Parse(msg.Params) {
			if ev.Kind == acpupdate.KindReasoning && !i.acceptReasoning(msg.Params) {
				continue
			}
			if err := emit(TurnEvent{Event: string(ev.Kind), Text: ev.Text, Data: ev.Data}); err != nil {
				return err
			}
		}
		return nil
	}

	terminalDetector, hasTerminal := i.agent.(agentspi.TerminalUpdateDetector)
	stopReason := "end_turn"
	resultReceived := false
	var drainTimer <-chan time.Time
	var idleTimer <-chan time.Time

	// finish completes a successful turn: re-attempt a deferred stable-id
	// resolution (StableSessionResolver) so the client learns the settled id
	// via a late session event before the done event.
	finish := func() (string, error) {
		i.resolveStableSessionID(ctx, emit)
		return stopReason, nil
	}

	for {
		select {
		case <-ctx.Done():
			i.cancel()
			return "cancelled", nil
		case res := <-done:
			if res.err != nil {
				return "end_turn", fmt.Errorf("session/prompt: %w", res.err)
			}
			if res.stopReason != "" {
				stopReason = res.stopReason
			}
			resultReceived = true
			// The turn's updates are expected to precede the session/prompt
			// result, but agents race the two in practice (observed live: the
			// real opencode binary can deliver the final agent_message_chunk
			// updates after the result). Keep draining until a terminal update
			// (TerminalUpdateDetector agents) or until the stream has been
			// quiet for a short grace period, capped by terminalDrainTimeout,
			// so the tail of the response is never dropped.
			drainTimer = time.After(terminalDrainTimeout)
			if !hasTerminal {
				idleTimer = time.After(postResultIdleGrace)
			}
		case msg, ok := <-updates:
			if !ok {
				return finish()
			}
			if err := emitUpdate(msg); err != nil {
				i.cancel()
				return "cancelled", err
			}
			if resultReceived {
				if hasTerminal && terminalDetector.IsTerminalUpdate(msg.Params) {
					return finish()
				}
				if !hasTerminal {
					idleTimer = time.After(postResultIdleGrace)
				}
			}
		case <-idleTimer:
			return finish()
		case <-drainTimer:
			return finish()
		}
	}
}

type promptResult struct {
	stopReason string
	err        error
}

// drainBuffered emits every update already queued on the channel without
// blocking, stopping as soon as the channel is empty or closed.
func drainBuffered(updates <-chan acptransport.Message, emit func(acptransport.Message) error) error {
	for {
		select {
		case msg, ok := <-updates:
			if !ok {
				return nil
			}
			if err := emit(msg); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func parseStopReason(raw json.RawMessage) string {
	var payload struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.StopReason)
}

func (i *instance) cancel() error {
	if i == nil || i.t == nil || i.protocolSessionID() == "" {
		return ErrRunNotReady
	}
	return i.agent.Cancel(context.Background(), i.t, i.protocolSessionID())
}

func (i *instance) close() error {
	if i == nil || i.t == nil {
		return nil
	}
	// End the metadata subscription first so its consumer goroutine exits.
	if i.metaUnsub != nil {
		i.metaUnsub()
	}
	return i.t.Close()
}

func (i *instance) alive() bool {
	return i != nil && i.t != nil && i.t.Alive()
}

func parseSessionID(raw json.RawMessage) string {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	if id, ok := payload["sessionId"].(string); ok {
		return strings.TrimSpace(id)
	}
	if nested, ok := payload["session"].(map[string]any); ok {
		if id, ok := nested["sessionId"].(string); ok {
			return strings.TrimSpace(id)
		}
		if id, ok := nested["id"].(string); ok {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

// acceptReasoning lets an agent suppress agent_thought_chunk updates that are
// not genuine reasoning (ReasoningUpdateFilter). Without the capability, every
// reasoning update is forwarded.
func (i *instance) acceptReasoning(params json.RawMessage) bool {
	if filter, ok := i.agent.(agentspi.ReasoningUpdateFilter); ok {
		return filter.AcceptReasoningUpdate(params)
	}
	return true
}

// permissionHandler is the configured-decision handler used by transient
// connections (session/list, transcript replay), which have no turn client to
// ask: interactive degrades to fail-closed deny there.
func permissionHandler(mode string) func(context.Context, json.RawMessage) acptransport.PermissionResponse {
	return func(_ context.Context, params json.RawMessage) acptransport.PermissionResponse {
		// Fail closed unless the service explicitly opts into auto-approval.
		if mode != baseacp.PermissionModeAutoApprove {
			return acptransport.PermissionResponse{Outcome: acptransport.PermissionOutcomeCancelled}
		}
		if id := acptransport.AllowOptionID(params); id != "" {
			return acptransport.PermissionResponse{Outcome: acptransport.PermissionOutcomeSelected, SelectedOptionID: id}
		}
		return acptransport.PermissionResponse{Outcome: acptransport.PermissionOutcomeCancelled}
	}
}

// permissionFunc builds the pooled instance's permission handler. deny and
// auto_approve are configured decisions; interactive forwards the request to
// the active turn client as a "permission" SSE event and waits for the
// decision endpoint to resolve it, failing closed on timeout (the transport's
// permissionTimeout context), client error, or when no turn is streaming.
func (i *instance) permissionFunc() func(context.Context, json.RawMessage) acptransport.PermissionResponse {
	configured := permissionHandler(i.cfg.PermissionMode)
	if i.cfg.PermissionMode != baseacp.PermissionModeInteractive {
		return configured
	}
	return func(ctx context.Context, params json.RawMessage) acptransport.PermissionResponse {
		return i.interactivePermission(ctx, params)
	}
}

func (i *instance) interactivePermission(ctx context.Context, params json.RawMessage) acptransport.PermissionResponse {
	cancelled := acptransport.PermissionResponse{Outcome: acptransport.PermissionOutcomeCancelled}
	emit := i.turnSink()
	if emit == nil || i.permissions == nil {
		// No streaming turn client to ask — fail closed.
		return cancelled
	}
	pending, err := i.permissions.create(i.cfg.OwnerID, i.sessionID, params)
	if err != nil {
		return cancelled
	}
	defer i.permissions.remove(pending.info.RequestID)
	expiresAt, _ := ctx.Deadline()
	if err := emit(TurnEvent{Event: "permission", RequestID: pending.info.RequestID, SessionID: i.sessionID, PermissionExpiresAt: expiresAt, Data: params}); err != nil {
		return cancelled
	}
	select {
	case resp := <-pending.decision:
		return resp
	case <-ctx.Done():
		// The transport's fail-closed permission timeout (or teardown) fired
		// before the client answered.
		return cancelled
	}
}

// lockedEventSink serializes writes to a turn's event sink: during an
// interactive permission wait the permission handler emits from the transport
// goroutine concurrently with the prompt loop.
func lockedEventSink(emit EventSink) EventSink {
	if emit == nil {
		return nil
	}
	var mu sync.Mutex
	return func(ev TurnEvent) error {
		mu.Lock()
		defer mu.Unlock()
		return emit(ev)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
