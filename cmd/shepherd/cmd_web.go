package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/web"
)

func cmdWeb(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("web", flag.ContinueOnError)
	fl.SetOutput(stderr)
	addr := fl.String("addr", "127.0.0.1:8080", "web server listen address")
	port := fl.Int("port", 0, "port override (replaces port in --addr)")
	root := fl.String("root", ".", "project root")
	planRoot := fl.String("plan", "docs/plan", "plan directory")
	sockPath := fl.String("socket", control.DefaultSocketPath(), "path to unix domain control socket")

	if err := fl.Parse(args); err != nil {
		return 2
	}

	listenAddr := *addr
	if *port > 0 {
		listenAddr = fmt.Sprintf("127.0.0.1:%d", *port)
	}

	client := control.NewClient(*sockPath, "")
	srv := web.NewServer(listenAddr, client, *root, *planRoot)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(stdout, "Shepherd Web Dashboard listening on http://%s\n", listenAddr)
	fmt.Fprintln(stdout, "Press Ctrl+C to shut down.")

	if err := srv.Start(ctx); err != nil {
		fmt.Fprintf(stderr, "shepherd web: %v\n", err)
		return 1
	}

	return 0
}
