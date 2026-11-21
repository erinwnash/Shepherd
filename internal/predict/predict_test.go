package predict

import (
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/workorder"
)

func makeWO(t *testing.T, id, effort, review string, files []string, body string) *workorder.WorkOrder {
	t.Helper()
	src := "---\n" +
		"id: " + id + "\n" +
		"title: Test " + id + "\n" +
		"sprint: S01\n" +
		"est_effort: " + effort + "\n" +
		"review: " + review + "\n" +
		"files:\n"
	for _, f := range files {
		src += "  - " + f + "\n"
	}
	src += "no_tests_reason: \"none\"\n"
	src += "---\n" + body
	wo, err := workorder.Parse(id+".md", []byte(src))
	if err != nil {
		t.Fatalf("Parse %s: %v", id, err)
	}
	return wo
}

func TestExtractFeatures(t *testing.T) {
	body := "## Goal\nSome goal\n## Steps\n1. First\n2. Second\n3. Third\n4. Fourth\n5. Fifth\n"
	files := []string{"src/components/View.tsx", "new:src/lib/helper.go", "src/a.go", "src/b.go"}
	wo := makeWO(t, "01A", "M", "LOW", files, body)

	feats := ExtractFeatures(wo)
	if feats.EffortML != 1 {
		t.Errorf("EffortML = %d, want 1", feats.EffortML)
	}
	if feats.HasUI != 1 {
		t.Errorf("HasUI = %d, want 1", feats.HasUI)
	}
	if feats.NewFiles != 1 {
		t.Errorf("NewFiles = %d, want 1", feats.NewFiles)
	}
	if feats.ManyFiles != 1 {
		t.Errorf("ManyFiles = %d, want 1", feats.ManyFiles)
	}
	if feats.ManySteps != 1 {
		t.Errorf("ManySteps = %d, want 1", feats.ManySteps)
	}
}

func TestPredict_FallbackRules(t *testing.T) {
	// Model with 0 samples
	m := Train(nil)
	if m.N != 0 {
		t.Fatalf("expected N=0, got %d", m.N)
	}

	// Work order with risk features (e.g. M effort)
	woRisky := makeWO(t, "01B", "M", "LOW", []string{"src/foo.go"}, "## Goal\nTest\n")
	p, why := Predict(m, woRisky)
	if p != 0.7 {
		t.Errorf("expected p=0.7 for fallback with risk features, got %f", p)
	}
	if !strings.Contains(why, "rules") {
		t.Errorf("expected why to mention 'rules', got: %s", why)
	}

	// Work order with no risk features (S effort, 1 file, no UI, no new files)
	woSmall := makeWO(t, "01C", "S", "LOW", []string{"src/foo.go"}, "## Goal\nTest\n")
	p2, why2 := Predict(m, woSmall)
	if p2 != 0.3 {
		t.Errorf("expected p=0.3 for fallback without risk features, got %f", p2)
	}
	if !strings.Contains(why2, "small") {
		t.Errorf("expected why to mention 'small', got: %s", why2)
	}
}

func TestPredict_TrainedModel(t *testing.T) {
	// Generate 10 samples (>= MinSamples of 8)
	var samples []Sample
	for i := 0; i < 5; i++ {
		// Risky chunk that failed locally (Label = 1)
		samples = append(samples, Sample{
			ID:    "fail",
			Label: 1,
			X:     []float64{1, 1, 1, 1, 0},
		})
		// Small chunk that succeeded locally (Label = 0)
		samples = append(samples, Sample{
			ID:    "ok",
			Label: 0,
			X:     []float64{0, 0, 0, 0, 0},
		})
	}

	model := Train(samples)
	if model.N != 10 {
		t.Fatalf("model.N = %d, want 10", model.N)
	}

	// Risky input should have high predicted failure probability
	pRisky, _ := PredictVector(model, []float64{1, 1, 1, 1, 0})
	if pRisky < 0.6 {
		t.Errorf("expected high p for risky features, got %f", pRisky)
	}

	// Safe input should have low failure probability
	pSafe, _ := PredictVector(model, []float64{0, 0, 0, 0, 0})
	if pSafe > 0.4 {
		t.Errorf("expected low p for safe features, got %f", pSafe)
	}
}

func TestRoute(t *testing.T) {
	m := Train(nil) // fallback rules

	// 1. Review: HIGH always starts on the cloud
	woHigh := makeWO(t, "02A", "S", "HIGH", []string{"src/safe.go"}, "## Goal\nHigh review\n")
	cloud, reason := Route(woHigh, m, 0.5)
	if !cloud {
		t.Errorf("Review: HIGH chunk must route cloud first")
	}
	if !strings.Contains(reason, "HIGH") {
		t.Errorf("reason should mention HIGH, got: %s", reason)
	}

	// 2. Risky chunk (p=0.7 >= 0.5) routes cloud first
	woRisky := makeWO(t, "02B", "L", "LOW", []string{"src/safe.go"}, "## Goal\nLarge\n")
	cloud2, reason2 := Route(woRisky, m, 0.5)
	if !cloud2 {
		t.Errorf("Risky chunk should route cloud first")
	}
	if !strings.Contains(reason2, "predicted") {
		t.Errorf("reason should mention predicted, got: %s", reason2)
	}

	// 3. Small chunk (p=0.3 < 0.5) routes local first
	woSmall := makeWO(t, "02C", "S", "LOW", []string{"src/safe.go"}, "## Goal\nSmall\n")
	cloud3, reason3 := Route(woSmall, m, 0.5)
	if cloud3 {
		t.Errorf("Small chunk should route local first")
	}
	if !strings.Contains(reason3, "local") {
		t.Errorf("reason should mention local, got: %s", reason3)
	}
}
