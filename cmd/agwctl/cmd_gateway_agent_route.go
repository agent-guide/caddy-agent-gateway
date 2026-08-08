package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/agent-guide/agent-gateway/pkg/gateway/agentroute"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var gatewayAgentRouteCmd = &cobra.Command{Use: "agent-route", Short: "Manage unified Agent ingress routes"}
var gatewayAgentRouteFile string
var gatewayAgentRouteListCmd = &cobra.Command{Use: "list", Short: "List Agent routes", RunE: func(cmd *cobra.Command, args []string) error {
	items, err := newGatewayClient().ListAgentRoutes(context.Background())
	if err != nil {
		return err
	}
	return printJSON(items)
}}
var gatewayAgentRouteGetCmd = &cobra.Command{Use: "get <agent-route-id>", Short: "Get one Agent route", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	item, err := newGatewayClient().GetAgentRoute(context.Background(), args[0])
	if err != nil {
		return err
	}
	return printJSON(item)
}}
var gatewayAgentRouteDeleteCmd = &cobra.Command{Use: "delete <agent-route-id>", Short: "Delete one Agent route", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	item, err := newGatewayClient().DeleteAgentRoute(context.Background(), args[0])
	if err != nil {
		return err
	}
	return printJSON(item)
}}
var gatewayAgentRouteCreateCmd = &cobra.Command{Use: "create", Short: "Create an Agent route from --file", RunE: func(cmd *cobra.Command, args []string) error {
	cfg, err := loadAgentRouteFile()
	if err != nil {
		return err
	}
	item, err := newGatewayClient().CreateAgentRoute(context.Background(), cfg)
	if err != nil {
		return err
	}
	return printJSON(item)
}}
var gatewayAgentRouteUpdateCmd = &cobra.Command{Use: "update <agent-route-id>", Short: "Update an Agent route from --file", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	cfg, err := loadAgentRouteFile()
	if err != nil {
		return err
	}
	item, err := newGatewayClient().UpdateAgentRoute(context.Background(), args[0], cfg)
	if err != nil {
		return err
	}
	return printJSON(item)
}}

func loadAgentRouteFile() (agentroute.AgentRouteConfig, error) {
	if gatewayAgentRouteFile == "" {
		return agentroute.AgentRouteConfig{}, fmt.Errorf("--file is required")
	}
	data, err := os.ReadFile(gatewayAgentRouteFile)
	if err != nil {
		return agentroute.AgentRouteConfig{}, err
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return agentroute.AgentRouteConfig{}, err
	}
	data, err = json.Marshal(raw)
	if err != nil {
		return agentroute.AgentRouteConfig{}, err
	}
	var cfg agentroute.AgentRouteConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func init() {
	gatewayAgentRouteCreateCmd.Flags().StringVarP(&gatewayAgentRouteFile, "file", "f", "", "Agent route YAML or JSON")
	gatewayAgentRouteUpdateCmd.Flags().StringVarP(&gatewayAgentRouteFile, "file", "f", "", "Agent route YAML or JSON")
	gatewayAgentRouteCmd.AddCommand(gatewayAgentRouteListCmd, gatewayAgentRouteGetCmd, gatewayAgentRouteCreateCmd, gatewayAgentRouteUpdateCmd, gatewayAgentRouteDeleteCmd)
	rootCmd.AddCommand(gatewayAgentRouteCmd)
}
