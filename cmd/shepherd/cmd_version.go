package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
)

var (
	// Version holds the semantic version of Shepherd.
	Version = "0.6.0"
	// CommitSHA holds the git commit hash set at build time.
	CommitSHA = "dev"
	// BuildDate holds the ISO-8601 build timestamp.
	BuildDate = "unknown"
)

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	selfPath, err := os.Executable()
	selfHash := "unknown"
	if err == nil {
		if data, err := os.ReadFile(selfPath); err == nil {
			h := sha256.Sum256(data)
			selfHash = hex.EncodeToString(h[:])
		}
	}

	fmt.Fprintf(stdout, "Shepherd version %s\n", Version)
	fmt.Fprintf(stdout, "  Commit:     %s\n", CommitSHA)
	fmt.Fprintf(stdout, "  Build Date: %s\n", BuildDate)
	fmt.Fprintf(stdout, "  Go Runtime: %s (%s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(stdout, "  SHA-256:    %s\n", selfHash)
	return 0
}
