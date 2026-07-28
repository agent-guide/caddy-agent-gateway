package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"

	"github.com/spf13/cobra"
)

// ── gateway agent ────────────────────────────────────────────────────────────
//
// Reads and lifecycle deletes only. Agents are created/updated through
// `agwctl gateway apply` like every other gateway-bundle object.

var gatewayAgentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage gateway agents",
}

var gatewayAgentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all agents",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListAgents(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayAgentsTable(items)
		return nil
	},
}

var gatewayAgentGetCmd = &cobra.Command{
	Use:   "get <agent-id>",
	Short: "Get one agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetAgent(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayAgentDeleteCmd = &cobra.Command{
	Use:   "delete <agent-id>",
	Short: "Delete an agent after its AgentRoutes are removed",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().DeleteAgent(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayAgentWorkspaceCmd = &cobra.Command{
	Use:   "workspace <agent-id>",
	Short: "Get the aggregated workspace summary for an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().GetAgentWorkspace(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayAgentActivityCmd = &cobra.Command{
	Use:   "activity <agent-id>",
	Short: "Get recent activity for an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().GetAgentActivity(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayAgentUsageCmd = &cobra.Command{
	Use:   "usage <agent-id>",
	Short: "Get usage summary for an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().GetAgentUsage(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayAgentInteractionsCmd = &cobra.Command{
	Use:   "interactions <agent-id>",
	Short: "Get interaction events for an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().GetAgentInteractions(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayAgentResourcesCmd = &cobra.Command{
	Use:   "resources <agent-id>",
	Short: "Get linked resources for an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().GetAgentResources(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayAgentHealthCmd = &cobra.Command{
	Use:   "health <agent-id>",
	Short: "Get shallow health for an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().GetAgentHealth(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var agentCancelMode string
var agentPermissionOutcome string
var agentPermissionOptionID string
var agentPermissionDecisions string
var agentSessionCWD string
var agentSessionCursor string

var gatewayAgentCapabilitiesCmd = rawAgentReadCommand("capabilities <agent-id>", "Get runtime capabilities for an agent", func(ctx context.Context, id string) (json.RawMessage, error) {
	return newGatewayClient().GetAgentCapabilities(ctx, id)
})
var gatewayAgentRunsCmd = rawAgentReadCommand("runs <agent-id>", "List active and retained runs for an agent", func(ctx context.Context, id string) (json.RawMessage, error) {
	return newGatewayClient().ListAgentRuns(ctx, id)
})
var gatewayAgentPermissionsCmd = rawAgentReadCommand("permissions <agent-id>", "List pending permissions for an agent", func(ctx context.Context, id string) (json.RawMessage, error) {
	return newGatewayClient().ListAgentPermissions(ctx, id)
})

var gatewayAgentCancelCmd = &cobra.Command{Use: "cancel <agent-id> <run-id>", Short: "Cancel one exact agent run", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
	resp, err := newGatewayClient().CancelAgentRun(context.Background(), args[0], args[1], runtimeapi.CancelMode(agentCancelMode))
	if err != nil {
		return err
	}
	return printJSON(resp)
}}
var gatewayAgentDecideCmd = &cobra.Command{Use: "decide <agent-id> <request-id>", Short: "Resolve one pending agent permission", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
	decision := runtimeapi.PermissionDecision{RequestID: args[1], Outcome: agentPermissionOutcome, OptionID: agentPermissionOptionID}
	if agentPermissionDecisions != "" {
		if err := json.Unmarshal([]byte(agentPermissionDecisions), &decision.Decisions); err != nil {
			return fmt.Errorf("decode --decisions: %w", err)
		}
	}
	resp, err := newGatewayClient().ResolveAgentPermission(context.Background(), args[0], args[1], decision)
	if err != nil {
		return err
	}
	return printJSON(resp)
}}
var gatewayAgentSessionsCmd = &cobra.Command{Use: "sessions <agent-id>", Short: "List sessions exposed by an agent runtime", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	resp, err := newGatewayClient().ListAgentSessions(context.Background(), args[0], agentSessionCWD, agentSessionCursor)
	if err != nil {
		return err
	}
	return printJSON(resp)
}}
var gatewayAgentTranscriptCmd = &cobra.Command{Use: "transcript <agent-id> <session-id>", Short: "Load a session transcript exposed by an agent runtime", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
	resp, err := newGatewayClient().GetAgentTranscript(context.Background(), args[0], args[1], agentSessionCWD)
	if err != nil {
		return err
	}
	return printJSON(resp)
}}

func rawAgentReadCommand(use, short string, read func(context.Context, string) (json.RawMessage, error)) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := read(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	}}
}

func init() {
	gatewayAgentCancelCmd.Flags().StringVar(&agentCancelMode, "mode", "force", "cancel mode: force or graceful")
	gatewayAgentDecideCmd.Flags().StringVar(&agentPermissionOutcome, "outcome", "", "runtime-native common outcome")
	gatewayAgentDecideCmd.Flags().StringVar(&agentPermissionOptionID, "option-id", "", "ACP option id")
	gatewayAgentDecideCmd.Flags().StringVar(&agentPermissionDecisions, "decisions", "", "builtin action decisions as JSON array")
	gatewayAgentSessionsCmd.Flags().StringVar(&agentSessionCWD, "cwd", "", "session cwd filter")
	gatewayAgentSessionsCmd.Flags().StringVar(&agentSessionCursor, "cursor", "", "session page cursor")
	gatewayAgentTranscriptCmd.Flags().StringVar(&agentSessionCWD, "cwd", "", "session cwd")
	gatewayAgentCmd.AddCommand(
		gatewayAgentListCmd,
		gatewayAgentGetCmd,
		gatewayAgentDeleteCmd,
		gatewayAgentWorkspaceCmd,
		gatewayAgentActivityCmd,
		gatewayAgentUsageCmd,
		gatewayAgentInteractionsCmd,
		gatewayAgentResourcesCmd,
		gatewayAgentHealthCmd,
		gatewayAgentCapabilitiesCmd,
		gatewayAgentRunsCmd,
		gatewayAgentCancelCmd,
		gatewayAgentPermissionsCmd,
		gatewayAgentDecideCmd,
		gatewayAgentSessionsCmd,
		gatewayAgentTranscriptCmd,
	)
	gatewayCmd.AddCommand(gatewayAgentCmd)
}
