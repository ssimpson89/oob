package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/ssimpson89/oob/internal/mcpserver"
)

// mcpCmd serves the host list and consoles to an AI agent over MCP on stdio.
func mcpCmd() *cobra.Command {
	var o mcpserver.Options
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Serve your hosts to an AI agent over MCP (stdio)",
		Long: "Run a Model Context Protocol server on stdin and stdout, for an AI agent such as Claude Code:\n" +
			"  claude mcp add " + AppName + " -- " + AppName + " mcp\n\n" +
			"Tools list hosts, read power state, the event log and sensors, and open a server's serial\n" +
			"console (IPMI SOL, iLO VSP, iDRAC console com2) to read it and type at it. Anything that\n" +
			"changes state (power, boot, uid, nmi, sel clear, BMC reset) asks you first through the\n" +
			"client, and is refused if the client can't ask. So do console commands that reboot, wipe or\n" +
			"format, and anything typed at a bootloader prompt or the BMC's own command line.\n\n" +
			"Passwords and keys stay inside " + AppName + ": tools take host names and never return credentials.\n" +
			"Nothing is written to stdout except the protocol. Console sessions close when the client\n" +
			"exits or after sitting idle. Use -c to point at another host file.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			o.Version = version
			return mcpserver.Run(ctx, o)
		},
	}
	c.Flags().BoolVar(&o.ReadOnly, "read-only", false, "only the read-only host tools: no power, boot or other changes, no console")
	c.Flags().BoolVar(&o.Log, "log", false, "save a log of every console session (what it printed and what was typed)")
	c.Flags().StringVar(&o.LogDir, "log-dir", "", "where --log writes (default: the "+AppName+" state directory's logs folder)")
	c.Flags().DurationVar(&o.IdleTimeout, "idle-timeout", 15*time.Minute, "close a console session no tool has used for this long")
	return c
}
