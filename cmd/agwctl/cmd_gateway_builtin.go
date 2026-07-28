package main

import (
	"context"

	"github.com/spf13/cobra"
)

// ── gateway builtin route ────────────────────────────────────────────────────

var gatewayBuiltinRouteCmd = &cobra.Command{
	Use:   "builtin-route",
	Short: "Inspect gateway builtin agent routes",
}

var gatewayBuiltinRouteListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all builtin routes",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListBuiltinRoutes(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayBuiltinRoutesTable(items)
		return nil
	},
}

var gatewayBuiltinRouteGetCmd = &cobra.Command{
	Use:   "get <builtin-route-id>",
	Short: "Get one builtin route",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetBuiltinRoute(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

// ── gateway builtin runtime ──────────────────────────────────────────────────

var gatewayBuiltinCancelMode string

var gatewayBuiltinRuntimeCmd = &cobra.Command{
	Use:   "builtin-runtime",
	Short: "Inspect and operate the builtin ADK host runtime",
}

var gatewayBuiltinRuntimeGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Get the builtin runtime view (materializations, pending permissions, in-flight turns)",
	RunE: func(cmd *cobra.Command, args []string) error {
		view, err := newGatewayClient().GetBuiltinRuntime(context.Background())
		if err != nil {
			return err
		}
		return printJSON(view)
	},
}

var gatewayBuiltinRuntimeInFlightCmd = &cobra.Command{
	Use:   "inflight",
	Short: "List in-flight builtin turns",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListBuiltinInFlight(context.Background())
		if err != nil {
			return err
		}
		return printJSON(items)
	},
}

var gatewayBuiltinRuntimeCancelTurnCmd = &cobra.Command{
	Use:   "cancel-turn <agent-id> <session-id>",
	Short: "Cancel a running builtin turn (--mode force|graceful)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().CancelBuiltinTurn(context.Background(), args[0], args[1], gatewayBuiltinCancelMode)
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

func init() {
	gatewayBuiltinRuntimeCancelTurnCmd.Flags().StringVar(&gatewayBuiltinCancelMode, "mode", "", "cancel mode: force (default) or graceful")
	gatewayBuiltinRuntimeCmd.AddCommand(
		gatewayBuiltinRuntimeGetCmd,
		gatewayBuiltinRuntimeInFlightCmd,
		gatewayBuiltinRuntimeCancelTurnCmd,
	)
	gatewayCmd.AddCommand(gatewayBuiltinRuntimeCmd)
}
