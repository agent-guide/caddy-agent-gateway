package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/agent-guide/agent-gateway/pkg/adminclient"
	"github.com/agent-guide/agent-gateway/pkg/gatewaybundle"
)

func runGatewayExport(ctx context.Context, path string) error {
	client := newGatewayClient()

	providers, err := client.ListProviders(ctx, adminclient.ProviderListOptions{})
	if err != nil {
		return err
	}
	managedModels, err := client.ListManagedModels(ctx, adminclient.ManagedModelListOptions{})
	if err != nil {
		return err
	}
	llmRoutes, err := client.ListLLMRoutes(ctx, adminclient.LLMRouteListOptions{})
	if err != nil {
		return err
	}
	virtualKeys, err := client.ListVirtualKeys(ctx, adminclient.VirtualKeyListOptions{})
	if err != nil {
		return err
	}
	mcpServices, err := client.ListMCPServices(ctx)
	if err != nil {
		return err
	}
	mcpRoutes, err := client.ListMCPRoutes(ctx)
	if err != nil {
		return err
	}
	agentRoutes, err := client.ListAgentRoutes(ctx)
	if err != nil {
		return err
	}
	agents, err := client.ListAgents(ctx)
	if err != nil {
		return err
	}

	bundle := &gatewaybundle.GatewayBundle{
		APIVersion: gatewaybundle.APIVersionV1Alpha1,
		Kind:       gatewaybundle.KindGatewayBundle,
	}
	for _, item := range providers {
		bundle.Providers = append(bundle.Providers, item.ProviderConfig)
	}
	for _, item := range managedModels {
		bundle.ManagedModels = append(bundle.ManagedModels, item.ManagedModel)
	}
	for _, item := range llmRoutes {
		routeCfg, err := item.LLMRouteConfig()
		if err != nil {
			return fmt.Errorf("decode llm route %q: %w", item.ID, err)
		}
		cfg, err := routeCfg.ToConfig()
		if err != nil {
			return fmt.Errorf("encode llm route %q: %w", item.ID, err)
		}
		bundle.LLMRoutes = append(bundle.LLMRoutes, cfg)
	}
	for _, item := range virtualKeys {
		bundle.VirtualKeys = append(bundle.VirtualKeys, gatewaybundle.BundleVirtualKeyFromRuntime(item.VirtualKey))
	}
	for _, item := range mcpServices {
		bundle.MCPServices = append(bundle.MCPServices, item.MCPServiceConfig)
	}
	for _, item := range mcpRoutes {
		bundle.MCPRoutes = append(bundle.MCPRoutes, item.MCPRouteConfig)
	}
	for _, item := range agentRoutes {
		bundle.AgentRoutes = append(bundle.AgentRoutes, item.AgentRouteConfig)
	}
	for _, item := range agents {
		bundle.Agents = append(bundle.Agents, item.Agent)
	}

	sortGatewayBundle(bundle)
	yamlBytes, err := gatewaybundle.EncodeYAML(bundle)
	if err != nil {
		return err
	}
	if path == "" {
		_, err = cmdOrStdout().Write(yamlBytes)
		return err
	}
	if err := os.WriteFile(path, yamlBytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if outputFormat == "json" {
		return printJSON(map[string]any{
			"status": "ok",
			"file":   path,
		})
	}
	fmt.Fprintf(cmdOrStdout(), "gateway bundle exported: %s\n", path)
	return nil
}

func sortGatewayBundle(bundle *gatewaybundle.GatewayBundle) {
	sort.Slice(bundle.Providers, func(i, j int) bool {
		return bundle.Providers[i].Id < bundle.Providers[j].Id
	})
	sort.Slice(bundle.ManagedModels, func(i, j int) bool {
		if bundle.ManagedModels[i].ProviderID != bundle.ManagedModels[j].ProviderID {
			return bundle.ManagedModels[i].ProviderID < bundle.ManagedModels[j].ProviderID
		}
		return bundle.ManagedModels[i].UpstreamModel < bundle.ManagedModels[j].UpstreamModel
	})
	sort.Slice(bundle.LLMRoutes, func(i, j int) bool {
		return bundle.LLMRoutes[i].ID < bundle.LLMRoutes[j].ID
	})
	sort.Slice(bundle.VirtualKeys, func(i, j int) bool {
		return bundle.VirtualKeys[i].ID < bundle.VirtualKeys[j].ID
	})
	sort.Slice(bundle.MCPServices, func(i, j int) bool {
		return bundle.MCPServices[i].ID < bundle.MCPServices[j].ID
	})
	sort.Slice(bundle.MCPRoutes, func(i, j int) bool {
		return bundle.MCPRoutes[i].ID < bundle.MCPRoutes[j].ID
	})
	sort.Slice(bundle.AgentRoutes, func(i, j int) bool {
		return bundle.AgentRoutes[i].ID < bundle.AgentRoutes[j].ID
	})
	sort.Slice(bundle.Agents, func(i, j int) bool {
		return bundle.Agents[i].ID < bundle.Agents[j].ID
	})
}

func cmdOrStdout() *os.File {
	return os.Stdout
}
