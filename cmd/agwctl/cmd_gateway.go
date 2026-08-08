package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-guide/agent-gateway/pkg/adminclient"
	"github.com/agent-guide/agent-gateway/pkg/gatewaybundle"
	"github.com/spf13/cobra"

	_ "github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/anthropic"
	_ "github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/cc"
	_ "github.com/agent-guide/agent-gateway/pkg/dispatcher/llmapi/openai"
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

var (
	gwAdminBasicAuth string
	gwAdminHeaders   []string

	gatewayBundleFile string
	gatewayExportFile string

	gatewayManagedModelProviderID string
	gatewayCredentialType         string
	gatewayCredentialProviderType string
	gatewayCredentialProviderID   string
	gatewayMCPRuntimeRouteID      string
	gatewayMCPToolArguments       string
	gatewayMCPPromptArguments     string
)

var gatewayValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate a gateway bundle YAML file locally",
	RunE: func(cmd *cobra.Command, args []string) error {
		if gatewayBundleFile == "" {
			return fmt.Errorf("--file is required")
		}
		bundle, err := gatewaybundle.LoadFile(gatewayBundleFile)
		if err != nil {
			return err
		}
		if err := bundle.ValidateForConfigStore(); err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(map[string]any{
				"status": "ok",
				"file":   gatewayBundleFile,
				"counts": map[string]int{
					"providers":      len(bundle.Providers),
					"managed_models": len(bundle.ManagedModels),
					"llm_routes":     len(bundle.LLMRoutes),
					"virtual_keys":   len(bundle.VirtualKeys),
					"mcp_services":   len(bundle.MCPServices),
					"mcp_routes":     len(bundle.MCPRoutes),
					"agent_routes":   len(bundle.AgentRoutes),
					"agents":         len(bundle.Agents),
				},
			})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "gateway bundle is valid: %s\n", gatewayBundleFile)
		return nil
	},
}

var gatewayApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply a gateway bundle YAML file to the remote admin API",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runGatewayApply(context.Background(), gatewayBundleFile)
	},
}

var gatewayExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export gateway objects from the remote admin API as bundle YAML",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runGatewayExport(context.Background(), gatewayExportFile)
	},
}

// ── gateway provider ─────────────────────────────────────────────────────────

var gatewayProviderCmd = &cobra.Command{
	Use:   "provider",
	Short: "Manage gateway providers",
}

var gatewayProviderListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all providers",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListProviders(context.Background(), adminclient.ProviderListOptions{})
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayProvidersTable(items)
		return nil
	},
}

var gatewayProviderGetCmd = &cobra.Command{
	Use:   "get <provider-id>",
	Short: "Get one provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetProvider(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayProviderDeleteCmd = &cobra.Command{
	Use:   "delete <provider-id>",
	Short: "Delete a provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().DeleteProvider(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayProviderEnableCmd = &cobra.Command{
	Use:   "enable <provider-id>",
	Short: "Enable a provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().EnableProvider(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayProviderDisableCmd = &cobra.Command{
	Use:   "disable <provider-id>",
	Short: "Disable a provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().DisableProvider(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

// ── gateway llm route ─────────────────────────────────────────────────────────

var gatewayLLMRouteCmd = &cobra.Command{
	Use:   "llm-route",
	Short: "Manage gateway LLM routes",
}

var gatewayLLMRouteListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all LLM routes",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListLLMRoutes(context.Background(), adminclient.LLMRouteListOptions{})
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayLLMRoutesTable(items)
		return nil
	},
}

var gatewayLLMRouteGetCmd = &cobra.Command{
	Use:   "get <llm-route-id>",
	Short: "Get one LLM route",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetLLMRoute(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayLLMRouteDeleteCmd = &cobra.Command{
	Use:   "delete <llm-route-id>",
	Short: "Delete an LLM route",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().DeleteLLMRoute(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayLLMRouteEnableCmd = &cobra.Command{
	Use:   "enable <llm-route-id>",
	Short: "Enable an LLM route",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().EnableLLMRoute(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayLLMRouteDisableCmd = &cobra.Command{
	Use:   "disable <llm-route-id>",
	Short: "Disable an LLM route",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().DisableLLMRoute(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

// ── gateway mcp service ──────────────────────────────────────────────────────

var gatewayMCPServiceCmd = &cobra.Command{
	Use:   "mcp-service",
	Short: "Manage gateway MCP services",
}

var gatewayMCPServiceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all MCP services",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListMCPServices(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayMCPServicesTable(items)
		return nil
	},
}

var gatewayMCPServiceGetCmd = &cobra.Command{
	Use:   "get <mcp-service-id>",
	Short: "Get one MCP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetMCPService(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayMCPServiceDeleteCmd = &cobra.Command{
	Use:   "delete <mcp-service-id>",
	Short: "Delete an MCP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().DeleteMCPService(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayMCPServiceSessionCmd = &cobra.Command{
	Use:   "session <mcp-service-id>",
	Short: "Get the active gateway session for an MCP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetMCPServiceSession(context.Background(), args[0])
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(item)
		}
		printGatewayMCPSessionTable(item)
		return nil
	},
}

var gatewayMCPServiceCapabilitiesCmd = &cobra.Command{
	Use:   "capabilities <mcp-service-id>",
	Short: "Get the upstream initialize payload for an MCP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetMCPServiceCapabilities(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayMCPServiceToolsCmd = &cobra.Command{
	Use:   "tools <mcp-service-id>",
	Short: "List tools exposed by an MCP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListMCPServiceTools(context.Background(), args[0])
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayMCPToolsTable(items)
		return nil
	},
}

var gatewayMCPServiceToolCallCmd = &cobra.Command{
	Use:   "tool-call <mcp-service-id> <tool-name>",
	Short: "Call a tool exposed by an MCP service",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		arguments, err := parseOptionalJSONObjectFlag(gatewayMCPToolArguments, "--arguments")
		if err != nil {
			return err
		}
		item, err := newGatewayClient().CallMCPServiceTool(context.Background(), args[0], adminclient.MCPToolCallRequest{
			Name:      args[1],
			Arguments: arguments,
		})
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayMCPServiceResourcesCmd = &cobra.Command{
	Use:   "resources <mcp-service-id>",
	Short: "List resources exposed by an MCP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListMCPServiceResources(context.Background(), args[0])
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayMCPResourcesTable(items)
		return nil
	},
}

var gatewayMCPServiceResourceTemplatesCmd = &cobra.Command{
	Use:   "resource-templates <mcp-service-id>",
	Short: "List resource templates exposed by an MCP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListMCPServiceResourceTemplates(context.Background(), args[0])
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayMCPResourceTemplatesTable(items)
		return nil
	},
}

var gatewayMCPServiceResourceReadCmd = &cobra.Command{
	Use:   "resource-read <mcp-service-id> <resource-uri>",
	Short: "Read one resource exposed by an MCP service",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().ReadMCPServiceResource(context.Background(), args[0], adminclient.MCPResourceReadRequest{
			URI: args[1],
		})
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayMCPServicePromptsCmd = &cobra.Command{
	Use:   "prompts <mcp-service-id>",
	Short: "List prompts exposed by an MCP service",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListMCPServicePrompts(context.Background(), args[0])
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayMCPPromptsTable(items)
		return nil
	},
}

var gatewayMCPServicePromptGetCmd = &cobra.Command{
	Use:   "prompt-get <mcp-service-id> <prompt-name>",
	Short: "Get one prompt from an MCP service",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		arguments, err := parseOptionalJSONObjectFlag(gatewayMCPPromptArguments, "--arguments")
		if err != nil {
			return err
		}
		item, err := newGatewayClient().GetMCPServicePrompt(context.Background(), args[0], adminclient.MCPPromptGetRequest{
			Name:      args[1],
			Arguments: arguments,
		})
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

// ── gateway mcp route ────────────────────────────────────────────────────────

var gatewayMCPRouteCmd = &cobra.Command{
	Use:   "mcp-route",
	Short: "Manage gateway MCP routes",
}

var gatewayMCPRouteListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all MCP routes",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListMCPRoutes(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayMCPRoutesTable(items)
		return nil
	},
}

var gatewayMCPRouteGetCmd = &cobra.Command{
	Use:   "get <mcp-route-id>",
	Short: "Get one MCP route",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetMCPRoute(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayMCPRouteDeleteCmd = &cobra.Command{
	Use:   "delete <mcp-route-id>",
	Short: "Delete an MCP route",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().DeleteMCPRoute(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── gateway mcp runtime ──────────────────────────────────────────────────────

var gatewayMCPRuntimeCmd = &cobra.Command{
	Use:   "mcp-runtime",
	Short: "Inspect gateway MCP runtime state",
}

var gatewayMCPRuntimeGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Get the MCP runtime overview",
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetMCPRuntime(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(item)
		}
		printGatewayMCPRuntimeOverview(item)
		return nil
	},
}

var gatewayMCPRuntimeInFlightCmd = &cobra.Command{
	Use:   "inflight",
	Short: "List in-flight MCP requests",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListMCPRuntimeInFlight(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayMCPRuntimeInFlightTable(items)
		return nil
	},
}

var gatewayMCPRuntimeProgressCmd = &cobra.Command{
	Use:   "progress",
	Short: "List MCP progress notifications",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListMCPRuntimeProgress(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayMCPRuntimeProgressTable(items)
		return nil
	},
}

var gatewayMCPRuntimeHistoryCmd = &cobra.Command{
	Use:   "history",
	Short: "List completed MCP request history",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListMCPRuntimeHistory(context.Background(), gatewayMCPRuntimeRouteID)
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayMCPRuntimeHistoryTable(items)
		return nil
	},
}

// ── gateway virtualkey ───────────────────────────────────────────────────────

var gatewayVirtualKeyCmd = &cobra.Command{
	Use:   "virtualkey",
	Short: "Manage gateway virtual keys",
}

var gatewayVirtualKeyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all virtual keys",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListVirtualKeys(context.Background(), adminclient.VirtualKeyListOptions{})
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayVirtualKeysTable(items)
		return nil
	},
}

var gatewayVirtualKeyGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get one virtual key by id",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetVirtualKey(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayVirtualKeyDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a virtual key by id",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().DeleteVirtualKey(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayVirtualKeyEnableCmd = &cobra.Command{
	Use:   "enable <id>",
	Short: "Enable a virtual key by id",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().EnableVirtualKey(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayVirtualKeyDisableCmd = &cobra.Command{
	Use:   "disable <id>",
	Short: "Disable a virtual key by id",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().DisableVirtualKey(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

// ── gateway credential ────────────────────────────────────────────────────────

var gatewayCredentialCmd = &cobra.Command{
	Use:   "credential",
	Short: "Manage gateway credentials",
}

// ── gateway models ───────────────────────────────────────────────────────────

var gatewayModelsCmd = &cobra.Command{
	Use:   "model",
	Short: "Manage gateway models",
}

// ── gateway provider types ───────────────────────────────────────────────────

var gatewayProviderTypesCmd = &cobra.Command{
	Use:   "provider-type",
	Short: "Inspect gateway provider types",
}

var gatewayProviderTypesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List provider types known to the gateway runtime",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListProviderTypes(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayProviderTypesTable(items)
		return nil
	},
}

// ── gateway llm api handler types ────────────────────────────────────────────

var gatewayLLMAPIHandlerTypesCmd = &cobra.Command{
	Use:   "llm-api-handler-type",
	Short: "Manage gateway LLM API handler types",
}

var gatewayLLMAPIHandlerTypesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List LLM API handler types known to the gateway runtime",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListLLMAPIHandlerTypes(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayLLMAPIHandlerTypesTable(items)
		return nil
	},
}

var gatewayModelsDiscoveredCmd = &cobra.Command{
	Use:   "discovered",
	Short: "Manage discovered provider models",
}

var gatewayModelsDiscoveredListCmd = &cobra.Command{
	Use:   "list <provider-id>",
	Short: "List discovered models for one provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListDiscoveredModels(context.Background(), args[0])
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayDiscoveredModelsTable(items)
		return nil
	},
}

var gatewayModelsDiscoveredRefreshCmd = &cobra.Command{
	Use:   "refresh <provider-id>",
	Short: "Refresh discovered models for one provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().RefreshProviderModels(context.Background(), args[0])
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(resp)
		}
		printGatewayDiscoveredModelsTable(resp.Items)
		return nil
	},
}

var gatewayModelsManagedCmd = &cobra.Command{
	Use:   "managed",
	Short: "Manage managed model overlays",
}

var gatewayModelsManagedListCmd = &cobra.Command{
	Use:   "list",
	Short: "List managed models",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListManagedModels(context.Background(), adminclient.ManagedModelListOptions{
			ProviderID: gatewayManagedModelProviderID,
		})
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayManagedModelsTable(items)
		return nil
	},
}

var gatewayModelsManagedGetCmd = &cobra.Command{
	Use:   "get <provider-id> <upstream-model>",
	Short: "Get one managed model overlay",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetManagedModel(context.Background(), args[0], args[1])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayModelsManagedDeleteCmd = &cobra.Command{
	Use:   "delete <provider-id> <upstream-model>",
	Short: "Delete a managed model overlay",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().DeleteManagedModel(context.Background(), args[0], args[1])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

var gatewayCredentialListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all credentials",
	RunE: func(cmd *cobra.Command, args []string) error {
		items, err := newGatewayClient().ListCredentials(context.Background(), adminclient.CredentialListOptions{
			Type:         gatewayCredentialType,
			ProviderType: gatewayCredentialProviderType,
			ProviderID:   gatewayCredentialProviderID,
		})
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(items)
		}
		printGatewayCredentialsTable(items)
		return nil
	},
}

var gatewayCredentialGetCmd = &cobra.Command{
	Use:   "get <credential-id>",
	Short: "Get one credential",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		item, err := newGatewayClient().GetCredential(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(item)
	},
}

var gatewayCredentialDeleteCmd = &cobra.Command{
	Use:   "delete <credential-id>",
	Short: "Delete a credential",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := newGatewayClient().DeleteCredential(context.Background(), args[0])
		if err != nil {
			return err
		}
		return printJSON(resp)
	},
}

func newGatewayClient() *adminclient.Client {
	return adminclient.New(adminclient.Config{
		BaseURL:   globalGatewayAddr,
		BasicAuth: gwAdminBasicAuth,
		Headers:   gwAdminHeaders,
	})
}

func parseOptionalJSONObjectFlag(raw string, flagName string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object: %w", flagName, err)
	}
	if payload == nil {
		return nil, fmt.Errorf("%s must be a JSON object", flagName)
	}
	return payload, nil
}

func init() {
	gatewayModelsManagedListCmd.Flags().StringVar(&gatewayManagedModelProviderID, "provider-id", "", "filter managed models by provider ID")
	gatewayCredentialListCmd.Flags().StringVar(&gatewayCredentialType, "type", "", "filter credentials by type (api_key or oauth_token)")
	gatewayCredentialListCmd.Flags().StringVar(&gatewayCredentialProviderType, "provider-type", "", "filter credentials by provider type")
	gatewayCredentialListCmd.Flags().StringVar(&gatewayCredentialProviderID, "provider-id", "", "filter credentials by provider ID")
	gatewayMCPRuntimeHistoryCmd.Flags().StringVar(&gatewayMCPRuntimeRouteID, "route-id", "", "filter completed MCP requests by route ID")
	gatewayMCPServiceToolCallCmd.Flags().StringVar(&gatewayMCPToolArguments, "arguments", "", "JSON object passed as tool arguments")
	gatewayMCPServicePromptGetCmd.Flags().StringVar(&gatewayMCPPromptArguments, "arguments", "", "JSON object passed as prompt arguments")
	gatewayValidateCmd.Flags().StringVarP(&gatewayBundleFile, "file", "f", "", "path to gateway bundle YAML file")
	gatewayApplyCmd.Flags().StringVarP(&gatewayBundleFile, "file", "f", "", "path to gateway bundle YAML file")
	gatewayExportCmd.Flags().StringVarP(&gatewayExportFile, "file", "f", "", "write bundle YAML to this file instead of stdout")

	gatewayProviderCmd.AddCommand(
		gatewayProviderListCmd,
		gatewayProviderGetCmd,
		gatewayProviderDeleteCmd,
		gatewayProviderEnableCmd,
		gatewayProviderDisableCmd,
	)
	gatewayLLMRouteCmd.AddCommand(
		gatewayLLMRouteListCmd,
		gatewayLLMRouteGetCmd,
		gatewayLLMRouteDeleteCmd,
		gatewayLLMRouteEnableCmd,
		gatewayLLMRouteDisableCmd,
	)
	gatewayMCPServiceCmd.AddCommand(
		gatewayMCPServiceListCmd,
		gatewayMCPServiceGetCmd,
		gatewayMCPServiceDeleteCmd,
		gatewayMCPServiceSessionCmd,
		gatewayMCPServiceCapabilitiesCmd,
		gatewayMCPServiceToolsCmd,
		gatewayMCPServiceToolCallCmd,
		gatewayMCPServiceResourcesCmd,
		gatewayMCPServiceResourceTemplatesCmd,
		gatewayMCPServiceResourceReadCmd,
		gatewayMCPServicePromptsCmd,
		gatewayMCPServicePromptGetCmd,
	)
	gatewayMCPRouteCmd.AddCommand(
		gatewayMCPRouteListCmd,
		gatewayMCPRouteGetCmd,
		gatewayMCPRouteDeleteCmd,
	)
	gatewayMCPRuntimeCmd.AddCommand(
		gatewayMCPRuntimeGetCmd,
		gatewayMCPRuntimeInFlightCmd,
		gatewayMCPRuntimeProgressCmd,
		gatewayMCPRuntimeHistoryCmd,
	)
	gatewayVirtualKeyCmd.AddCommand(
		gatewayVirtualKeyListCmd,
		gatewayVirtualKeyGetCmd,
		gatewayVirtualKeyDeleteCmd,
		gatewayVirtualKeyEnableCmd,
		gatewayVirtualKeyDisableCmd,
	)
	gatewayCredentialCmd.AddCommand(
		gatewayCredentialListCmd,
		gatewayCredentialGetCmd,
		gatewayCredentialDeleteCmd,
	)
	gatewayModelsDiscoveredCmd.AddCommand(
		gatewayModelsDiscoveredListCmd,
		gatewayModelsDiscoveredRefreshCmd,
	)
	gatewayModelsManagedCmd.AddCommand(
		gatewayModelsManagedListCmd,
		gatewayModelsManagedGetCmd,
		gatewayModelsManagedDeleteCmd,
	)
	gatewayModelsCmd.AddCommand(gatewayModelsDiscoveredCmd, gatewayModelsManagedCmd)
	gatewayProviderTypesCmd.AddCommand(gatewayProviderTypesListCmd)
	gatewayLLMAPIHandlerTypesCmd.AddCommand(
		gatewayLLMAPIHandlerTypesListCmd,
	)
	rootCmd.AddCommand(
		gatewayValidateCmd,
		gatewayApplyCmd,
		gatewayExportCmd,
		gatewayProviderCmd,
		gatewayLLMRouteCmd,
		gatewayMCPServiceCmd,
		gatewayMCPRouteCmd,
		gatewayMCPRuntimeCmd,
		gatewayVirtualKeyCmd,
		gatewayCredentialCmd,
		gatewayProviderTypesCmd,
		gatewayLLMAPIHandlerTypesCmd,
		gatewayModelsCmd,
	)
}
