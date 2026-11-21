package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/erinwnash/Shepherd/internal/service"
)

func cmdService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: shepherd service <install|uninstall|status>")
		return 2
	}

	fl := flag.NewFlagSet("service", flag.ContinueOnError)
	fl.SetOutput(stderr)
	execPath := fl.String("exec", "", "path to shepherd binary (optional, auto-detected)")
	workDir := fl.String("dir", "", "working directory for daemon (optional, auto-detected)")
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}

	switch args[0] {
	case "install":
		msg, err := service.Install(*execPath, *workDir)
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: service install failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0

	case "uninstall":
		msg, err := service.Uninstall()
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: service uninstall failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0

	case "status":
		msg, err := service.Status()
		if err != nil {
			fmt.Fprintf(stderr, "shepherd: service status error: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0

	default:
		fmt.Fprintf(stderr, "unknown service action %q (want install, uninstall, or status)\n", args[0])
		return 2
	}
}
