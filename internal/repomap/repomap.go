package repomap

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/erinwnash/Shepherd/internal/workorder"
)

var (
	tsExportRegex = regexp.MustCompile(`(?m)^\s*export\s+(interface|type|class|function|enum|const)\s+([A-Za-z0-9_]+)`)
	pyDefRegex    = regexp.MustCompile(`(?m)^(class|def)\s+([A-Za-z0-9_]+)`)
)

// ExtractFileSymbols parses exported types, interfaces, structs, and functions from a file.
func ExtractFileSymbols(filePath string, src []byte) []string {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".go":
		return extractGoSymbols(filePath, src)
	case ".ts", ".tsx", ".js", ".jsx":
		return extractTypeScriptSymbols(src)
	case ".py":
		return extractPythonSymbols(src)
	default:
		return nil
	}
}

func extractGoSymbols(filePath string, src []byte) []string {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, src, parser.ParseComments)
	if err != nil {
		return nil
	}

	var symbols []string

	for _, decl := range node.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					if ts.Name.IsExported() {
						kind := "type"
						switch ts.Type.(type) {
						case *ast.StructType:
							kind = "struct"
						case *ast.InterfaceType:
							kind = "interface"
						}
						symbols = append(symbols, fmt.Sprintf("type %s %s", ts.Name.Name, kind))
					}
				}
			}
		case *ast.FuncDecl:
			if d.Name.IsExported() {
				var buf bytes.Buffer
				// Print signature only without function body
				savedBody := d.Body
				d.Body = nil
				_ = printer.Fprint(&buf, fset, d)
				d.Body = savedBody
				symbols = append(symbols, strings.TrimSpace(buf.String()))
			}
		}
	}

	return symbols
}

func extractTypeScriptSymbols(src []byte) []string {
	matches := tsExportRegex.FindAllStringSubmatch(string(src), -1)
	var symbols []string
	for _, m := range matches {
		if len(m) >= 3 {
			symbols = append(symbols, fmt.Sprintf("%s %s", m[1], m[2]))
		}
	}
	return symbols
}

func extractPythonSymbols(src []byte) []string {
	matches := pyDefRegex.FindAllStringSubmatch(string(src), -1)
	var symbols []string
	for _, m := range matches {
		if len(m) >= 3 {
			symbols = append(symbols, fmt.Sprintf("%s %s", m[1], m[2]))
		}
	}
	return symbols
}

// BuildRepoMap scans target paths and their sibling package files to generate
// a concise exported symbol map.
func BuildRepoMap(projectRoot string, declaredPaths []string, maxSymbols int) string {
	if maxSymbols <= 0 {
		maxSymbols = 30
	}

	seenDirs := make(map[string]bool)
	filesToScan := make(map[string]bool)

	for _, p := range declaredPaths {
		clean := filepath.Clean(p)
		filesToScan[clean] = true
		dir := filepath.Dir(clean)
		if !seenDirs[dir] && dir != "." && dir != "/" {
			seenDirs[dir] = true
			// Scan sibling files in the same directory
			fullDir := filepath.Join(projectRoot, dir)
			entries, err := os.ReadDir(fullDir)
			if err == nil {
				for _, e := range entries {
					if !e.IsDir() {
						filesToScan[filepath.Join(dir, e.Name())] = true
					}
				}
			}
		}
	}

	var sb strings.Builder
	totalSymbols := 0

	for relPath := range filesToScan {
		if totalSymbols >= maxSymbols {
			break
		}
		fullPath := filepath.Join(projectRoot, relPath)
		src, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}

		symbols := ExtractFileSymbols(relPath, src)
		if len(symbols) == 0 {
			continue
		}

		sb.WriteString(fmt.Sprintf("%s:\n", relPath))
		for _, sym := range symbols {
			if totalSymbols >= maxSymbols {
				break
			}
			sb.WriteString(fmt.Sprintf("  %s\n", sym))
			totalSymbols++
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// FindGoldenExample searches recent git history for a completed chunk commit
// touching related directories to provide a reference pattern for the agent.
func FindGoldenExample(ctx context.Context, projectRoot string, targetPaths []string, maxLines int) (string, error) {
	if maxLines <= 0 {
		maxLines = 40
	}

	targetDirs := make(map[string]bool)
	for _, p := range targetPaths {
		targetDirs[filepath.Dir(p)] = true
	}

	// List recent commits
	cmd := exec.CommandContext(ctx, "git", "log", "-n", "10", "--format=%H %s")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			continue
		}
		commitSHA := parts[0]
		subject := parts[1]

		// Check if it's a completed feature commit
		if !strings.HasPrefix(subject, "feat(") && !strings.HasPrefix(subject, "fix(") {
			continue
		}

		// Inspect touched files
		diffStatCmd := exec.CommandContext(ctx, "git", "show", "--name-only", "--format=", commitSHA)
		diffStatCmd.Dir = projectRoot
		statOut, err := diffStatCmd.Output()
		if err != nil {
			continue
		}

		touched := strings.Split(strings.TrimSpace(string(statOut)), "\n")
		matched := false
		for _, t := range touched {
			if targetDirs[filepath.Dir(t)] {
				matched = true
				break
			}
		}

		if matched {
			// Extract unified diff
			diffCmd := exec.CommandContext(ctx, "git", "show", "--unified=1", commitSHA)
			diffCmd.Dir = projectRoot
			diffBytes, err := diffCmd.Output()
			if err != nil {
				continue
			}

			diffLines := strings.Split(string(diffBytes), "\n")
			if len(diffLines) > maxLines {
				diffLines = append(diffLines[:maxLines], "... [truncated for brevity]")
			}
			return strings.Join(diffLines, "\n"), nil
		}
	}

	return "", nil
}

// EnrichPrompt pre-injects the exported symbol repo map and golden example diff into the chunk prompt.
func EnrichPrompt(wo *workorder.WorkOrder, projectRoot string) string {
	var paths []string
	for _, f := range wo.Files {
		paths = append(paths, f.Path)
	}
	paths = append(paths, wo.Tests...)

	repoMap := BuildRepoMap(projectRoot, paths, 30)
	goldenEx, _ := FindGoldenExample(context.Background(), projectRoot, paths, 40)

	var sb strings.Builder
	sb.WriteString(wo.Body)

	if repoMap != "" {
		sb.WriteString("\n\n## Repository Symbol Map (Exported Definitions)\n```\n")
		sb.WriteString(repoMap)
		sb.WriteString("\n```\n")
	}

	if goldenEx != "" {
		sb.WriteString("\n\n## Golden Example: Reference Coding & Testing Pattern\n```diff\n")
		sb.WriteString(goldenEx)
		sb.WriteString("\n```\n")
	}

	return sb.String()
}
