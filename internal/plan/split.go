// Package plan provides linting, loading, and chunk splitting per brief §8 and §9.
package plan

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/erinwnash/Shepherd/internal/workorder"
	"go.yaml.in/yaml/v3"
)

// SplitOptions controls chunk splitting.
type SplitOptions struct {
	Parts int // default 2
}

// SplitChunk splits an oversize work order into sequential smaller sub-chunks per brief §8.
func SplitChunk(wo *workorder.WorkOrder, opts SplitOptions) ([]*workorder.WorkOrder, error) {
	if wo == nil {
		return nil, errors.New("nil work order")
	}

	parts := opts.Parts
	if parts <= 1 {
		parts = 2
	}
	if parts > 4 {
		parts = 4
	}

	totalFiles := len(wo.Files)
	filesPerPart := int(math.Ceil(float64(totalFiles) / float64(parts)))
	if filesPerPart == 0 {
		filesPerPart = 1
	}

	var out []*workorder.WorkOrder
	prevID := ""

	for i := 0; i < parts; i++ {
		start := i * filesPerPart
		if start >= totalFiles && i > 0 && len(wo.Files) > 0 {
			break
		}
		end := start + filesPerPart
		if end > totalFiles {
			end = totalFiles
		}

		var partFiles []workorder.File
		if totalFiles > 0 {
			partFiles = wo.Files[start:end]
		}

		subID := fmt.Sprintf("%s.%d", wo.ID, i+1)
		title := fmt.Sprintf("%s (Part %d of %d)", wo.Title, i+1, parts)

		// Dependencies: Part 1 inherits original dependencies; subsequent parts depend on the previous part
		var deps []string
		if i == 0 {
			deps = append([]string{}, wo.DependsOn...)
		} else {
			deps = []string{prevID}
		}
		prevID = subID

		body := fmt.Sprintf("## Goal\nComplete part %d of %d for %s.\n\n## Scope\nFocus on assigned files for this sub-chunk.\n",
			i+1, parts, wo.Title)

		subWO := &workorder.WorkOrder{
			ID:        subID,
			Title:     title,
			Epic:      wo.Epic,
			Sprint:    wo.Sprint,
			DependsOn: deps,
			Review:    wo.Review,
			Effort:    workorder.EffortS,
			Status:    workorder.StatusPending,
			Files:     partFiles,
			Body:      body,
		}

		out = append(out, subWO)
	}

	return out, nil
}

// FormatWorkOrderMarkdown serializes a WorkOrder struct to standard YAML frontmatter + markdown body.
func FormatWorkOrderMarkdown(wo *workorder.WorkOrder) (string, error) {
	type frontmatter struct {
		ID            string   `yaml:"id"`
		Title         string   `yaml:"title"`
		Epic          string   `yaml:"epic,omitempty"`
		Sprint        string   `yaml:"sprint,omitempty"`
		DependsOn     []string `yaml:"depends_on,omitempty"`
		Review        string   `yaml:"review,omitempty"`
		Effort        string   `yaml:"est_effort,omitempty"`
		Files         []string `yaml:"files,omitempty"`
		Tests         []string `yaml:"tests,omitempty"`
		NoTestsReason string   `yaml:"no_tests_reason,omitempty"`
		ReadOnlyTests bool     `yaml:"read_only_tests,omitempty"`
		Status        string   `yaml:"status,omitempty"`
	}

	var fileList []string
	for _, f := range wo.Files {
		prefix := ""
		if f.New {
			prefix = "new:"
		}
		fileList = append(fileList, prefix+f.Path)
	}

	fm := frontmatter{
		ID:            wo.ID,
		Title:         wo.Title,
		Epic:          wo.Epic,
		Sprint:        wo.Sprint,
		DependsOn:     wo.DependsOn,
		Review:        string(wo.Review),
		Effort:        string(wo.Effort),
		Status:        string(wo.Status),
		Files:         fileList,
		Tests:         wo.Tests,
		NoTestsReason: wo.NoTestsReason,
		ReadOnlyTests: wo.ReadOnlyTests,
	}

	fmBytes, err := yaml.Marshal(fm)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("---\n%s---\n\n%s\n", string(fmBytes), strings.TrimSpace(wo.Body)), nil
}
