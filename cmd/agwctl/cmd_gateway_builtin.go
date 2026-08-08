package main

import (
	"context"

	"github.com/spf13/cobra"
)

var gatewayBuiltinRuntimeCmd = &cobra.Command{Use: "builtin-runtime", Short: "Inspect the builtin ADK host runtime"}

var gatewayBuiltinRuntimeGetCmd = &cobra.Command{
	Use: "get", Short: "Get builtin materialization and runtime diagnostics",
	RunE: func(cmd *cobra.Command, args []string) error {
		view, err := newGatewayClient().GetBuiltinRuntime(context.Background())
		if err != nil {
			return err
		}
		return printJSON(view)
	},
}

var gatewayBuiltinRuntimeInFlightCmd = &cobra.Command{
	Use: "inflight", Short: "List in-flight builtin turns",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListBuiltinInFlight(context.Background())
		if err != nil {
			return err
		}
		return printJSON(items)
	},
}

func init() {
	gatewayBuiltinRuntimeCmd.AddCommand(gatewayBuiltinRuntimeGetCmd, gatewayBuiltinRuntimeInFlightCmd)
	rootCmd.AddCommand(gatewayBuiltinRuntimeCmd)
}
