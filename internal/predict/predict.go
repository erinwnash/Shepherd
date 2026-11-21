// Package predict estimates the probability of local model failure to route
// difficult chunks to cloud tiers first, saving time and quota.
//
// Built from scratch: L2-regularized logistic regression over work order features.
package predict

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/erinwnash/Shepherd/internal/workorder"
)

const (
	MinSamples  = 8
	Lambda      = 1.0
	NumFeatures = 5
)

// FeatureNames lists the human-readable names of the 5 predictor features.
var FeatureNames = []string{
	"effort M/L",
	"UI file",
	"new files",
	"4+ files",
	"5+ steps",
}

// Features holds the binary feature flags extracted from a work order.
type Features struct {
	EffortML  int
	HasUI     int
	NewFiles  int
	ManyFiles int
	ManySteps int
}

// ToVector converts Features to a float slice matching FeatureNames order.
func (f Features) ToVector() []float64 {
	return []float64{
		float64(f.EffortML),
		float64(f.HasUI),
		float64(f.NewFiles),
		float64(f.ManyFiles),
		float64(f.ManySteps),
	}
}

var stepLineRegex = regexp.MustCompile(`^\s*\d+\.`)

// ExtractFeatures extracts the 5 prediction features from a parsed work order.
func ExtractFeatures(wo *workorder.WorkOrder) Features {
	var feats Features

	if wo.Effort == workorder.EffortM || wo.Effort == workorder.EffortL {
		feats.EffortML = 1
	}

	for _, f := range wo.Files {
		pLower := strings.ToLower(f.Path)
		if strings.HasSuffix(pLower, ".tsx") || strings.HasSuffix(pLower, ".jsx") ||
			strings.HasSuffix(pLower, ".svelte") || strings.HasSuffix(pLower, ".vue") {
			feats.HasUI = 1
		}
		if f.New {
			feats.NewFiles = 1
		}
	}

	if len(wo.Files) >= 4 {
		feats.ManyFiles = 1
	}

	// Count numbered steps across body sections
	steps := 0
	for _, sec := range []string{"steps", "requirements", "acceptance"} {
		if text, ok := wo.Sections[sec]; ok {
			for _, line := range strings.Split(text, "\n") {
				if stepLineRegex.MatchString(line) {
					steps++
				}
			}
		}
	}
	if steps >= 5 {
		feats.ManySteps = 1
	}

	return feats
}

// Sample represents one historical training row from execution stats.
type Sample struct {
	ID    string
	Label int // 0 = local succeeded; 1 = needed cloud
	X     []float64
}

// Model represents a trained logistic regression model.
type Model struct {
	B    float64
	W    []float64
	Base float64
	N    int
}

func sigmoid(z float64) float64 {
	return 1.0 / (1.0 + math.Exp(-z))
}

// Train fits an L2-regularized logistic regression model over historical samples.
func Train(samples []Sample) Model {
	if len(samples) == 0 {
		return Model{
			W: make([]float64, NumFeatures),
			N: 0,
		}
	}

	totalLabels := 0.0
	for _, s := range samples {
		totalLabels += float64(s.Label)
	}
	base := totalLabels / float64(len(samples))
	b := math.Log((base + 0.05) / (1.05 - base))
	w := make([]float64, NumFeatures)

	n := float64(len(samples))
	for it := 0; it < 4000; it++ {
		gw := make([]float64, NumFeatures)
		for i := range gw {
			gw[i] = Lambda * w[i]
		}
		gb := 0.0

		for _, s := range samples {
			z := b
			for i, xi := range s.X {
				if i < len(w) {
					z += xi * w[i]
				}
			}
			err := sigmoid(z) - float64(s.Label)
			gb += err
			for i, xi := range s.X {
				if i < len(gw) {
					gw[i] += err * xi
				}
			}
		}

		b -= (0.1 * gb) / n
		for i := range w {
			w[i] -= (0.1 * gw[i]) / n
		}
	}

	return Model{
		B:    b,
		W:    w,
		Base: base,
		N:    len(samples),
	}
}

// Predict estimates failure probability and returns an explanation.
func Predict(model Model, wo *workorder.WorkOrder) (float64, string) {
	feats := ExtractFeatures(wo)
	return PredictVector(model, feats.ToVector())
}

// PredictVector evaluates the model on a raw feature vector.
func PredictVector(model Model, x []float64) (float64, string) {
	var hits []string
	for i, v := range x {
		if v > 0 && i < len(FeatureNames) {
			hits = append(hits, FeatureNames[i])
		}
	}

	if model.N < MinSamples {
		if len(hits) > 0 {
			return 0.7, fmt.Sprintf("rules: %s", strings.Join(hits, ", "))
		}
		return 0.3, "rules: small edit-only chunk"
	}

	z := model.B
	for i, xi := range x {
		if i < len(model.W) {
			z += xi * model.W[i]
		}
	}
	p := sigmoid(z)

	reason := fmt.Sprintf("model (n=%d): ", model.N)
	if len(hits) > 0 {
		reason += strings.Join(hits, ", ")
	} else {
		reason += "no risk features"
	}

	return p, reason
}

// Route decides whether a chunk should start on the cloud or locally.
func Route(wo *workorder.WorkOrder, model Model, cloudFirstThreshold float64) (bool, string) {
	if wo.Review == workorder.ReviewHigh {
		return true, "Review: HIGH -> cloud first"
	}

	p, why := Predict(model, wo)
	if p >= cloudFirstThreshold {
		return true, fmt.Sprintf("predicted local failure (p=%.2f >= %.2f): %s -> cloud first", p, cloudFirstThreshold, why)
	}

	return false, fmt.Sprintf("predicted local success (p=%.2f < %.2f): %s -> local first", p, cloudFirstThreshold, why)
}
