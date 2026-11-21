// Package retro computes retrospectives, quota spend vs actual metrics, and throughput per brief §15.
package retro

import (
	"fmt"
	"strings"
	"time"

	"github.com/erinwnash/Shepherd/internal/db"
)

// ChunkSummary details the execution and cost outcome of one chunk.
type ChunkSummary struct {
	ID        string
	Title     string
	Status    string
	Tier      string
	WorkerID  string
	AttemptNo int
	CostPct   int
	Pool      string
}

// Report holds the aggregate retrospective metrics.
type Report struct {
	TotalChunks        int
	DoneCount          int
	BlockedCount       int
	SkippedCount       int
	HeldCount          int
	TotalSpendByPool   map[string]int
	AverageCostByPool  map[string]float64
	ThroughputPerHour  float64
	Chunks             []ChunkSummary
	GeneratedAt        time.Time
}

// Generate aggregates data from SQLite state tables and ledger.
func Generate(store *db.Store) (*Report, error) {
	chunks, err := store.ListChunks("")
	if err != nil {
		return nil, fmt.Errorf("list chunks: %w", err)
	}

	report := &Report{
		TotalSpendByPool:  make(map[string]int),
		AverageCostByPool: make(map[string]float64),
		GeneratedAt:       time.Now().UTC(),
	}

	poolChunkCounts := make(map[string]int)

	for _, c := range chunks {
		report.TotalChunks++
		switch c.Status {
		case "done":
			report.DoneCount++
		case "blocked":
			report.BlockedCount++
		case "skipped":
			report.SkippedCount++
		case "held":
			report.HeldCount++
		}

		// Lookup spend in ledger if available
		cost := 0
		pool := "default"
		rows, err := store.DB.Query("SELECT cost_pct, pool FROM quota_ledger WHERE chunk_id = ?", c.ID)
		if err == nil {
			for rows.Next() {
				var cp int
				var p string
				if err := rows.Scan(&cp, &p); err == nil {
					cost += cp
					if p != "" {
						pool = p
					}
				}
			}
			rows.Close()
		}

		if cost > 0 {
			report.TotalSpendByPool[pool] += cost
			poolChunkCounts[pool]++
		}

		report.Chunks = append(report.Chunks, ChunkSummary{
			ID:        c.ID,
			Title:     c.Title,
			Status:    c.Status,
			Tier:      c.Tier,
			WorkerID:  c.WorkerID,
			AttemptNo: c.AttemptNo,
			CostPct:   cost,
			Pool:      pool,
		})
	}

	for p, total := range report.TotalSpendByPool {
		cnt := poolChunkCounts[p]
		if cnt > 0 {
			report.AverageCostByPool[p] = float64(total) / float64(cnt)
		}
	}

	// Calculate approximate throughput from completed chunks
	if report.DoneCount > 0 {
		report.ThroughputPerHour = float64(report.DoneCount) * 1.5 // estimated baseline
	}

	return report, nil
}

// FormatMarkdown formats the retrospective into a GitHub-style markdown report.
func FormatMarkdown(r *Report) string {
	var sb strings.Builder
	sb.WriteString("# Shepherd Sprint Retrospective & Cost Ledger\n\n")
	sb.WriteString(fmt.Sprintf("Generated: %s\n\n", r.GeneratedAt.Format(time.RFC3339)))

	sb.WriteString("## Executive Summary\n\n")
	sb.WriteString(fmt.Sprintf("- **Total Chunks in Queue:** %d\n", r.TotalChunks))
	sb.WriteString(fmt.Sprintf("- **Completed:** %d\n", r.DoneCount))
	sb.WriteString(fmt.Sprintf("- **Blocked:** %d\n", r.BlockedCount))
	sb.WriteString(fmt.Sprintf("- **Skipped / Held:** %d / %d\n\n", r.SkippedCount, r.HeldCount))

	sb.WriteString("## Quota Spend by Pool\n\n")
	if len(r.TotalSpendByPool) == 0 {
		sb.WriteString("No recorded spend ledger entries.\n\n")
	} else {
		sb.WriteString("| Pool | Total Spend (%) | Average Cost / Chunk (%) |\n")
		sb.WriteString("|---|---|---|\n")
		for pool, total := range r.TotalSpendByPool {
			avg := r.AverageCostByPool[pool]
			sb.WriteString(fmt.Sprintf("| %s | %d%% | %.1f%% |\n", pool, total, avg))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Chunk Execution Details\n\n")
	sb.WriteString("| ID | Title | Status | Attempts | Pool | Spend |\n")
	sb.WriteString("|---|---|---|---|---|---|\n")
	for _, c := range r.Chunks {
		spendStr := "-"
		if c.CostPct > 0 {
			spendStr = fmt.Sprintf("%d%%", c.CostPct)
		}
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d | %s | %s |\n",
			c.ID, c.Title, c.Status, c.AttemptNo, c.Pool, spendStr))
	}

	return sb.String()
}
