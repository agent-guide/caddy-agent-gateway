package runtimeapitest

import (
	"context"
	"sync"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
)

func TestBackendCapabilitiesCanBeReplacedConcurrently(t *testing.T) {
	t.Parallel()

	backend := NewBackend("fake")
	var wait sync.WaitGroup
	for i := 0; i < 100; i++ {
		wait.Add(2)
		go func(executable bool) {
			defer wait.Done()
			backend.SetCapabilities(runtimeapi.Capabilities{Executable: executable}, nil)
		}(i%2 == 0)
		go func() {
			defer wait.Done()
			_, _ = backend.Capabilities(context.Background(), agent.Agent{})
		}()
	}
	wait.Wait()
}
