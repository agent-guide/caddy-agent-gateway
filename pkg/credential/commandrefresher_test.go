package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCommandRefresherProtocol(t *testing.T) {
	t.Setenv("AGW_CREDENTIAL_REFRESH_HELPER", "success")
	refresher := NewCommandRefresher(os.Args[0], "-test.run=TestCredentialRefreshHelperProcess")
	updated, err := refresher.Refresh(context.Background(), &Credential{ID: "cred-1"})
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if updated.ID != "cred-1" || updated.Label != "refreshed" {
		t.Fatalf("updated credential = %#v", updated)
	}
}

func TestCommandRefresherRejectsEmptyCommand(t *testing.T) {
	_, err := NewCommandRefresher(" ").Refresh(context.Background(), &Credential{})
	if err == nil || !strings.Contains(err.Error(), "command is empty") {
		t.Fatalf("Refresh error = %v, want empty-command error", err)
	}
}

func TestCommandRefresherIncludesStderrOnFailure(t *testing.T) {
	t.Setenv("AGW_CREDENTIAL_REFRESH_HELPER", "failure")
	refresher := NewCommandRefresher(os.Args[0], "-test.run=TestCredentialRefreshHelperProcess")
	_, err := refresher.Refresh(context.Background(), &Credential{})
	if err == nil || !strings.Contains(err.Error(), "refresh exploded") {
		t.Fatalf("Refresh error = %v, want helper stderr", err)
	}
}

func TestCommandRefresherRejectsInvalidJSON(t *testing.T) {
	t.Setenv("AGW_CREDENTIAL_REFRESH_HELPER", "invalid-json")
	refresher := NewCommandRefresher(os.Args[0], "-test.run=TestCredentialRefreshHelperProcess")
	_, err := refresher.Refresh(context.Background(), &Credential{})
	if err == nil || !strings.Contains(err.Error(), "decode credential refresher output") {
		t.Fatalf("Refresh error = %v, want decode error", err)
	}
}

func TestCommandRefresherRejectsOversizedStdout(t *testing.T) {
	t.Setenv("AGW_CREDENTIAL_REFRESH_HELPER", "large-stdout")
	refresher := NewCommandRefresher(os.Args[0], "-test.run=TestCredentialRefreshHelperProcess")
	_, err := refresher.Refresh(context.Background(), &Credential{})
	if err == nil || !strings.Contains(err.Error(), "output exceeds") {
		t.Fatalf("Refresh error = %v, want output limit error", err)
	}
}

func TestCommandRefresherBoundsStderr(t *testing.T) {
	t.Setenv("AGW_CREDENTIAL_REFRESH_HELPER", "large-stderr")
	refresher := NewCommandRefresher(os.Args[0], "-test.run=TestCredentialRefreshHelperProcess")
	_, err := refresher.Refresh(context.Background(), &Credential{})
	if err == nil || !strings.Contains(err.Error(), "stderr truncated") {
		t.Fatalf("Refresh error = %v, want truncated-stderr error", err)
	}
	if len(err.Error()) > maxCommandRefreshStderr+256 {
		t.Fatalf("Refresh error length = %d, want bounded stderr", len(err.Error()))
	}
}

func TestCommandRefresherAppliesDeadline(t *testing.T) {
	t.Setenv("AGW_CREDENTIAL_REFRESH_HELPER", "hang")
	refresher := NewCommandRefresher(os.Args[0], "-test.run=TestCredentialRefreshHelperProcess")
	if refresher.timeout != defaultCommandRefreshTimeout {
		t.Fatalf("default timeout = %v, want %v", refresher.timeout, defaultCommandRefreshTimeout)
	}
	refresher.timeout = 20 * time.Millisecond
	_, err := refresher.Refresh(context.Background(), &Credential{})
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("Refresh error = %v, want deadline error", err)
	}
}

func TestCredentialRefreshHelperProcess(t *testing.T) {
	mode := os.Getenv("AGW_CREDENTIAL_REFRESH_HELPER")
	if mode == "" {
		return
	}
	if len(os.Args) != 2 || os.Args[1] != "-test.run=TestCredentialRefreshHelperProcess" {
		os.Exit(2)
	}
	switch mode {
	case "failure":
		_, _ = fmt.Fprint(os.Stderr, "refresh exploded")
		os.Exit(7)
	case "invalid-json":
		_, _ = fmt.Fprint(os.Stdout, "{")
		os.Exit(0)
	case "large-stdout":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", maxCommandRefreshStdout+1))
		os.Exit(0)
	case "large-stderr":
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("x", maxCommandRefreshStderr+1))
		os.Exit(7)
	case "hang":
		time.Sleep(time.Hour)
		os.Exit(0)
	case "success":
	default:
		os.Exit(5)
	}
	var cred Credential
	if err := json.NewDecoder(os.Stdin).Decode(&cred); err != nil {
		os.Exit(3)
	}
	cred.Label = "refreshed"
	if err := json.NewEncoder(os.Stdout).Encode(&cred); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}
