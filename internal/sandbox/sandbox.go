// Package sandbox provides OS-level process containment for agent CLIs and verifier gates per ADR-0006.
// Uses bubblewrap (bwrap) on Linux and sandbox-exec on macOS to isolate network, hide credentials,
// SSH keys, cloud profiles, and the Shepherd control socket.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrUnsandboxedDisallowed is returned when no sandbox runner is available and policy.allow_unsandboxed is false.
var ErrUnsandboxedDisallowed = errors.New("sandboxing required by policy (ADR-0006) but no supported sandbox (bwrap/sandbox-exec) found; set policy.allow_unsandboxed = true to bypass")

// Profile defines the containment boundaries for a chunk or gate execution.
type Profile struct {
	WorktreeDir string
	TempDir     string
	AdapterDirs []string
	// ReadOnlyAdapterDirs are adapter directories under $HOME that the agent may read but not
	// change (for example a CLI's config and credentials), mounted --ro-bind.
	ReadOnlyAdapterDirs []string
	MaskDirs            []string
	AllowUnsandboxed    bool
	AllowNetwork        bool     // if false, network namespace is isolated (--unshare-net, STRIDE B4)
	EgressAllowList     []string // permitted outbound hosts when network is enabled
	Backend             string   // "", "bwrap", "sandbox-exec", "podman", "docker"
	ContainerImage      string   // container image when Backend is "podman" or "docker"
}

// LookBwrap returns the absolute path to bubblewrap if found on PATH.
var LookBwrap = func() (string, error) {
	return exec.LookPath("bwrap")
}

// LookSandboxExec returns the path to sandbox-exec on macOS.
var LookSandboxExec = func() (string, error) {
	return exec.LookPath("sandbox-exec")
}

// LookContainer returns the path to podman or docker.
var LookContainer = func(runtimeName string) (string, error) {
	return exec.LookPath(runtimeName)
}

// CurrentOS allows mocking runtime.GOOS in tests.
var CurrentOS = runtime.GOOS

// Wrap builds a sandboxed argument slice wrapping argv according to the specified profile.
func Wrap(argv []string, p Profile) ([]string, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty argv cannot be sandboxed")
	}

	// 1. Explicit container backend (podman / docker)
	if p.Backend == "podman" || p.Backend == "docker" {
		rtPath, err := LookContainer(p.Backend)
		if err == nil {
			return buildContainerArgv(rtPath, argv, p)
		}
		if !p.AllowUnsandboxed {
			return nil, fmt.Errorf("%w: container runtime %q not found", ErrUnsandboxedDisallowed, p.Backend)
		}
	}

	// 2. Native OS sandboxes
	if CurrentOS == "linux" {
		bwrapPath, err := LookBwrap()
		if err == nil {
			return buildBwrapArgv(bwrapPath, argv, p)
		}
	} else if CurrentOS == "darwin" {
		sbPath, err := LookSandboxExec()
		if err == nil {
			return buildDarwinArgv(sbPath, argv, p)
		}
	}

	// Fallback when no platform sandbox is available
	if p.AllowUnsandboxed {
		return argv, nil
	}

	return nil, ErrUnsandboxedDisallowed
}

// effectiveMasks returns the paths to hide. A mask that contains the worktree (the front-run lane's
// checkout lives inside the masked .shepherd state dir) cannot be hidden whole without hiding the
// worktree too, so its other entries are masked one by one instead, recursing down to the worktree.
func effectiveMasks(p Profile) []string {
	wt := ""
	if p.WorktreeDir != "" {
		wt = absPath(p.WorktreeDir)
	}
	var out []string
	var add func(mask string)
	add = func(mask string) {
		if mask == "" {
			return
		}
		mask = absPath(mask)
		if wt == "" || mask == wt || !strings.HasPrefix(wt, mask+string(filepath.Separator)) {
			out = append(out, mask)
			return
		}
		entries, err := os.ReadDir(mask)
		if err != nil {
			return
		}
		for _, e := range entries {
			child := filepath.Join(mask, e.Name())
			if child == wt {
				continue
			}
			add(child)
		}
	}
	for _, m := range p.MaskDirs {
		add(m)
	}
	return out
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func isRegular(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && !st.IsDir()
}

func buildContainerArgv(rtPath string, argv []string, p Profile) ([]string, error) {
	image := p.ContainerImage
	if image == "" {
		image = "alpine:latest"
	}
	args := []string{
		rtPath, "run", "--rm", "-i",
		"--security-opt=no-new-privileges",
		"--cap-drop=ALL",
	}
	if !p.AllowNetwork {
		args = append(args, "--network=none")
	}
	if p.WorktreeDir != "" {
		absWorktree, err := filepath.Abs(p.WorktreeDir)
		if err != nil {
			absWorktree = p.WorktreeDir
		}
		args = append(args, "-v", fmt.Sprintf("%s:%s:Z", absWorktree, absWorktree), "-w", absWorktree)
	}
	for _, absMask := range effectiveMasks(p) {
		if isRegular(absMask) {
			args = append(args, "-v", "/dev/null:"+absMask+":ro")
		} else {
			args = append(args, "--tmpfs", absMask+":ro,noexec,nosuid,size=64k")
		}
	}
	args = append(args, image)
	args = append(args, argv...)
	return args, nil
}

func buildBwrapArgv(bwrapPath string, argv []string, p Profile) ([]string, error) {
	args := []string{
		bwrapPath,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}

	// Network isolation (STRIDE B4): unshare network namespace unless network is explicitly allowed
	if !p.AllowNetwork {
		args = append(args, "--unshare-net")
	}

	home, _ := os.UserHomeDir()
	if home != "" {
		// Mask the user's entire home directory
		args = append(args, "--tmpfs", home)

		// Preserve read-only user CLI toolchains (e.g. mise, ~/.local/bin, ~/.opencode/bin)
		toolchainDirs := []string{
			".local/share/mise",
			".local/bin",
			".opencode/bin",
			".gemini/antigravity-cli",
		}
		for _, td := range toolchainDirs {
			target := filepath.Join(home, td)
			if _, err := os.Stat(target); err == nil {
				args = append(args, "--ro-bind", target, target)
			}
		}

		// Selectively re-expose adapter directories inside $HOME
		for _, ad := range p.AdapterDirs {
			target := ad
			if !filepath.IsAbs(target) {
				target = filepath.Join(home, target)
			}
			if _, err := os.Stat(target); err == nil {
				args = append(args, "--bind", target, target)
			}
		}
		for _, ad := range p.ReadOnlyAdapterDirs {
			target := ad
			if !filepath.IsAbs(target) {
				target = filepath.Join(home, target)
			}
			if _, err := os.Stat(target); err == nil {
				args = append(args, "--ro-bind", target, target)
			}
		}
	}

	// Mount worktree read-write
	if p.WorktreeDir != "" {
		absWorktree, err := filepath.Abs(p.WorktreeDir)
		if err == nil {
			args = append(args, "--bind", absWorktree, absWorktree)
		} else {
			args = append(args, "--bind", p.WorktreeDir, p.WorktreeDir)
		}
	}

	// Mount private temp dir if specified
	if p.TempDir != "" {
		args = append(args, "--bind", p.TempDir, p.TempDir)
	}

	// Mask explicit protected paths (such as the shepherd state dir and control socket dir)
	for _, absMask := range effectiveMasks(p) {
		if isRegular(absMask) {
			args = append(args, "--ro-bind", "/dev/null", absMask)
		} else {
			args = append(args, "--tmpfs", absMask)
		}
	}

	if p.WorktreeDir != "" {
		args = append(args, "--chdir", absPath(p.WorktreeDir))
	}

	args = append(args, "--die-with-parent", "--")
	args = append(args, argv...)

	return args, nil
}

func escapeSBPLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func buildDarwinArgv(sbPath string, argv []string, p Profile) ([]string, error) {
	netRule := "(deny network*)"
	if p.AllowNetwork {
		netRule = "(allow network-outbound)"
	}

	var denyReads strings.Builder
	home, _ := os.UserHomeDir()
	if home != "" {
		sensitiveDirs := []string{".ssh", ".aws", ".gnupg", ".kube", ".config/gcloud"}
		for _, sd := range sensitiveDirs {
			full := filepath.Join(home, sd)
			fmt.Fprintf(&denyReads, "(deny file-read* (subpath \"%s\"))\n", escapeSBPLString(full))
		}
	}
	for _, absMask := range effectiveMasks(p) {
		fmt.Fprintf(&denyReads, "(deny file-read* (subpath \"%s\"))\n(deny file-write* (subpath \"%s\"))\n",
			escapeSBPLString(absMask), escapeSBPLString(absMask))
	}

	profileStr := fmt.Sprintf(`(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow sysctl-read)
(allow file-read*)
%s(allow file-write* (subpath "%s"))
%s
`, denyReads.String(), escapeSBPLString(p.WorktreeDir), netRule)

	args := []string{
		sbPath,
		"-p", profileStr,
		"--",
	}
	args = append(args, argv...)
	return args, nil
}

// ScrubEnv strips sensitive environment variables from child processes per STRIDE I2.
// Keeps only safe standard variables plus explicitly allowed variables.
func ScrubEnv(rawEnv []string, extraAllow []string) []string {
	safeKeys := map[string]bool{
		"PATH":     true,
		"LANG":     true,
		"LC_ALL":   true,
		"LC_CTYPE": true,
		"TERM":     true,
		"TMPDIR":   true,
		"USER":     true,
		"HOME":     true,
		"SHELL":    true,
	}
	for _, k := range extraAllow {
		safeKeys[k] = true
	}

	var out []string
	for _, entry := range rawEnv {
		idx := strings.IndexByte(entry, '=')
		if idx <= 0 {
			continue
		}
		key := entry[:idx]

		// Explicitly reject credentials and secrets unless allowed
		if strings.HasPrefix(key, "SSH_") ||
			strings.HasPrefix(key, "AWS_") ||
			strings.HasPrefix(key, "GOOGLE_") ||
			strings.HasPrefix(key, "AZURE_") ||
			strings.HasPrefix(key, "SHEPHERD_") ||
			key == "KUBECONFIG" ||
			key == "GITHUB_TOKEN" {
			if !safeKeys[key] {
				continue
			}
		}
		if safeKeys[key] {
			out = append(out, entry)
		}
	}
	return out
}

// ValidateEgressEndpoint checks whether an outbound host matches the declared egress allow-list.
func ValidateEgressEndpoint(host string, allowList []string) bool {
	if len(allowList) == 0 {
		return false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	for _, allowed := range allowList {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if host == allowed {
			return true
		}
		if strings.HasPrefix(allowed, "*.") && strings.HasSuffix(host, allowed[1:]) {
			return true
		}
	}
	return false
}
