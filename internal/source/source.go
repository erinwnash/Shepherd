// Package source implements pluggable queue sources per brief §4:
// markdown-dir, github-issues, and json-file.
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/erinwnash/Shepherd/internal/workorder"
)

// QueueSource defines the interface for pulling work orders into the controller.
type QueueSource interface {
	FetchWorkOrders(ctx context.Context) ([]*workorder.WorkOrder, error)
}

// MarkdownDirSource loads work orders from a local markdown plan directory tree.
type MarkdownDirSource struct {
	Root string
}

func (m *MarkdownDirSource) FetchWorkOrders(ctx context.Context) ([]*workorder.WorkOrder, error) {
	if m.Root == "" {
		m.Root = filepath.Join("docs", "plan")
	}

	var orders []*workorder.WorkOrder
	err := filepath.Walk(m.Root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := strings.ToLower(info.Name())
			if name == "_done" || name == ".done" || strings.HasSuffix(name, "done") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".md") || strings.EqualFold(info.Name(), "readme.md") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		wo, err := workorder.Parse(p, data)
		if err != nil {
			// Skip files that aren't valid work orders
			return nil
		}
		orders = append(orders, wo)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return orders, nil
}

// JSONFileSource loads work orders from a static JSON file.
type JSONFileSource struct {
	Path string
}

func (j *JSONFileSource) FetchWorkOrders(ctx context.Context) ([]*workorder.WorkOrder, error) {
	data, err := os.ReadFile(j.Path)
	if err != nil {
		return nil, fmt.Errorf("read json source: %w", err)
	}

	var orders []*workorder.WorkOrder
	if err := json.Unmarshal(data, &orders); err != nil {
		return nil, fmt.Errorf("unmarshal json work orders: %w", err)
	}
	return orders, nil
}

// GitHubIssue represents the JSON structure returned by 'gh issue list --json'.
type GitHubIssue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

// GitHubIssuesSource imports work orders from GitHub issues via gh CLI or raw JSON payload.
type GitHubIssuesSource struct {
	Repo     string
	JSONData []byte // optional raw data override for testing or offline piping
}

func (g *GitHubIssuesSource) FetchWorkOrders(ctx context.Context) ([]*workorder.WorkOrder, error) {
	var raw []byte
	if len(g.JSONData) > 0 {
		raw = g.JSONData
	} else {
		if g.Repo == "" {
			return nil, errors.New("github-issues source requires repo name (e.g. 'owner/repo')")
		}
		cmd := exec.CommandContext(ctx, "gh", "issue", "list", "--repo", g.Repo, "--state", "open", "--json", "number,title,body")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("gh issue list: %w", err)
		}
		raw = out
	}

	var issues []GitHubIssue
	if err := json.Unmarshal(raw, &issues); err != nil {
		return nil, fmt.Errorf("parse github issues json: %w", err)
	}

	var orders []*workorder.WorkOrder
	for _, issue := range issues {
		id := fmt.Sprintf("GH-%d", issue.Number)
		wo, err := workorder.Parse(fmt.Sprintf("issue-%d.md", issue.Number), []byte(issue.Body))
		if err != nil {
			// If body lacks strict frontmatter, create a standard workorder from issue metadata
			wo = &workorder.WorkOrder{
				ID:     id,
				Title:  issue.Title,
				Body:   issue.Body,
				Status: workorder.StatusPending,
				Effort: workorder.EffortM,
				Review: workorder.ReviewLow,
				Files:  []workorder.File{{Path: "src/issue.txt", New: true}},
			}
		} else {
			if wo.ID == "" {
				wo.ID = id
			}
		}
		orders = append(orders, wo)
	}

	return orders, nil
}

// MarkDone updates a markdown work order file's frontmatter status to 'done'.
func MarkDone(path, chunkID string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(data)
	statuses := []string{"pending", "in-progress", "in_progress", "running", "held", "blocked"}
	updated := content
	for _, s := range statuses {
		updated = strings.Replace(content, "status: "+s, "status: done", 1)
		if updated != content {
			break
		}
		updated = strings.Replace(content, fmt.Sprintf("status: %q", s), "status: done", 1)
		if updated != content {
			break
		}
	}
	if updated == content {
		return nil
	}
	return os.WriteFile(path, []byte(updated), 0644)
}

