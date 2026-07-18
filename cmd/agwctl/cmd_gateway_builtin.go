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

func init() {
	gatewayBuiltinRouteCmd.AddCommand(
		gatewayBuiltinRouteListCmd,
		gatewayBuiltinRouteGetCmd,
	)
	gatewayCmd.AddCommand(gatewayBuiltinRouteCmd)
}
