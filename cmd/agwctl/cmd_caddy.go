package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/agwctl/caddyadminclient"
	"github.com/spf13/cobra"
)

// ── caddy ─────────────────────────────────────────────────────────────────────

var caddyCmd = &cobra.Command{
	Use:   "caddy",
	Short: "Manage Caddy servers and routes via the Caddy admin API",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("admin-addr") {
			return fmt.Errorf("--admin-addr configures the Agent Gateway admin API; use --addr for the Caddy admin API")
		}
		return nil
	},
}

var rejectedCaddyAdminAddr string

// ── caddy server ──────────────────────────────────────────────────────────────

var caddyServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage Caddy HTTP servers",
}

var caddyServerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all Caddy HTTP servers",
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr := caddyadminclient.NewManager(globalCaddyAdmin)
		servers, err := mgr.ListServers(context.Background())
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(servers)
		}
		printCaddyServersTable(servers)
		return nil
	},
}

var caddyServerGetCmd = &cobra.Command{
	Use:   "get <server-id>",
	Short: "Get a Caddy HTTP server",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr := caddyadminclient.NewManager(globalCaddyAdmin)
		srv, err := mgr.GetServer(context.Background(), args[0])
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(srv)
		}
		printCaddyServersTable([]*caddyadminclient.ServerResponse{srv})
		return nil
	},
}

var (
	serverCreateID      string
	serverCreateListen  []string
	serverCreateTLSAuto bool
)

var caddyServerCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new Caddy HTTP server",
	RunE: func(cmd *cobra.Command, args []string) error {
		if serverCreateID == "" {
			return fmt.Errorf("--id is required")
		}
		if len(serverCreateListen) == 0 {
			return fmt.Errorf("--listen is required")
		}
		mgr := caddyadminclient.NewManager(globalCaddyAdmin)
		req := &caddyadminclient.ServerRequest{
			ID:     serverCreateID,
			Listen: serverCreateListen,
		}
		if serverCreateTLSAuto {
			req.TLS = &caddyadminclient.TLSConf{Auto: true}
		}
		if err := mgr.CreateServer(context.Background(), req); err != nil {
			return err
		}
		srv, err := mgr.GetServer(context.Background(), serverCreateID)
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(srv)
		}
		printCaddyServersTable([]*caddyadminclient.ServerResponse{srv})
		return nil
	},
}

var (
	serverUpdateListen  []string
	serverUpdateTLSAuto bool
)

var caddyServerUpdateCmd = &cobra.Command{
	Use:   "update <server-id>",
	Short: "Update an existing Caddy HTTP server",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(serverUpdateListen) == 0 {
			return fmt.Errorf("--listen is required")
		}
		mgr := caddyadminclient.NewManager(globalCaddyAdmin)
		req := &caddyadminclient.ServerRequest{
			ID:     args[0],
			Listen: serverUpdateListen,
		}
		if serverUpdateTLSAuto {
			req.TLS = &caddyadminclient.TLSConf{Auto: true}
		}
		if err := mgr.UpdateServer(context.Background(), req); err != nil {
			return err
		}
		srv, err := mgr.GetServer(context.Background(), args[0])
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(srv)
		}
		printCaddyServersTable([]*caddyadminclient.ServerResponse{srv})
		return nil
	},
}

var caddyServerDeleteCmd = &cobra.Command{
	Use:   "delete <server-id>",
	Short: "Delete a Caddy HTTP server",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr := caddyadminclient.NewManager(globalCaddyAdmin)
		if err := mgr.DeleteServer(context.Background(), args[0]); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "deleted server %q\n", args[0])
		return nil
	},
}

// ── caddy route ───────────────────────────────────────────────────────────────

var caddyRouteCmd = &cobra.Command{
	Use:   "route",
	Short: "Manage Caddy HTTP routes within a server",
}

var caddyRouteListCmd = &cobra.Command{
	Use:   "list <server-id>",
	Short: "List routes in a Caddy server",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr := caddyadminclient.NewManager(globalCaddyAdmin)
		routes, err := mgr.ListRoutes(context.Background(), args[0])
		if err != nil {
			return err
		}
		if outputFormat == "json" {
			return printJSON(routes)
		}
		printCaddyRoutesTable(routes)
		return nil
	},
}

var (
	routeAddID       string
	routeAddOrder    int
	routeAddPaths    []string
	routeAddHosts    []string
	routeAddHandlers []string
)

var caddyRouteAddCmd = &cobra.Command{
	Use:   "add <server-id>",
	Short: "Add a route to a Caddy server",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if routeAddID == "" {
			return fmt.Errorf("--id is required")
		}
		handlers, err := parseHandlers(routeAddHandlers)
		if err != nil {
			return err
		}
		mgr := caddyadminclient.NewManager(globalCaddyAdmin)
		req := &caddyadminclient.RouteRequest{
			ID:       routeAddID,
			Order:    routeAddOrder,
			Match:    caddyadminclient.MatchConf{Paths: routeAddPaths, Hosts: routeAddHosts},
			Handlers: handlers,
		}
		if err := mgr.AddRoute(context.Background(), args[0], req); err != nil {
			return err
		}
		routes, err := mgr.ListRoutes(context.Background(), args[0])
		if err != nil {
			return err
		}
		for _, r := range routes {
			if r.ID == routeAddID {
				if outputFormat == "json" {
					return printJSON(r)
				}
				printCaddyRoutesTable([]*caddyadminclient.RouteResponse{r})
				return nil
			}
		}
		fmt.Fprintf(os.Stdout, "added route %q\n", routeAddID)
		return nil
	},
}

var caddyRouteDeleteCmd = &cobra.Command{
	Use:   "delete <server-id> <route-id>",
	Short: "Delete a route from a Caddy server",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr := caddyadminclient.NewManager(globalCaddyAdmin)
		if err := mgr.DeleteRoute(context.Background(), args[0], args[1]); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "deleted route %q from server %q\n", args[1], args[0])
		return nil
	},
}

// parseHandlers parses handler descriptors of the form "type[:key=value,...]".
// Supported shorthand types: agent_route_dispatcher, admin, reverse_proxy, file_server.
// Examples:
//
//	agent_route_dispatcher:apis=openai,anthropic
//	reverse_proxy:upstream=localhost:9000
//	file_server:root=/var/www
//	admin
func parseHandlers(specs []string) ([]caddyadminclient.HandlerConf, error) {
	handlers := make([]caddyadminclient.HandlerConf, 0, len(specs))
	for _, spec := range specs {
		htype, params, _ := strings.Cut(spec, ":")
		conf := caddyadminclient.HandlerConf{Type: htype}
		if params != "" {
			for _, kv := range strings.Split(params, ",") {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "apis":
					conf.APIs = strings.Split(v, ";")
				case "upstream":
					conf.Upstream = v
				case "root":
					conf.Root = v
				default:
					return nil, fmt.Errorf("unknown handler param %q in %q", k, spec)
				}
			}
		}
		handlers = append(handlers, conf)
	}
	return handlers, nil
}

func init() {
	caddyCmd.PersistentFlags().StringVar(&globalCaddyAdmin, "addr", envOr("CADDY_ADMIN_ADDR", "http://localhost:2019"), "Caddy admin API address")
	// Shadow the root gateway flag so the former Caddy alias fails explicitly
	// instead of silently changing which admin endpoint it configures.
	caddyCmd.PersistentFlags().StringVar(&rejectedCaddyAdminAddr, "admin-addr", "", "")
	_ = caddyCmd.PersistentFlags().MarkHidden("admin-addr")

	// server create flags
	caddyServerCreateCmd.Flags().StringVar(&serverCreateID, "id", "", "server ID (required)")
	caddyServerCreateCmd.Flags().StringArrayVar(&serverCreateListen, "listen", nil, "listen addresses (repeatable)")
	caddyServerCreateCmd.Flags().BoolVar(&serverCreateTLSAuto, "tls-auto", false, "enable automatic TLS")

	// server update flags
	caddyServerUpdateCmd.Flags().StringArrayVar(&serverUpdateListen, "listen", nil, "listen addresses (repeatable)")
	caddyServerUpdateCmd.Flags().BoolVar(&serverUpdateTLSAuto, "tls-auto", false, "enable automatic TLS")

	// route add flags
	caddyRouteAddCmd.Flags().StringVar(&routeAddID, "id", "", "route ID (required)")
	caddyRouteAddCmd.Flags().IntVar(&routeAddOrder, "order", 0, "insertion position (0 = first)")
	caddyRouteAddCmd.Flags().StringArrayVar(&routeAddPaths, "path", nil, "match path (repeatable)")
	caddyRouteAddCmd.Flags().StringArrayVar(&routeAddHosts, "host", nil, "match host (repeatable)")
	caddyRouteAddCmd.Flags().StringArrayVar(&routeAddHandlers, "handler", nil, `handler spec: type[:key=val,...] (repeatable)`)

	caddyServerCmd.AddCommand(caddyServerListCmd, caddyServerGetCmd, caddyServerCreateCmd, caddyServerUpdateCmd, caddyServerDeleteCmd)
	caddyRouteCmd.AddCommand(caddyRouteListCmd, caddyRouteAddCmd, caddyRouteDeleteCmd)
	caddyCmd.AddCommand(caddyServerCmd, caddyRouteCmd)
	rootCmd.AddCommand(caddyCmd)
}
