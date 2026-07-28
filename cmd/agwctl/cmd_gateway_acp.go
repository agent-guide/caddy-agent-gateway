package main

import (
	"context"
	"fmt"

	"github.com/agent-guide/agent-gateway/pkg/adminclient"
	"github.com/spf13/cobra"
)

var (
	gatewayACPSessionCWD         string
	gatewayACPSessionCursor      string
	gatewayACPTranscriptCWD      string
	gatewayACPPermissionOutcome  string
	gatewayACPPermissionOptionID string
)

// ── gateway acp service ──────────────────────────────────────────────────────

var gatewayACPServiceCmd = &cobra.Command{
	Use:   "acp-service",
	Short: "Manage gateway ACP services",
}

var gatewayACPServiceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all ACP services",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListACPServices(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayACPServicesTable(items)
		return nil
	},
}

var gatewayACPServiceGetCmd = &cobra.Command{
	Use:   "get <acp-service-id>",
	Short: "Get one ACP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetACPService(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayACPServiceDeleteCmd = &cobra.Command{
	Use:   "delete <acp-service-id>",
	Short: "Delete an ACP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().DeleteACPService(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayACPServiceSessionsCmd = &cobra.Command{
	Use:   "sessions <acp-service-id>",
	Short: "List agent-side sessions of an ACP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().ListACPSessions(context.Background(), args[0], adminclient.ACPSessionListOptions{
			CWD:    gatewayACPSessionCWD,
			Cursor: gatewayACPSessionCursor,
		})
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(resp)
		}
		printGatewayACPSessionsTable(resp)
		return nil
	},
}

var gatewayACPServiceTranscriptCmd = &cobra.Command{
	Use:   "transcript <acp-service-id> <session-id>",
	Short: "Replay one agent session transcript via session/load",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().GetACPSessionTranscript(context.Background(), args[0], args[1], gatewayACPTranscriptCWD)
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── gateway acp route ────────────────────────────────────────────────────────

var gatewayACPRouteCmd = &cobra.Command{
	Use:   "acp-route",
	Short: "Manage gateway ACP routes",
}

var gatewayACPRouteListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all ACP routes",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListACPRoutes(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayACPRoutesTable(items)
		return nil
	},
}

var gatewayACPRouteGetCmd = &cobra.Command{
	Use:   "get <acp-route-id>",
	Short: "Get one ACP route",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetACPRoute(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayACPRouteDeleteCmd = &cobra.Command{
	Use:   "delete <acp-route-id>",
	Short: "Delete an ACP route",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().DeleteACPRoute(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── gateway acp runtime ──────────────────────────────────────────────────────

var gatewayACPRuntimeCmd = &cobra.Command{
	Use:   "acp-runtime",
	Short: "Inspect and operate gateway ACP runtime state",
}

var gatewayACPRuntimeGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Get the ACP runtime overview",
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetACPRuntime(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(item)
		}
		printGatewayACPRuntimeOverview(item)
		return nil
	},
}

var gatewayACPRuntimeInFlightCmd = &cobra.Command{
	Use:   "inflight",
	Short: "List in-flight ACP turns",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListACPRuntimeInFlight(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayACPInFlightTable(items)
		return nil
	},
}

var gatewayACPRuntimeCloseThreadCmd = &cobra.Command{
	Use:   "close-thread <agent-id> <thread-id>",
	Short: "Close the pooled agent instances of one ACP thread",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().CloseACPThread(context.Background(), args[0], args[1])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayACPRuntimeResolvePermissionCmd = &cobra.Command{
	Use:   "resolve-permission <request-id>",
	Short: "Answer a pending interactive ACP permission request",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if gatewayACPPermissionOutcome == "" {
			return fmt.Errorf("--outcome is required (selected or cancelled)")
		}
		resp, err := newGatewayClient().ResolveACPPermission(context.Background(), adminclient.ACPPermissionDecision{
			RequestID: args[0],
			Outcome:   gatewayACPPermissionOutcome,
			OptionID:  gatewayACPPermissionOptionID,
		})
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

func init() {
	gatewayACPServiceSessionsCmd.Flags().StringVar(&gatewayACPSessionCWD, "cwd", "", "filter sessions by working directory")
	gatewayACPServiceSessionsCmd.Flags().StringVar(&gatewayACPSessionCursor, "cursor", "", "pagination cursor from a previous response")
	gatewayACPServiceTranscriptCmd.Flags().StringVar(&gatewayACPTranscriptCWD, "cwd", "", "working directory used for the transient replay connection")
	gatewayACPRuntimeCmd.AddCommand(
		gatewayACPRuntimeGetCmd,
		gatewayACPRuntimeInFlightCmd,
		gatewayACPRuntimeCloseThreadCmd,
	)
	gatewayCmd.AddCommand(gatewayACPRuntimeCmd)
}
