package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/glebarez/sqlite"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type Config struct {
	SQLitePath string `json:"sqlite_path,omitempty"`
}

type SQLiteConfigStoreCreator struct {
	SQLitePath string `json:"sqlite_path,omitempty"`

	logger *zap.Logger
	db     *gorm.DB
}

func init() {
	configstore.RegisterConfigStoreCreatorFactory("sqlite", func(ctx context.Context, raw json.RawMessage, logger *zap.Logger) (configstore.ConfigStoreCreator, error) {
		var cfg Config
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("decode sqlite config store backend config: %w", err)
			}
		}
		return Open(ctx, cfg, logger)
	})
}

func Open(ctx context.Context, cfg Config, logger *zap.Logger) (*SQLiteConfigStoreCreator, error) {
	if err := PreflightLegacyAgentRuntime(ctx, cfg.SQLitePath); err != nil {
		return nil, err
	}
	configstoreBackend := &SQLiteConfigStoreCreator{
		SQLitePath: cfg.SQLitePath,
		logger:     logger,
	}
	if err := configstoreBackend.Open(ctx); err != nil {
		return nil, err
	}
	return configstoreBackend, nil
}

func (s *SQLiteConfigStoreCreator) Open(_ context.Context) error {
	if s.logger == nil {
		s.logger = zap.NewNop()
	}
	if s.SQLitePath == "" {
		return fmt.Errorf("sqlite path is required")
	}

	if _, err := os.Stat(s.SQLitePath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(s.SQLitePath), 0o755); err != nil {
			return err
		}
		f, err := os.Create(s.SQLitePath)
		if err != nil {
			return err
		}
		_ = f.Close()
	}

	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=cache_size(10000)&_pragma=busy_timeout(60000)&_pragma=wal_autocheckpoint(1000)&_pragma=foreign_keys(1)", s.SQLitePath)
	s.logger.Debug("opening DB with dsn", zap.String("dsn", dsn))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: newGormLogger(*s.logger),
	})
	if err != nil {
		return err
	}
	s.logger.Debug("sqlite db opened for config store backend")

	s.db = db
	return nil
}

func (s *SQLiteConfigStoreCreator) NewStore(storeSchema configstore.StoreSchema) (configstore.ConfigStore, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("sqlite config store backend db is nil")
	}
	if err := migrateSchema(s.db, storeSchema); err != nil {
		return nil, err
	}
	return &JSONConfigStore{db: s.db, schema: storeSchema}, nil
}

var _ configstore.ConfigStoreCreator = (*SQLiteConfigStoreCreator)(nil)
