package plan

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/erinwnash/Shepherd/internal/workorder"
)

// IsTestPath returns true if the file path is recognized as a test suite, specification, or fixture.
func IsTestPath(path string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(p)

	// Suffix patterns
	testSuffixes := []string{
		"_test.go",
		".test.ts", ".spec.ts",
		".test.tsx", ".spec.tsx",
		".test.js", ".spec.js",
		".test.jsx", ".spec.jsx",
		"_test.py",
		"_test.rs",
		"_test.rb",
		"test.java",
		"tests.java",
		"tests.cs",
	}
	for _, s := range testSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	if strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") {
		return true
	}

	// Directory patterns
	parts := strings.Split(p, "/")
	if len(parts) > 1 {
		for _, part := range parts[:len(parts)-1] {
			if part == "tests" || part == "test" || part == "spec" || part == "specs" || part == "__tests__" {
				return true
			}
		}
	}

	return false
}

// SuggestTestPath generates an appropriate default test file path for a production file.
func SuggestTestPath(prodPath string) string {
	p := filepath.ToSlash(prodPath)
	dir := filepath.Dir(p)
	base := filepath.Base(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	switch ext {
	case ".go":
		if dir == "." {
			return stem + "_test.go"
		}
		return filepath.ToSlash(filepath.Join(dir, stem+"_test.go"))
	case ".ts":
		if dir == "." {
			return stem + ".test.ts"
		}
		return filepath.ToSlash(filepath.Join(dir, stem+".test.ts"))
	case ".tsx":
		if dir == "." {
			return stem + ".test.tsx"
		}
		return filepath.ToSlash(filepath.Join(dir, stem+".test.tsx"))
	case ".js":
		if dir == "." {
			return stem + ".test.js"
		}
		return filepath.ToSlash(filepath.Join(dir, stem+".test.js"))
	case ".jsx":
		if dir == "." {
			return stem + ".test.jsx"
		}
		return filepath.ToSlash(filepath.Join(dir, stem+".test.jsx"))
	case ".py":
		if dir == "." {
			return "test_" + stem + ".py"
		}
		return filepath.ToSlash(filepath.Join(dir, "test_"+stem+".py"))
	case ".rs":
		if dir == "." {
			return stem + "_test.rs"
		}
		return filepath.ToSlash(filepath.Join(dir, stem+"_test.rs"))
	default:
		if dir == "." {
			return "tests/" + stem + "_test" + ext
		}
		return filepath.ToSlash(filepath.Join("tests", p))
	}
}

// SplitTestFirst decomposes a work order into two sequential chunks:
// 1. <id>.spec (Test Authoring): Authors the test suite and assertions (read_only_tests: false).
// 2. <id>.impl (Implementation): Implements production code to satisfy the immutable tests (read_only_tests: true).
func SplitTestFirst(wo *workorder.WorkOrder) (*workorder.WorkOrder, *workorder.WorkOrder, error) {
	if wo == nil {
		return nil, nil, errors.New("nil work order")
	}

	specID := wo.ID + ".spec"
	implID := wo.ID + ".impl"

	// Gather test files and implementation files
	var testPaths []string
	testSeen := make(map[string]bool)

	addTest := func(p string) {
		p = filepath.ToSlash(strings.TrimSpace(p))
		if p != "" && !testSeen[p] {
			testSeen[p] = true
			testPaths = append(testPaths, p)
		}
	}

	var implFiles []workorder.File
	for _, f := range wo.Files {
		if IsTestPath(f.Path) {
			addTest(f.Path)
		} else {
			implFiles = append(implFiles, f)
		}
	}

	for _, t := range wo.Tests {
		addTest(t)
	}

	// If no tests were identified, synthesize one from the first implementation file
	if len(testPaths) == 0 {
		if len(implFiles) > 0 {
			suggested := SuggestTestPath(implFiles[0].Path)
			addTest(suggested)
		} else {
			addTest("tests/" + strings.ToLower(wo.ID) + "_test.go")
		}
	}

	// Validate implementation files exist
	if len(implFiles) == 0 {
		return nil, nil, errors.New("cannot create test-first pair: work order has no implementation files")
	}

	// Build spec files list
	var specFiles []workorder.File
	for _, t := range testPaths {
		specFiles = append(specFiles, workorder.File{Path: t, New: true})
	}

	specDeps := append([]string{}, wo.DependsOn...)

	specBody := fmt.Sprintf(`## Goal
Author comprehensive test suites and assertions for %s.

## Scope
Author ONLY test files and test fixtures declared under 'files':
%s

Do NOT implement the production code in this chunk.
The tests should clearly articulate all requirements, expected behavior, edge cases, and interfaces.

## Instructions
1. Write exhaustive unit/integration tests with clear assertions.
2. Ensure tests compile and fail gracefully against missing or stubbed production logic (Red-Green-Refactor).
3. Once committed, these tests become IMMUTABLE read-only specifications for the implementation chunk.
`, wo.Title, formatBulletList(testPaths))

	implBody := fmt.Sprintf(`## Goal
Implement production code satisfying the immutable test suite authored in %s.

## Scope
Modify ONLY the production files declared under 'files':
%s

The test files under 'tests' are READ-ONLY (TDD Anti-Cheating):
%s
Any alteration, weakening, or removal of test assertions is strictly forbidden and will be rejected by the verification scope gate.

## Instructions
1. Implement the minimal clean code necessary to pass all assertions in the test suite.
2. Verify all gates pass without modifying any test files.
`, specID, formatFileList(implFiles), formatBulletList(testPaths))

	specWO := &workorder.WorkOrder{
		ID:            specID,
		Title:         wo.Title + " (Test Specification)",
		Epic:          wo.Epic,
		Sprint:        wo.Sprint,
		DependsOn:     specDeps,
		Review:        wo.Review,
		Effort:        workorder.EffortS,
		Status:        workorder.StatusPending,
		Files:         specFiles,
		Tests:         testPaths,
		ReadOnlyTests: false,
		Body:          specBody,
	}

	implWO := &workorder.WorkOrder{
		ID:            implID,
		Title:         wo.Title + " (Implementation)",
		Epic:          wo.Epic,
		Sprint:        wo.Sprint,
		DependsOn:     []string{specID},
		Review:        wo.Review,
		Effort:        wo.Effort,
		Status:        workorder.StatusPending,
		Files:         implFiles,
		Tests:         testPaths,
		ReadOnlyTests: true,
		Body:          implBody,
	}

	return specWO, implWO, nil
}

func formatBulletList(items []string) string {
	var sb strings.Builder
	for _, it := range items {
		sb.WriteString("- " + it + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatFileList(files []workorder.File) string {
	var sb strings.Builder
	for _, f := range files {
		prefix := ""
		if f.New {
			prefix = "new:"
		}
		sb.WriteString("- " + prefix + f.Path + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
