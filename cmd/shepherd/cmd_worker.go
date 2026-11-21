package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/erinwnash/Shepherd/internal/ca"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/pairing"
)

func parsePositionalAndFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flagArgs = append(flagArgs, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		} else {
			positional = append(positional, args[i])
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	return positional, nil
}

func cmdWorker(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: shepherd worker <add|join|ls|revoke|drain> [args...]")
		return 2
	}

	switch args[0] {
	case "add":
		return cmdWorkerAdd(args[1:], stdout, stderr)
	case "join":
		return cmdWorkerJoin(args[1:], stdout, stderr)
	case "ls", "list":
		return cmdWorkerList(args[1:], stdout, stderr)
	case "revoke":
		return cmdWorkerRevoke(args[1:], stdout, stderr)
	case "drain":
		return cmdWorkerDrain(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown worker subcommand %q\n", args[0])
		return 2
	}
}

func cmdWorkerAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shepherd worker add", flag.ContinueOnError)
	fs.SetOutput(stderr)

	addr := fs.String("addr", "127.0.0.1:8443", "LAN controller listen address (explicit IP:port, not 0.0.0.0)")
	caDir := fs.String("ca-dir", filepath.Join(".shepherd", "ca"), "path to CA key and cert directory")
	dbPath := fs.String("db", filepath.Join(".shepherd", "shepherd.db"), "path to controller SQLite database")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Load or initialize CA
	certPath := filepath.Join(*caDir, "ca.crt")
	keyPath := filepath.Join(*caDir, "ca.key")
	var rootCA *ca.CA
	var err error

	if _, statErr := os.Stat(certPath); statErr == nil {
		rootCA, err = ca.LoadCA(certPath, keyPath)
	} else {
		rootCA, err = ca.GenerateCA("Shepherd Root CA")
		if err == nil {
			_ = rootCA.Save(certPath, keyPath)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "error loading/generating CA: %v\n", err)
		return 1
	}

	store, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "error opening state db: %v\n", err)
		return 1
	}
	defer store.Close()

	mgr := pairing.NewManager(rootCA, store)
	tokenStr, err := mgr.CreateToken(*addr)
	if err != nil {
		fmt.Fprintf(stderr, "error generating pairing token: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "\nPairing challenge created (10-minute expiry, single use per ADR-0003):\n\n")
	fmt.Fprintf(stdout, "  PAIRING STRING:\n    %s\n\n", tokenStr)
	fmt.Fprintf(stdout, "To enroll a worker node, execute on the remote machine:\n")
	fmt.Fprintf(stdout, "  shepherd worker join %s\n\n", tokenStr)
	return 0
}

func cmdWorkerJoin(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shepherd worker join", flag.ContinueOnError)
	fs.SetOutput(stderr)

	workerID := fs.String("id", "", "unique identifier for this worker (default hostname)")
	outDir := fs.String("dir", filepath.Join(".shepherd", "worker"), "directory to store worker certificate and key")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "usage: shepherd worker join <pairing-string> [--id <worker-id>] [--dir <dir>]")
		return 2
	}
	pairingString := fs.Arg(0)

	id := *workerID
	if id == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = fmt.Sprintf("worker-%d", time.Now().Unix())
		}
		id = host
	}

	certPath := filepath.Join(*outDir, "worker.crt")
	keyPath := filepath.Join(*outDir, "worker.key")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := pairing.ClientPair(ctx, pairingString, id, certPath, keyPath)
	if err != nil {
		fmt.Fprintf(stderr, "pairing failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Successfully enrolled worker with Shepherd controller!\n")
	fmt.Fprintf(stdout, "Worker ID:   %s\n", resp.WorkerID)
	fmt.Fprintf(stdout, "Public Key:  %s\n", resp.PubkeyHash)
	fmt.Fprintf(stdout, "Certificate: %s\n", certPath)
	fmt.Fprintf(stdout, "Private Key: %s (mode 0600)\n", keyPath)
	return 0
}

func cmdWorkerList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shepherd worker ls", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbPath := fs.String("db", filepath.Join(".shepherd", "shepherd.db"), "path to controller SQLite database")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	store, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "error opening database: %v\n", err)
		return 1
	}
	defer store.Close()

	workers, err := store.ListWorkers()
	if err != nil {
		fmt.Fprintf(stderr, "error listing workers: %v\n", err)
		return 1
	}

	if len(workers) == 0 {
		fmt.Fprintln(stdout, "No workers registered. Run 'shepherd worker add' to generate a pairing token.")
		return 0
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tROLE\tSTATUS\tEXPIRES\tKEY HASH")
	for _, wk := range workers {
		shortHash := wk.PubkeyHash
		if len(shortHash) > 12 {
			shortHash = shortHash[:12] + "..."
		}
		expires := wk.ExpiresAt.Format("2006-01-02 15:04:05")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", wk.ID, wk.Role, wk.Status, expires, shortHash)
	}
	_ = w.Flush()
	return 0
}

func cmdWorkerRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shepherd worker revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbPath := fs.String("db", filepath.Join(".shepherd", "shepherd.db"), "path to controller SQLite database")
	pos, err := parsePositionalAndFlags(fs, args)
	if err != nil {
		return 2
	}

	if len(pos) < 1 {
		fmt.Fprintln(stderr, "usage: shepherd worker revoke <worker-id>")
		return 2
	}
	workerID := pos[0]

	store, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "error opening database: %v\n", err)
		return 1
	}
	defer store.Close()

	if err := store.SetWorkerStatus(workerID, "revoked"); err != nil {
		fmt.Fprintf(stderr, "error revoking worker: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Worker %q revoked (STRIDE S4).\n", workerID)
	return 0
}

func cmdWorkerDrain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shepherd worker drain", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dbPath := fs.String("db", filepath.Join(".shepherd", "shepherd.db"), "path to controller SQLite database")
	pos, err := parsePositionalAndFlags(fs, args)
	if err != nil {
		return 2
	}

	if len(pos) < 1 {
		fmt.Fprintln(stderr, "usage: shepherd worker drain <worker-id>")
		return 2
	}
	workerID := pos[0]

	store, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "error opening database: %v\n", err)
		return 1
	}
	defer store.Close()

	if err := store.SetWorkerStatus(workerID, "drained"); err != nil {
		fmt.Fprintf(stderr, "error draining worker: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Worker %q set to status 'drained'.\n", workerID)
	return 0
}
