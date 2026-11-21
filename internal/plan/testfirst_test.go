package plan_test

import (
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/plan"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

func TestIsTestPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"pkg/foo_test.go", true},
		{"src/components/button.test.ts", true},
		{"src/components/button.spec.tsx", true},
		{"tests/integration/api.go", true},
		{"test/helpers.py", true},
		{"__tests__/app.js", true},
		{"python/test_auth.py", true},
		{"python/auth_test.py", true},
		{"rust/src/lib_test.rs", true},
		{"pkg/foo.go", false},
		{"src/components/button.tsx", false},
		{"python/auth.py", false},
		{"docs/plan/01A.md", false},
	}

	for _, tc := range cases {
		got := plan.IsTestPath(tc.path)
		if got != tc.want {
			t.Errorf("IsTestPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestSplitTestFirst_Success(t *testing.T) {
	orig := &workorder.WorkOrder{
		ID:        "02A",
		Title:     "Add User Session Management",
		Sprint:    "S02",
		DependsOn: []string{"01C"},
		Review:    workorder.ReviewMedium,
		Effort:    workorder.EffortM,
		Files: []workorder.File{
			{Path: "internal/session/session.go"},
			{Path: "internal/session/session_test.go", New: true},
		},
		Tests: []string{"internal/session/session_test.go"},
		Body:  "## Goal\nImplement user sessions.\n",
	}

	spec, impl, err := plan.SplitTestFirst(orig)
	if err != nil {
		t.Fatalf("SplitTestFirst failed: %v", err)
	}

	// Verify Spec chunk
	if spec.ID != "02A.spec" {
		t.Errorf("spec ID = %q, want 02A.spec", spec.ID)
	}
	if len(spec.DependsOn) != 1 || spec.DependsOn[0] != "01C" {
		t.Errorf("spec DependsOn = %v, want [01C]", spec.DependsOn)
	}
	if spec.ReadOnlyTests {
		t.Errorf("spec ReadOnlyTests should be false, got true")
	}
	if len(spec.Files) != 1 || spec.Files[0].Path != "internal/session/session_test.go" {
		t.Errorf("spec Files = %+v, want [internal/session/session_test.go]", spec.Files)
	}

	// Verify Impl chunk
	if impl.ID != "02A.impl" {
		t.Errorf("impl ID = %q, want 02A.impl", impl.ID)
	}
	if len(impl.DependsOn) != 1 || impl.DependsOn[0] != "02A.spec" {
		t.Errorf("impl DependsOn = %v, want [02A.spec]", impl.DependsOn)
	}
	if !impl.ReadOnlyTests {
		t.Errorf("impl ReadOnlyTests should be true, got false")
	}
	if len(impl.Files) != 1 || impl.Files[0].Path != "internal/session/session.go" {
		t.Errorf("impl Files = %+v, want [internal/session/session.go]", impl.Files)
	}
	if len(impl.Tests) != 1 || impl.Tests[0] != "internal/session/session_test.go" {
		t.Errorf("impl Tests = %v, want [internal/session/session_test.go]", impl.Tests)
	}

	// Verify Markdown formatting and parse roundtrip
	implMD, err := plan.FormatWorkOrderMarkdown(impl)
	if err != nil {
		t.Fatalf("FormatWorkOrderMarkdown(impl) failed: %v", err)
	}

	if !strings.Contains(implMD, "read_only_tests: true") {
		t.Errorf("expected serialized markdown to contain read_only_tests: true:\n%s", implMD)
	}

	parsedImpl, err := workorder.Parse("02A.impl.md", []byte(implMD))
	if err != nil {
		t.Fatalf("failed to re-parse generated markdown: %v\nMarkdown:\n%s", err, implMD)
	}
	if !parsedImpl.ReadOnlyTests {
		t.Errorf("parsed work order ReadOnlyTests = false, want true")
	}
	if parsedImpl.ID != "02A.impl" {
		t.Errorf("parsed ID = %q, want 02A.impl", parsedImpl.ID)
	}
	if len(parsedImpl.DependsOn) != 1 || parsedImpl.DependsOn[0] != "02A.spec" {
		t.Errorf("parsed DependsOn = %v, want [02A.spec]", parsedImpl.DependsOn)
	}
}

func TestSplitTestFirst_SynthesizeTests(t *testing.T) {
	orig := &workorder.WorkOrder{
		ID:     "05B",
		Title:  "Refactor Metrics Engine",
		Sprint: "S05",
		Files: []workorder.File{
			{Path: "pkg/metrics/collector.go"},
		},
		Body: "## Goal\nRefactor metrics.\n",
	}

	spec, impl, err := plan.SplitTestFirst(orig)
	if err != nil {
		t.Fatalf("SplitTestFirst failed: %v", err)
	}

	wantTest := "pkg/metrics/collector_test.go"
	if len(spec.Files) != 1 || spec.Files[0].Path != wantTest {
		t.Errorf("spec Files = %+v, want [%s]", spec.Files, wantTest)
	}
	if len(impl.Tests) != 1 || impl.Tests[0] != wantTest {
		t.Errorf("impl Tests = %+v, want [%s]", impl.Tests, wantTest)
	}
	if !impl.ReadOnlyTests {
		t.Errorf("impl.ReadOnlyTests should be true")
	}
}

func TestSplitTestFirst_NoImplFilesError(t *testing.T) {
	orig := &workorder.WorkOrder{
		ID:     "07A",
		Title:  "Only Tests",
		Sprint: "S07",
		Files: []workorder.File{
			{Path: "tests/suite_test.go"},
		},
		Tests: []string{"tests/suite_test.go"},
	}

	_, _, err := plan.SplitTestFirst(orig)
	if err == nil {
		t.Fatalf("expected error when no implementation files exist, got nil")
	}
}
