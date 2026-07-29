package main

import (
	"context"

	"github.com/spf13/cobra"
)

var gatewayACPRuntimeCmd = &cobra.Command{Use: "acp-runtime", Short: "Inspect gateway ACP runtime state"}

var gatewayACPRuntimeGetCmd = &cobra.Command{
	Use: "get", Short: "Get the ACP runtime overview",
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
	Use: "inflight", Short: "List in-flight ACP turns",
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
	Use: "close-thread <agent-id> <thread-id>", Short: "Close pooled instances for one ACP Agent thread", Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().CloseACPThread(context.Background(), args[0], args[1])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

func init() {
	gatewayACPRuntimeCmd.AddCommand(gatewayACPRuntimeGetCmd, gatewayACPRuntimeInFlightCmd, gatewayACPRuntimeCloseThreadCmd)
	gatewayCmd.AddCommand(gatewayACPRuntimeCmd)
}
