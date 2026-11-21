package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandbox_UnsandboxedDisallowed(t *testing.T) {
	origLook := LookBwrap
	defer func() { LookBwrap = origLook }()
	LookBwrap = func() (string, error) { return "", errors.New("not found") }

	_, err := Wrap([]string{"echo", "hi"}, Profile{AllowUnsandboxed: false})
	if !errors.Is(err, ErrUnsandboxedDisallowed) {
		t.Fatalf("expected ErrUnsandboxedDisallowed, got %v", err)
	}
}

func TestSandbox_UnsandboxedAllowed(t *testing.T) {
	origLook := LookBwrap
	defer func() { LookBwrap = origLook }()
	LookBwrap = func() (string, error) { return "", errors.New("not found") }

	cmd, err := Wrap([]string{"echo", "hi"}, Profile{AllowUnsandboxed: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd) != 2 || cmd[0] != "echo" || cmd[1] != "hi" {
		t.Fatalf("unexpected passthrough cmd: %v", cmd)
	}
}

func TestSandbox_BwrapArgvConstruction(t *testing.T) {
	origLook := LookBwrap
	defer func() { LookBwrap = origLook }()
	LookBwrap = func() (string, error) { return "/usr/bin/bwrap", nil }

	origOS := CurrentOS
	defer func() { CurrentOS = origOS }()
	CurrentOS = "linux"

	worktree := t.TempDir()
	maskDir := filepath.Join(worktree, ".shepherd")
	_ = os.MkdirAll(maskDir, 0700)

	// Test 1: Network disabled -> must include --unshare-net (STRIDE B4)
	argv, err := Wrap([]string{"python3", "-c", "print(1)"}, Profile{
		WorktreeDir:  worktree,
		MaskDirs:     []string{maskDir},
		AllowNetwork: false,
	})
	if err != nil {
		t.Fatalf("unexpected Wrap error: %v", err)
	}

	foundUnshareNet := false
	for _, a := range argv {
		if a == "--unshare-net" {
			foundUnshareNet = true
			break
		}
	}
	if !foundUnshareNet {
		t.Fatalf("expected --unshare-net in bwrap arguments when AllowNetwork=false")
	}

	// Test 2: Network enabled -> must NOT include --unshare-net
	argvNet, err := Wrap([]string{"python3", "-c", "print(1)"}, Profile{
		WorktreeDir:  worktree,
		AllowNetwork: true,
	})
	if err != nil {
		t.Fatalf("unexpected Wrap error: %v", err)
	}
	for _, a := range argvNet {
		if a == "--unshare-net" {
			t.Fatalf("unexpected --unshare-net when AllowNetwork=true")
		}
	}
}

func TestSandbox_DarwinSandboxExec(t *testing.T) {
	origLook := LookSandboxExec
	defer func() { LookSandboxExec = origLook }()
	LookSandboxExec = func() (string, error) { return "/usr/bin/sandbox-exec", nil }

	origOS := CurrentOS
	defer func() { CurrentOS = origOS }()
	CurrentOS = "darwin"

	worktree := t.TempDir()

	argv, err := Wrap([]string{"node", "test.js"}, Profile{
		WorktreeDir:  worktree,
		AllowNetwork: false,
	})
	if err != nil {
		t.Fatalf("darwin Wrap failed: %v", err)
	}

	if argv[0] != "/usr/bin/sandbox-exec" {
		t.Fatalf("expected sandbox-exec, got %s", argv[0])
	}
	if !strings.Contains(argv[2], "(deny network*)") {
		t.Fatalf("expected (deny network*) in darwin sandbox profile, got: %s", argv[2])
	}
}

func TestSandbox_ScrubEnv_STRIDE_I2(t *testing.T) {
	rawEnv := []string{
		"PATH=/usr/bin:/bin",
		"LANG=en_US.UTF-8",
		"SSH_AUTH_SOCK=/tmp/ssh-agent.sock",
		"AWS_SECRET_ACCESS_KEY=secret123",
		"GOOGLE_APPLICATION_CREDENTIALS=/tmp/creds.json",
		"KUBECONFIG=/home/user/.kube/config",
		"GITHUB_TOKEN=ghp_secret",
		"SHEPHERD_SOCKET=/tmp/shepherd.sock",
		"CUSTOM_PERMITTED=allowed_val",
	}

	scrubbed := ScrubEnv(rawEnv, []string{"CUSTOM_PERMITTED"})

	scrubbedMap := make(map[string]bool)
	for _, s := range scrubbed {
		parts := strings.SplitN(s, "=", 2)
		scrubbedMap[parts[0]] = true
	}

	// Permitted standard vars
	if !scrubbedMap["PATH"] || !scrubbedMap["LANG"] {
		t.Fatalf("Expected PATH and LANG to be retained in scrubbed env")
	}
	if !scrubbedMap["CUSTOM_PERMITTED"] {
		t.Fatalf("Expected CUSTOM_PERMITTED to be retained")
	}

	// Specifically rejected credential vars (STRIDE I2)
	leaked := []string{
		"SSH_AUTH_SOCK",
		"AWS_SECRET_ACCESS_KEY",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"KUBECONFIG",
		"GITHUB_TOKEN",
		"SHEPHERD_SOCKET",
	}
	for _, l := range leaked {
		if scrubbedMap[l] {
			t.Fatalf("Sensitive environment variable %s was not scrubbed (STRIDE I2)!", l)
		}
	}
}

func TestSandbox_ValidateEgressEndpoint(t *testing.T) {
	allowList := []string{
		"api.anthropic.com",
		"generativelanguage.googleapis.com",
		"*.github.com",
	}

	if !ValidateEgressEndpoint("api.anthropic.com", allowList) {
		t.Fatalf("Expected exact match to succeed")
	}
	if !ValidateEgressEndpoint("api.github.com", allowList) {
		t.Fatalf("Expected wildcard match to succeed")
	}
	if ValidateEgressEndpoint("evil-malicious-site.com", allowList) {
		t.Fatalf("Expected unlisted site to be rejected")
	}
}

func TestSandbox_DarwinEscapingAndMasking(t *testing.T) {
	origLook := LookSandboxExec
	defer func() { LookSandboxExec = origLook }()
	LookSandboxExec = func() (string, error) { return "/usr/bin/sandbox-exec", nil }

	origOS := CurrentOS
	defer func() { CurrentOS = origOS }()
	CurrentOS = "darwin"

	argv, err := Wrap([]string{"echo", "ok"}, Profile{
		WorktreeDir:  `/tmp/work"inject)`,
		MaskDirs:     []string{"/tmp/secret-state"},
		AllowNetwork: false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prof := argv[2]
	if !strings.Contains(prof, `(subpath "/tmp/work\"inject)")`) {
		t.Errorf("expected escaped quote in SBPL profile, got:\n%s", prof)
	}
	if !strings.Contains(prof, `(deny file-read* (subpath "/tmp/secret-state"))`) {
		t.Errorf("expected MaskDirs deny rule in SBPL profile, got:\n%s", prof)
	}
}

func TestSandbox_ContainerBackend(t *testing.T) {
	origLook := LookContainer
	defer func() { LookContainer = origLook }()
	LookContainer = func(rt string) (string, error) { return "/usr/bin/" + rt, nil }

	worktree := t.TempDir()
	argv, err := Wrap([]string{"go", "test", "./..."}, Profile{
		Backend:        "podman",
		ContainerImage: "golang:1.22",
		WorktreeDir:    worktree,
		MaskDirs:       []string{filepath.Join(worktree, ".shepherd")},
		AllowNetwork:   false,
	})
	if err != nil {
		t.Fatalf("container wrap failed: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "/usr/bin/podman run --rm -i") {
		t.Errorf("expected podman run, got: %s", joined)
	}
	if !strings.Contains(joined, "--network=none") {
		t.Errorf("expected --network=none when AllowNetwork=false, got: %s", joined)
	}
	if !strings.Contains(joined, "golang:1.22 go test ./...") {
		t.Errorf("expected image and command at end, got: %s", joined)
	}
}

// A masked directory that contains the worktree (the front-run lane lives in .shepherd/frontrun)
// must not hide it: its other entries are masked one by one instead.
func TestSandbox_MaskAroundAWorktreeInsideIt(t *testing.T) {
	origLook, origOS := LookBwrap, CurrentOS
	defer func() { LookBwrap, CurrentOS = origLook, origOS }()
	LookBwrap = func() (string, error) { return "/usr/bin/bwrap", nil }
	CurrentOS = "linux"

	root := t.TempDir()
	state := filepath.Join(root, ".shepherd")
	wt := filepath.Join(state, "frontrun")
	for _, d := range []string{wt, filepath.Join(state, "prompts"), filepath.Join(state, "slots")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "state.db"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	argv, err := Wrap([]string{"claude"}, Profile{WorktreeDir: wt, MaskDirs: []string{state}, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(argv, " ") + " "
	if strings.Contains(joined, " --tmpfs "+state+" ") {
		t.Fatalf("masking the whole state dir hides the worktree: %s", joined)
	}
	for _, want := range []string{
		" --tmpfs " + filepath.Join(state, "prompts") + " ",
		" --tmpfs " + filepath.Join(state, "slots") + " ",
		" --ro-bind /dev/null " + filepath.Join(state, "state.db") + " ",
		" --bind " + wt + " " + wt + " ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, " --tmpfs "+wt+" ") {
		t.Errorf("worktree itself must stay visible:\n%s", joined)
	}

	// Without a worktree inside it, the directory is still masked whole.
	argv, _ = Wrap([]string{"claude"}, Profile{WorktreeDir: root, MaskDirs: []string{state}, AllowNetwork: true})
	if !strings.Contains(" "+strings.Join(argv, " ")+" ", " --tmpfs "+state+" ") {
		t.Errorf("a plain mask dir must still be masked whole: %v", argv)
	}
}

func TestSandbox_BwrapChdirIsAbsoluteForRelativeWorktree(t *testing.T) {
	origLook, origOS := LookBwrap, CurrentOS
	defer func() { LookBwrap, CurrentOS = origLook, origOS }()
	LookBwrap = func() (string, error) { return "/usr/bin/bwrap", nil }
	CurrentOS = "linux"

	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(".shepherd", "frontrun"), 0700); err != nil {
		t.Fatal(err)
	}
	argv, err := Wrap([]string{"claude"}, Profile{WorktreeDir: filepath.Join(".shepherd", "frontrun"), MaskDirs: []string{".shepherd"}, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, ".shepherd", "frontrun")
	for i, a := range argv {
		if a == "--chdir" {
			if argv[i+1] != want {
				t.Fatalf("--chdir = %q, want %q", argv[i+1], want)
			}
			return
		}
	}
	t.Fatalf("no --chdir in %v", argv)
}

// bwrapHasMount reports whether argv contains `flag src dst` with src == dst == path.
func bwrapHasMount(argv []string, flag, path string) bool {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == path && argv[i+2] == path {
			return true
		}
	}
	return false
}

func TestSandbox_ReadOnlyAdapterDirsAreNotWritable(t *testing.T) {
	origLook, origOS := LookBwrap, CurrentOS
	defer func() { LookBwrap, CurrentOS = origLook, origOS }()
	LookBwrap = func() (string, error) { return "/usr/bin/bwrap", nil }
	CurrentOS = "linux"

	home := t.TempDir()
	t.Setenv("HOME", home)
	config := filepath.Join(home, ".config", "opencode")
	data := filepath.Join(home, ".local", "share", "opencode")
	for _, d := range []string{config, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	argv, err := Wrap([]string{"opencode", "run"}, Profile{
		WorktreeDir:         t.TempDir(),
		AdapterDirs:         []string{".local/share/opencode"},
		ReadOnlyAdapterDirs: []string{".config/opencode"},
		AllowNetwork:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bwrapHasMount(argv, "--ro-bind", config) {
		t.Errorf("read-only adapter dir %s is not mounted --ro-bind: %v", config, argv)
	}
	if bwrapHasMount(argv, "--bind", config) {
		t.Errorf("read-only adapter dir %s must not also be mounted writable", config)
	}
	if !bwrapHasMount(argv, "--bind", data) {
		t.Errorf("writable adapter dir %s is not mounted --bind: %v", data, argv)
	}
}
