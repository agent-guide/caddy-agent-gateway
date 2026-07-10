package pipeline

import (
	"sync"
	"testing"
	"time"
)

type captureSink struct {
	mu     sync.Mutex
	events []any
}

func (s *captureSink) Write(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, v)
	return nil
}

func (s *captureSink) Close() error { return nil }

func TestPipelineCloseFlushesPendingEvent(t *testing.T) {
	sink := &captureSink{}
	p := NewEventPipeline(4, sink)
	p.Start()
	if !p.Enqueue("one") {
		t.Fatal("Enqueue() = false")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if len(sink.events) != 1 || sink.events[0] != "one" {
		t.Fatalf("events = %#v, want one pending event flushed", sink.events)
	}
}

func TestPipelineDropsWhenFull(t *testing.T) {
	p := NewEventPipeline(1)
	if !p.Enqueue("one") {
		t.Fatal("first Enqueue() = false")
	}
	if p.Enqueue("two") {
		t.Fatal("second Enqueue() = true, want drop")
	}
	if got := p.DroppedEvents(); got != 1 {
		t.Fatalf("DroppedEvents() = %d, want 1", got)
	}
}

func TestPipelineCloseConcurrentEnqueueDoesNotPanic(t *testing.T) {
	for i := 0; i < 100; i++ {
		p := NewEventPipeline(1, &captureSink{})
		p.Start()
		errs := make(chan any, 32)
		var wg sync.WaitGroup
		for worker := 0; worker < 16; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if recovered := recover(); recovered != nil {
						errs <- recovered
					}
				}()
				deadline := time.Now().Add(5 * time.Millisecond)
				for time.Now().Before(deadline) {
					_ = p.Enqueue("event")
				}
			}()
		}
		if err := p.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		wg.Wait()
		close(errs)
		for recovered := range errs {
			t.Fatalf("Enqueue panicked during Close: %v", recovered)
		}
		if p.Enqueue("after-close") {
			t.Fatal("Enqueue() after Close = true, want false")
		}
	}
}
