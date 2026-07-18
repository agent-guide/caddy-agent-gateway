package pipeline

import (
	"fmt"
	"sync"
	"time"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/pkg/configstore/sqlite"
	"gorm.io/gorm"
)

// usageJanitorInterval is how often expired usage events are pruned while the
// gateway runs. Retention is also applied once at startup; this keeps a
// long-running process from accumulating events indefinitely.
const usageJanitorInterval = 6 * time.Hour

type SQLiteSink struct {
	db        *gorm.DB
	retention time.Duration
	stop      chan struct{}
	stopOnce  sync.Once
}

func NewSQLiteSink(provider usage.SQLDBProvider, cfg usage.Config) (*SQLiteSink, *sqlite.UsageQueries, error) {
	if provider == nil || provider.UsageDB() == nil {
		return nil, nil, fmt.Errorf("usage sqlite db is not available")
	}
	cfg = cfg.Normalized()
	db := provider.UsageDB()
	if err := sqlite.MigrateUsageTables(db); err != nil {
		return nil, nil, err
	}
	retention := time.Duration(cfg.RetentionDays) * 24 * time.Hour
	if err := sqlite.CleanupUsageEvents(db, retention); err != nil {
		return nil, nil, err
	}
	sink := &SQLiteSink{db: db, retention: retention, stop: make(chan struct{})}
	if retention > 0 {
		go sink.runJanitor()
	}
	return sink, sqlite.NewUsageQueries(db), nil
}

func (s *SQLiteSink) Write(ev any) error {
	if s == nil || s.db == nil {
		return nil
	}
	switch typed := ev.(type) {
	case usage.LLMUsageEvent:
		return sqlite.InsertLLMUsageEvent(s.db, typed)
	case usage.MCPUsageEvent:
		return sqlite.InsertMCPUsageEvent(s.db, typed)
	case usage.ACPUsageEvent:
		return sqlite.InsertACPUsageEvent(s.db, typed)
	case usage.BuiltinUsageEvent:
		return sqlite.InsertBuiltinUsageEvent(s.db, typed)
	default:
		return nil
	}
}

func (s *SQLiteSink) runJanitor() {
	ticker := time.NewTicker(usageJanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			_ = sqlite.CleanupUsageEvents(s.db, s.retention)
		}
	}
}

func (s *SQLiteSink) Close() error {
	if s == nil {
		return nil
	}
	s.stopOnce.Do(func() {
		if s.stop != nil {
			close(s.stop)
		}
	})
	return nil
}
