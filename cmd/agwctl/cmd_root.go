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
	Short: "Agent Gateway CLI — manage Gateway and Caddy state",
}

func init() {
	initOutputFlag()
}
