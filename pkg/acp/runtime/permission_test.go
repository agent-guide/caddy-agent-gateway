package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/acp/runtimeconfig"
	acptransport "github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

func TestPermissionBrokerResolveIsOneShot(t *testing.T) {
	b := newPermissionBroker()
	pending, err := b.create("svc", "s1", json.RawMessage(`{"options":[]}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := b.list(); len(got) != 1 || got[0].RequestID != pending.info.RequestID || got[0].OwnerID != "svc" {
		t.Fatalf("list = %+v, want the pending entry", got)
	}

	resp := acptransport.PermissionResponse{Outcome: acptransport.PermissionOutcomeSelected, SelectedOptionID: "allow-once"}
	if err := b.resolve(pending.info.RequestID, resp); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := <-pending.decision; got != resp {
		t.Fatalf("decision = %+v, want %+v", got, resp)
	}
	if err := b.resolve(pending.info.RequestID, resp); !errors.Is(err, ErrPermissionNotFound) {
		t.Fatalf("second resolve error = %v, want ErrPermissionNotFound", err)
	}
	if got := b.list(); len(got) != 0 {
		t.Fatalf("list after resolve = %+v, want empty", got)
	}
}

func TestManagerResolvePermissionValidatesDecision(t *testing.T) {
	m := newTestManager()
	pending, err := m.permissions.create("svc", "s1", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	cases := []struct {
		name     string
		decision PermissionDecision
		wantErr  string
	}{
		{"missing request id", PermissionDecision{Outcome: "cancelled"}, "request_id is required"},
		{"unsupported outcome", PermissionDecision{RequestID: pending.info.RequestID, Outcome: "approved"}, "unsupported outcome"},
		{"selected without option", PermissionDecision{RequestID: pending.info.RequestID, Outcome: "selected"}, "option_id is required"},
		{"unknown request", PermissionDecision{RequestID: "perm-missing", Outcome: "cancelled"}, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := m.ResolvePermission(tc.decision)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}

	if err := m.ResolvePermission(PermissionDecision{RequestID: pending.info.RequestID, Outcome: "selected", OptionID: "allow-once"}); err != nil {
		t.Fatalf("valid decision: %v", err)
	}
	got := <-pending.decision
	if got.Outcome != acptransport.PermissionOutcomeSelected || got.SelectedOptionID != "allow-once" {
		t.Fatalf("decision = %+v, want selected allow-once", got)
	}
}

func interactiveInstance(broker *permissionBroker) *instance {
	return &instance{
		cfg:         runtimeconfig.Config{OwnerID: "svc", PermissionMode: "interactive"},
		sessionID:   "s1",
		agent:       stubAgent{},
		permissions: broker,
	}
}

func TestInteractivePermissionResolvesThroughBroker(t *testing.T) {
	broker := newPermissionBroker()
	inst := interactiveInstance(broker)

	var emitted []TurnEvent
	inst.setTurnSink(func(ev TurnEvent) error {
		emitted = append(emitted, ev)
		return nil
	})

	params := json.RawMessage(`{"sessionId":"s1","options":[{"id":"allow-once","kind":"allow_once"}]}`)
	got := make(chan acptransport.PermissionResponse, 1)
	go func() { got <- inst.interactivePermission(context.Background(), params) }()

	// Wait for the pending entry, then answer it like the decision endpoint.
	var requestID string
	deadline := time.Now().Add(5 * time.Second)
	for requestID == "" {
		if pending := broker.list(); len(pending) == 1 {
			requestID = pending[0].RequestID
		} else if time.Now().After(deadline) {
			t.Fatal("permission request never registered with the broker")
		} else {
			time.Sleep(time.Millisecond)
		}
	}
	if err := broker.resolve(requestID, acptransport.PermissionResponse{Outcome: "selected", SelectedOptionID: "allow-once"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	resp := <-got
	if resp.Outcome != "selected" || resp.SelectedOptionID != "allow-once" {
		t.Fatalf("response = %+v, want selected allow-once", resp)
	}
	if len(emitted) != 1 || emitted[0].Event != "permission" || emitted[0].RequestID != requestID || len(emitted[0].Data) == 0 {
		t.Fatalf("emitted = %+v, want one permission event carrying the request id and raw params", emitted)
	}
	if got := broker.list(); len(got) != 0 {
		t.Fatalf("pending after resolution = %+v, want empty", got)
	}
}

func TestInteractivePermissionFailsClosedWithoutTurnClient(t *testing.T) {
	inst := interactiveInstance(newPermissionBroker())
	// No turn sink registered: there is nobody to ask.
	resp := inst.interactivePermission(context.Background(), json.RawMessage(`{}`))
	if resp.Outcome != acptransport.PermissionOutcomeCancelled {
		t.Fatalf("outcome = %q, want cancelled", resp.Outcome)
	}
}

func TestInteractivePermissionFailsClosedOnTimeout(t *testing.T) {
	broker := newPermissionBroker()
	inst := interactiveInstance(broker)
	inst.setTurnSink(func(TurnEvent) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	resp := inst.interactivePermission(ctx, json.RawMessage(`{}`))
	if resp.Outcome != acptransport.PermissionOutcomeCancelled {
		t.Fatalf("outcome = %q, want cancelled", resp.Outcome)
	}
	if got := broker.list(); len(got) != 0 {
		t.Fatalf("pending after timeout = %+v, want empty (entry must be removed)", got)
	}
}

func TestPermissionFuncUsesConfiguredDecisionForNonInteractiveModes(t *testing.T) {
	params := json.RawMessage(`{"options":[{"id":"allow-once","kind":"allow_once","name":"Allow once"}]}`)

	deny := &instance{cfg: runtimeconfig.Config{PermissionMode: "deny"}}
	if resp := deny.permissionFunc()(context.Background(), params); resp.Outcome != acptransport.PermissionOutcomeCancelled {
		t.Fatalf("deny outcome = %q, want cancelled", resp.Outcome)
	}

	auto := &instance{cfg: runtimeconfig.Config{PermissionMode: "auto_approve"}}
	if resp := auto.permissionFunc()(context.Background(), params); resp.Outcome != acptransport.PermissionOutcomeSelected || resp.SelectedOptionID != "allow-once" {
		t.Fatalf("auto_approve response = %+v, want selected allow-once", resp)
	}
}
