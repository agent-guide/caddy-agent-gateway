package pipeline

import (
	"sync"
	"sync/atomic"
)

type Sink interface {
	Write(any) error
	Close() error
}

type EventPipeline struct {
	ch            chan any
	sinks         []Sink
	once          sync.Once
	closed        chan struct{}
	closeMu       sync.RWMutex
	closing       bool
	dropped       atomic.Uint64
	writeFailures atomic.Uint64
}

func NewEventPipeline(size int, sinks ...Sink) *EventPipeline {
	if size <= 0 {
		size = 4096
	}
	return &EventPipeline{
		ch:     make(chan any, size),
		sinks:  append([]Sink(nil), sinks...),
		closed: make(chan struct{}),
	}
}

func (p *EventPipeline) Start() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		go p.run()
	})
}

func (p *EventPipeline) Enqueue(v any) bool {
	if p == nil || v == nil {
		return false
	}
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closing {
		p.dropped.Add(1)
		return false
	}
	select {
	case p.ch <- v:
		return true
	default:
		p.dropped.Add(1)
		return false
	}
}

func (p *EventPipeline) DroppedEvents() uint64 {
	if p == nil {
		return 0
	}
	return p.dropped.Load()
}

func (p *EventPipeline) WriteFailures() uint64 {
	if p == nil {
		return 0
	}
	return p.writeFailures.Load()
}

func (p *EventPipeline) Close() error {
	if p == nil {
		return nil
	}
	p.Start()
	p.closeMu.Lock()
	defer p.closeMu.Unlock()
	if !p.closing {
		p.closing = true
		close(p.ch)
	}
	<-p.closed
	var err error
	for _, sink := range p.sinks {
		if sink == nil {
			continue
		}
		if closeErr := sink.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}
	return err
}

func (p *EventPipeline) run() {
	defer close(p.closed)
	for ev := range p.ch {
		for _, sink := range p.sinks {
			if sink == nil {
				continue
			}
			if err := sink.Write(ev); err != nil {
				p.writeFailures.Add(1)
			}
		}
	}
}
