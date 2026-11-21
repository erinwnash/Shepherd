package repomap_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/repomap"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

func TestExtractFileSymbols_Go(t *testing.T) {
	goSrc := `package auth

type SessionToken struct {
	ID string
}

type TokenValidator interface {
	Validate(raw string) bool
}

func NewValidator() TokenValidator {
	return nil
}

func (s *SessionToken) IsExpired() bool {
	return false
}

func internalHelper() {}
`
	symbols := repomap.ExtractFileSymbols("auth.go", []byte(goSrc))
	if len(symbols) != 4 {
		t.Fatalf("expected 4 exported symbols, got %d: %v", len(symbols), symbols)
	}

	joined := strings.Join(symbols, "\n")
	if !strings.Contains(joined, "type SessionToken struct") {
		t.Errorf("expected SessionToken struct, got: %s", joined)
	}
	if !strings.Contains(joined, "type TokenValidator interface") {
		t.Errorf("expected TokenValidator interface, got: %s", joined)
	}
	if !strings.Contains(joined, "func NewValidator() TokenValidator") {
		t.Errorf("expected NewValidator signature, got: %s", joined)
	}
	if !strings.Contains(joined, "func (s *SessionToken) IsExpired() bool") {
		t.Errorf("expected IsExpired signature, got: %s", joined)
	}
	if strings.Contains(joined, "internalHelper") {
		t.Errorf("unexported internalHelper must not be in symbols: %s", joined)
	}
}

func TestExtractFileSymbols_TypeScript(t *testing.T) {
	tsSrc := `
export interface UserSession {
  userId: string;
}

export type AuthState = "authenticated" | "anonymous";

export class TokenManager {
  verify() {}
}

export function createSession(): UserSession {}

function privateUtil() {}
`
	symbols := repomap.ExtractFileSymbols("session.ts", []byte(tsSrc))
	if len(symbols) != 4 {
		t.Fatalf("expected 4 TS exported symbols, got %d: %v", len(symbols), symbols)
	}

	joined := strings.Join(symbols, "\n")
	if !strings.Contains(joined, "interface UserSession") || !strings.Contains(joined, "class TokenManager") {
		t.Errorf("unexpected TS symbols: %s", joined)
	}
	if strings.Contains(joined, "privateUtil") {
		t.Errorf("unexported privateUtil should not be present: %s", joined)
	}
}

func TestBuildRepoMap_And_EnrichPrompt(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "pkg", "auth")
	if err := os.MkdirAll(authDir, 0755); err != nil {
		t.Fatal(err)
	}

	authGo := filepath.Join(authDir, "auth.go")
	content := "package auth\ntype User struct{}\nfunc Login() {}\n"
	if err := os.WriteFile(authGo, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	wo := &workorder.WorkOrder{
		ID:    "09A",
		Title: "Auth Service",
		Files: []workorder.File{
			{Path: "pkg/auth/auth.go"},
		},
		Body: "## Goal\nImplement auth service.\n",
	}

	enriched := repomap.EnrichPrompt(wo, dir)
	if !strings.Contains(enriched, "Repository Symbol Map") {
		t.Errorf("expected Repository Symbol Map in enriched prompt, got:\n%s", enriched)
	}
	if !strings.Contains(enriched, "type User struct") {
		t.Errorf("expected 'type User struct' in enriched prompt, got:\n%s", enriched)
	}
	if !strings.Contains(enriched, "func Login()") {
		t.Errorf("expected 'func Login()' in enriched prompt, got:\n%s", enriched)
	}
}

func TestFindGoldenExample_GitCommit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	runGit := func(args ...string) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Skipf("git command failed: %v", err)
		}
	}

	runGit("init")
	runGit("config", "user.email", "test@test.local")
	runGit("config", "user.name", "Tester")

	// Commit 1: init
	testDir := filepath.Join(dir, "pkg", "tokens")
	_ = os.MkdirAll(testDir, 0755)
	_ = os.WriteFile(filepath.Join(testDir, "token.go"), []byte("package tokens\n"), 0644)
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Commit 2: feat(tokens): add token logic
	_ = os.WriteFile(filepath.Join(testDir, "token.go"), []byte("package tokens\nfunc CreateToken() string { return \"xyz\" }\n"), 0644)
	runGit("add", "-A")
	runGit("commit", "-m", "feat(tokens): add token generator")

	example, err := repomap.FindGoldenExample(ctx, dir, []string{"pkg/tokens/token.go"}, 50)
	if err != nil {
		t.Fatalf("FindGoldenExample failed: %v", err)
	}
	if example == "" {
		t.Fatalf("expected non-empty golden example diff")
	}
	if !strings.Contains(example, "CreateToken") {
		t.Errorf("expected diff to contain CreateToken, got:\n%s", example)
	}
}
