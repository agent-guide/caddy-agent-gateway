package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	// Register the real agents so the runtime can resolve them by type.
	baseacp "github.com/agent-guide/agent-gateway/pkg/acp"
	_ "github.com/agent-guide/agent-gateway/pkg/acp/agent/codex"
	_ "github.com/agent-guide/agent-gateway/pkg/acp/agent/opencode"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
)

// These tests exercise a real ACP agent binary. They are opt-in: set
// AGW_ACP_SMOKE=1 and have the agent binary on PATH. They only run the
// initialize + session/new handshake, which does not require model credentials
// or network access, so they prove real wire-protocol interop without hitting
// an LLM. Run with:
//
//	AGW_ACP_SMOKE=1 go test ./pkg/acp/runtime -run Smoke -v

func requireSmoke(t *testing.T, bin string) {
	t.Helper()
	if os.Getenv("AGW_ACP_SMOKE") == "" {
		t.Skip("set AGW_ACP_SMOKE=1 to run real-agent smoke tests")
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%q not on PATH: %v", bin, err)
	}
}

func smokeHandshake(t *testing.T, cfg acpservice.ServiceConfig) {
	t.Helper()
	cfg.Normalize()
	m := newTestManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	inst, err := m.resolveInstance(ctx, "smoke", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("handshake (initialize + session/new): %v", err)
	}
	if inst.sessionID == "" {
		t.Fatal("agent returned an empty session id")
	}
	t.Logf("%s session id: %s", cfg.AgentType, inst.sessionID)
}

func TestSmokeOpencodeHandshake(t *testing.T) {
	requireSmoke(t, "opencode")
	cwd := t.TempDir()
	smokeHandshake(t, acpservice.ServiceConfig{
		ID:           "opencode-smoke",
		Name:         "opencode",
		AgentType:    baseacp.AgentTypeOpencode,
		CWD:          cwd,
		AllowedRoots: []string{cwd},
	})
}

// TestSmokeOpencodeSessionLifecycle creates a real session, then verifies the
// transient-connection paths against the real binary: session/list must report
// the new session, transcript replay via session/load must succeed (empty: no
// prompt has run), and the metadata cache must capture the config options from
// session/new plus the available_commands_update opencode pushes after it.
func TestSmokeOpencodeSessionLifecycle(t *testing.T) {
	requireSmoke(t, "opencode")
	cwd := t.TempDir()
	cfg := acpservice.ServiceConfig{
		ID:           "opencode-smoke",
		Name:         "opencode",
		AgentType:    baseacp.AgentTypeOpencode,
		CWD:          cwd,
		AllowedRoots: []string{cwd},
	}
	cfg.Normalize()
	m := newTestManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	inst, err := m.resolveInstance(ctx, "smoke", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}

	// Metadata cache: session/new config options are seeded synchronously; the
	// available_commands_update push arrives asynchronously right after.
	if snap := inst.meta.snapshot(); len(snap.ConfigOptions) == 0 {
		t.Fatal("config options from session/new were not cached")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if snap := inst.meta.snapshot(); len(snap.AvailableCommands) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("available_commands_update pushed after session/new was not cached")
		}
		time.Sleep(100 * time.Millisecond)
	}

	list, err := listAgentSessions(ctx, cfg, ListSessionsRequest{CWD: cwd})
	if err != nil {
		t.Fatalf("session/list: %v", err)
	}
	found := false
	for _, session := range list.Sessions {
		if session.SessionID == inst.sessionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("session/list did not include the new session %s (got %d sessions)", inst.sessionID, len(list.Sessions))
	}

	transcript, err := loadAgentTranscript(ctx, cfg, TranscriptRequest{SessionID: inst.sessionID, CWD: cwd})
	if err != nil {
		t.Fatalf("transcript replay: %v", err)
	}
	t.Logf("opencode session %s: %d listed sessions, %d transcript messages, commands cached",
		inst.sessionID, len(list.Sessions), len(transcript.Messages))
}

func TestSmokeCodexSessionLifecycle(t *testing.T) {
	requireSmoke(t, "codex-acp")
	cwd := t.TempDir()
	cfg := acpservice.ServiceConfig{
		ID:           "codex-smoke",
		Name:         "codex",
		AgentType:    baseacp.AgentTypeCodex,
		CWD:          cwd,
		AllowedRoots: []string{cwd},
		Codex:        &acpservice.CodexConfig{Mode: acpservice.CodexModeAdapter},
	}
	cfg.Normalize()
	m := newTestManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	inst, err := m.resolveInstance(ctx, "smoke", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}

	list, err := listAgentSessions(ctx, cfg, ListSessionsRequest{CWD: cwd})
	if err != nil {
		if strings.Contains(err.Error(), "does not advertise session/list") {
			t.Logf("codex-acp does not advertise session/list; skipping list check")
		} else {
			t.Fatalf("session/list: %v", err)
		}
	} else {
		t.Logf("codex-acp session/list returned %d sessions", len(list.Sessions))
	}

	transcript, err := loadAgentTranscript(ctx, cfg, TranscriptRequest{SessionID: inst.sessionID, CWD: cwd})
	switch {
	case err == nil:
		t.Logf("codex-acp transcript replay returned %d messages", len(transcript.Messages))
	case strings.Contains(err.Error(), "does not advertise session/load"):
		t.Logf("codex-acp does not advertise session/load; skipping transcript check")
	case strings.Contains(err.Error(), "Resource not found"):
		// codex does not persist a session until its first turn completes, so
		// replaying a fresh handshake-only session legitimately fails.
		t.Logf("codex-acp cannot replay a turn-less session (resource not found); skipping transcript check")
	default:
		t.Fatalf("transcript replay: %v", err)
	}
}

// requirePromptSmoke gates the prompt-level smokes behind an extra env var:
// they send a real prompt to a real model and therefore spend tokens.
func requirePromptSmoke(t *testing.T, bin string) {
	t.Helper()
	requireSmoke(t, bin)
	if os.Getenv("AGW_ACP_SMOKE_PROMPT") == "" {
		t.Skip("set AGW_ACP_SMOKE_PROMPT=1 to run prompt-level smoke tests (spends model tokens)")
	}
}

// smokePromptTurn runs one real model turn and then replays the session
// transcript, verifying the full prompt path (streaming deltas, stop reason)
// and that the replayed transcript carries the real user and assistant
// messages.
func smokePromptTurn(t *testing.T, cfg acpservice.ServiceConfig) {
	t.Helper()
	cfg.Normalize()
	m := newTestManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	inst, err := m.resolveInstance(ctx, "smoke-prompt", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}

	const input = "Reply with exactly the single word: pong"
	var deltas, reasoning []string
	var usageRaw []string
	events := map[string]int{}
	emit := func(ev TurnEvent) error {
		events[ev.Event]++
		switch ev.Event {
		case "delta":
			deltas = append(deltas, ev.Text)
		case "reasoning":
			reasoning = append(reasoning, ev.Text)
		case "usage":
			usageRaw = append(usageRaw, string(ev.Data))
		}
		return nil
	}
	stopReason, err := inst.prompt(ctx, TurnRequest{ThreadID: "t1", Input: input}, emit)
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	text := strings.TrimSpace(strings.Join(deltas, ""))
	if text == "" {
		t.Fatalf("prompt produced no delta text (events: %v)", events)
	}
	if stopReason == "" {
		t.Fatal("prompt returned an empty stop reason")
	}
	t.Logf("%s prompt: stop_reason=%s, reply=%q, reasoning_chunks=%d, events=%v",
		cfg.AgentType, stopReason, text, len(reasoning), events)

	transcript, err := loadAgentTranscript(ctx, cfg, TranscriptRequest{SessionID: inst.sessionID, CWD: cfg.CWD})
	if err != nil {
		t.Fatalf("transcript replay after a real turn: %v", err)
	}
	roles := map[string]string{}
	for _, msg := range transcript.Messages {
		roles[msg.Role] += msg.Text
	}
	if !strings.Contains(roles["user"], input) {
		t.Fatalf("replayed transcript user text = %q, want it to contain the prompt", roles["user"])
	}
	if strings.TrimSpace(roles["assistant"]) == "" {
		t.Fatalf("replayed transcript has no assistant text: %+v", transcript.Messages)
	}
	t.Logf("%s transcript: %d messages, assistant=%q", cfg.AgentType, len(transcript.Messages), strings.TrimSpace(roles["assistant"]))

	if len(usageRaw) > 0 {
		t.Logf("%s streamed %d usage event(s) during the turn:", cfg.AgentType, len(usageRaw))
		for i, raw := range usageRaw {
			t.Logf("  usage[%d] = %s", i, raw)
		}
	} else {
		t.Logf("%s streamed no usage events during the turn", cfg.AgentType)
	}
	if snap := inst.meta.snapshot(); len(snap.Usage) > 0 {
		t.Logf("%s usage cached after turn: %s", cfg.AgentType, snap.Usage)
	} else {
		t.Logf("%s pushed no usage_update during the turn", cfg.AgentType)
	}
}

func TestSmokeOpencodePromptTurn(t *testing.T) {
	requirePromptSmoke(t, "opencode")
	cwd := t.TempDir()
	smokePromptTurn(t, acpservice.ServiceConfig{
		ID:           "opencode-prompt-smoke",
		Name:         "opencode",
		AgentType:    baseacp.AgentTypeOpencode,
		CWD:          cwd,
		AllowedRoots: []string{cwd},
	})
}

// TestSmokeOpencodeInteractivePermission runs a real tool-using turn under
// permission_mode interactive and answers the agent's permission request the
// way the dispatcher decision endpoint does. If the agent's own policy does
// not ask for permission for the prompted action, the test logs that and only
// verifies the turn completes.
func TestSmokeOpencodeInteractivePermission(t *testing.T) {
	requirePromptSmoke(t, "opencode")
	cwd := t.TempDir()
	// Project-level opencode config forcing permission prompts for edits and
	// shell commands, so the agent actually raises session/request_permission.
	if err := os.WriteFile(cwd+"/opencode.json", []byte(`{"$schema":"https://opencode.ai/config.json","permission":{"edit":"ask","bash":"ask"}}`), 0o644); err != nil {
		t.Fatalf("write opencode.json: %v", err)
	}
	cfg := acpservice.ServiceConfig{
		ID:             "opencode-perm-smoke",
		Name:           "opencode",
		AgentType:      baseacp.AgentTypeOpencode,
		CWD:            cwd,
		AllowedRoots:   []string{cwd},
		PermissionMode: baseacp.PermissionModeInteractive,
	}
	cfg.Normalize()
	m := newTestManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	inst, err := m.resolveInstance(ctx, "smoke-perm", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}

	var permissionEvents int
	var resolved []string
	emit := func(ev TurnEvent) error {
		if ev.Event != "permission" {
			return nil
		}
		permissionEvents++
		var params struct {
			Options []struct {
				OptionID string `json:"optionId"`
				ID       string `json:"id"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		if err := json.Unmarshal(ev.Data, &params); err != nil {
			t.Errorf("decode permission params: %v", err)
			return nil
		}
		optionID := ""
		for _, opt := range params.Options {
			if strings.Contains(opt.Kind, "allow") {
				optionID = opt.OptionID
				if optionID == "" {
					optionID = opt.ID
				}
				break
			}
		}
		if optionID == "" {
			t.Errorf("no allow option offered: %s", ev.Data)
			return nil
		}
		if err := m.ResolvePermission(PermissionDecision{RequestID: ev.RequestID, Outcome: "selected", OptionID: optionID}); err != nil {
			t.Errorf("ResolvePermission: %v", err)
		}
		resolved = append(resolved, optionID)
		return nil
	}
	stop, err := inst.prompt(ctx, TurnRequest{ThreadID: "t1", Input: "Create a file named hello.txt in the current directory containing exactly the word: hi"}, emit)
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if permissionEvents == 0 {
		t.Logf("opencode did not request permission for this action (its own policy allowed it); turn completed with stop_reason=%s", stop)
		return
	}
	t.Logf("opencode interactive permission: %d requests resolved with options %v, stop_reason=%s", permissionEvents, resolved, stop)
}

func TestSmokeCodexPromptTurn(t *testing.T) {
	requirePromptSmoke(t, "codex-acp")
	cwd := t.TempDir()
	smokePromptTurn(t, acpservice.ServiceConfig{
		ID:           "codex-prompt-smoke",
		Name:         "codex",
		AgentType:    baseacp.AgentTypeCodex,
		CWD:          cwd,
		AllowedRoots: []string{cwd},
		Codex:        &acpservice.CodexConfig{Mode: acpservice.CodexModeAdapter},
	})
}

func smokeExactRunCancel(t *testing.T, cfg acpservice.ServiceConfig) {
	t.Helper()
	cfg.MaxInstances = 2
	cfg.Normalize()
	m := newTestManager()
	defer m.Close()
	cancelFrames := make(chan json.RawMessage, 2)
	m.notifyObserver = func(method string, params json.RawMessage) {
		if method == "session/cancel" {
			cancelFrames <- params
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	type turnResult struct {
		runID string
		err   error
	}
	const runA = "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const runB = "run-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	sessionSeen := make(chan string, 2)
	releases := map[string]chan struct{}{runA: make(chan struct{}), runB: make(chan struct{})}
	done := make(chan turnResult, 2)
	start := func(runID, threadID string) {
		go func() {
			err := m.serveTurn(ctx, cfg, TurnRequest{RunID: runID, ThreadID: threadID, Input: "Explain the repository in detail."}, func(ev TurnEvent) error {
				if ev.Event == "session" {
					sessionSeen <- runID
					<-releases[runID]
				}
				return nil
			})
			done <- turnResult{runID: runID, err: err}
		}()
	}
	start(runA, "cancel-smoke-a")
	start(runB, "cancel-smoke-b")
	seen := map[string]bool{}
	for len(seen) != 2 {
		select {
		case runID := <-sessionSeen:
			seen[runID] = true
		case result := <-done:
			t.Fatalf("run %s ended before cancellation: %v", result.runID, result.err)
		case <-ctx.Done():
			t.Fatalf("both turns did not start: %+v", seen)
		}
	}
	active := m.ListActiveRuns(cfg.ID)
	if len(active) != 2 || active[0].RunID != runA || active[1].RunID != runB || active[0].SessionID == "" || active[1].SessionID == "" {
		t.Fatalf("active runs missing native session bindings: %+v", active)
	}
	sessions := map[string]string{active[0].RunID: active[0].SessionID, active[1].RunID: active[1].SessionID}
	if err := m.CancelRun(cfg.ID, runA); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	assertNativeCancelFrame(t, cancelFrames, sessions[runA])
	close(releases[runA])
	select {
	case result := <-done:
		if result.runID != runA || result.err != nil {
			t.Fatalf("cancelled run=%s error=%v, want %s", result.runID, result.err, runA)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("native cancellation did not terminate promptly")
	}
	if got := m.ListActiveRuns(cfg.ID); len(got) != 1 || got[0].RunID != runB {
		t.Fatalf("unrelated run was affected by exact cancel: %+v", got)
	}
	if len(m.ListInstances()) != 2 {
		t.Fatalf("exact cancellation removed or replaced a pool entry")
	}
	if err := m.CancelRun(cfg.ID, runB); err != nil {
		t.Fatalf("cleanup CancelRun: %v", err)
	}
	assertNativeCancelFrame(t, cancelFrames, sessions[runB])
	close(releases[runB])
	select {
	case result := <-done:
		if result.runID != runB || result.err != nil {
			t.Fatalf("cleanup run=%s error=%v, want %s", result.runID, result.err, runB)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("unrelated run did not terminate during cleanup")
	}
}

func TestSmokeOpencodeExactRunCancel(t *testing.T) {
	requireSmoke(t, "opencode")
	cwd := t.TempDir()
	smokeExactRunCancel(t, acpservice.ServiceConfig{ID: "opencode-cancel-smoke", Name: "opencode", AgentType: baseacp.AgentTypeOpencode, CWD: cwd, AllowedRoots: []string{cwd}})
}
func TestSmokeCodexExactRunCancel(t *testing.T) {
	requireSmoke(t, "codex-acp")
	cwd := t.TempDir()
	smokeExactRunCancel(t, acpservice.ServiceConfig{ID: "codex-cancel-smoke", Name: "codex", AgentType: baseacp.AgentTypeCodex, CWD: cwd, AllowedRoots: []string{cwd}, Codex: &acpservice.CodexConfig{Mode: acpservice.CodexModeAdapter}})
}

func TestSmokeCodexHandshake(t *testing.T) {
	requireSmoke(t, "codex-acp")
	cwd := t.TempDir()
	smokeHandshake(t, acpservice.ServiceConfig{
		ID:           "codex-smoke",
		Name:         "codex",
		AgentType:    baseacp.AgentTypeCodex,
		CWD:          cwd,
		AllowedRoots: []string{cwd},
		Codex:        &acpservice.CodexConfig{Mode: acpservice.CodexModeAdapter},
	})
}
