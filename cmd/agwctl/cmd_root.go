package main

import (
	"github.com/spf13/cobra"
)

var (
	globalCaddyAdmin  string
	globalGatewayAddr string
)

var rootCmd = &cobra.Command{
	Use:   "agwctl",
	Short: "Manage Agent Gateway control-plane resources",
}

func init() {
	initOutputFlag()
	rootCmd.PersistentFlags().StringVar(&globalGatewayAddr, "admin-addr", envOr("AGW_ADMIN_ADDR", "http://localhost:8019"), "agent-gateway admin API address")
	rootCmd.PersistentFlags().StringVar(&gwAdminBasicAuth, "admin-basic-auth", envOr("AGW_ADMIN_BASIC_AUTH", ""), "gateway admin Basic Auth request credentials as username:password")
	rootCmd.PersistentFlags().StringArrayVar(&gwAdminHeaders, "admin-header", nil, "extra admin API header as 'Name: value'; repeat to send multiple headers")
}
