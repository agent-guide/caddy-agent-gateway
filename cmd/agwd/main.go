package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/agent-guide/agent-gateway/standalone/server"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"

	// ACP agents register runtime factories through init.
	_ "github.com/agent-guide/agent-gateway/pkg/acp/agent/codex"
	_ "github.com/agent-guide/agent-gateway/pkg/acp/agent/opencode"

	// CLI authenticators register runtime factories through init.
	_ "github.com/agent-guide/agent-gateway/pkg/cliauth/authenticator"

	// LLM providers register runtime factories through init.
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropic"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/claudecode"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/codex"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/deepseek"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/gemini"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/ollama"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/openai"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/openrouter"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/qwen"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/zhipu"
)

func main() {
	if err := loadRuntimeEnv(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var opts server.Options
	rootCmd := &cobra.Command{
		Use:   "agwd",
		Short: "Standalone Agent Gateway daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return server.Run(ctx, opts)
		},
	}
	rootCmd.Flags().StringVar(&opts.Addr, "addr", "127.0.0.1:8080", "LLM gateway listen address")
	rootCmd.Flags().StringVar(&opts.AdminAddr, "admin-addr", "localhost:8019", "Admin API listen address")
	rootCmd.Flags().StringVar(&opts.AdminBasicAuthHash, "admin-basic-auth-hash", os.Getenv("AGW_ADMIN_BASIC_AUTH_HASH"), "admin Basic Auth configuration as username:bcrypt-hash")
	rootCmd.Flags().StringVar(&opts.ConfigStorePath, "config-store", "./data/configstore.db", "SQLite config store file")
	rootCmd.Flags().StringVar(&opts.StaticConfigPath, "static-config", "", "gateway bundle YAML file loaded as read-only static configuration")
	rootCmd.Flags().StringArrayVar(&opts.ProviderTypes, "provider-type", nil, "provider type enabled for this process; repeat to allow multiple types")
	rootCmd.Flags().IntVar(&opts.Metrics.RetentionDays, "metrics-retention-days", 30, "retention window in days for persisted usage events")
	rootCmd.Flags().IntVar(&opts.Metrics.MaxAgentDepth, "max-agent-depth", 0, "maximum inbound X-Agent-Depth allowed before rejecting a request; 0 disables enforcement")
	rootCmd.Flags().StringVar(&opts.Metrics.OTLP.Endpoint, "metrics-otlp-endpoint", "", "OTLP collector endpoint (host:port or URL) for exporting usage events as spans; empty disables export")
	rootCmd.Flags().StringVar(&opts.Metrics.OTLP.Protocol, "metrics-otlp-protocol", "grpc", "OTLP transport protocol: grpc or http")
	rootCmd.Flags().BoolVar(&opts.Metrics.OTLP.Insecure, "metrics-otlp-insecure", false, "disable TLS for the OTLP exporter")
	rootCmd.Flags().StringToStringVar(&opts.Metrics.OTLP.Headers, "metrics-otlp-header", nil, "header sent with every OTLP export request as name=value; repeat for multiple headers")
	rootCmd.Flags().StringVar(&opts.Metrics.OTLP.ServiceName, "metrics-otlp-service-name", "", "OTel resource service.name for exported spans (default agent-gateway)")
	rootCmd.Flags().BoolVar(&opts.Metrics.OTLP.Components, "metrics-otlp-components", false, "additionally export one span per eino chat-model component call, nested under the interaction span")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func loadRuntimeEnv() error {
	for _, path := range []string{".env.local", ".env"} {
		if err := loadOptionalEnvFile(path); err != nil {
			return err
		}
	}
	return nil
}

func loadOptionalEnvFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := godotenv.Load(path); err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	return nil
}
