package controller

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/plan"
	"github.com/erinwnash/Shepherd/internal/workorder"
)

// doneEntry is a work order found in a done directory.
type doneEntry struct {
	wo   *workorder.WorkOrder
	path string // project-relative, slash-separated
}

// doneDirs lists every existing directory that holds completed work orders: the configured
// done_dir, the conventional <plan>DONE / docs/planDONE / planDONE, and any *done folder inside
// the plan tree (plan.Load skips those).
func (c *Controller) doneDirs() []string {
	cands := []string{c.cfg.Queue.DoneDir, c.planRoot + "DONE", "docs/planDONE", "planDONE"}
	absPlan := c.planRoot
	if !filepath.IsAbs(absPlan) {
		absPlan = filepath.Join(c.projectRoot, filepath.FromSlash(c.planRoot))
	}
	_ = filepath.WalkDir(absPlan, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), "done") {
			cands = append(cands, p)
			return filepath.SkipDir
		}
		return nil
	})
	seen := map[string]bool{}
	var out []string
	for _, cand := range cands {
		if cand == "" {
			continue
		}
		abs := cand
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(c.projectRoot, filepath.FromSlash(cand))
		}
		abs = filepath.Clean(abs)
		if st, err := os.Stat(abs); err != nil || !st.IsDir() || seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

// scanDone parses every work order under the done directories, keyed by chunk id.
func (c *Controller) scanDone() map[string]doneEntry {
	found := map[string]doneEntry{}
	for _, dir := range c.doneDirs() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.Type()&fs.ModeSymlink != 0 || d.IsDir() {
				return nil
			}
			if !strings.EqualFold(filepath.Ext(p), ".md") || plan.ReservedNames[strings.ToLower(d.Name())] {
				return nil
			}
			rel, rerr := filepath.Rel(c.projectRoot, p)
			if rerr != nil || strings.HasPrefix(rel, "..") {
				rel = p
			}
			rel = filepath.ToSlash(rel)
			src, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			wo, perr := workorder.Parse(rel, src)
			if perr != nil || wo.Status == workorder.StatusSkipped {
				return nil
			}
			found[wo.ID] = doneEntry{wo: wo, path: rel}
			return nil
		})
	}
	return found
}

// livePlanIDs is the set of chunk ids that still have a work order in the plan tree.
func (c *Controller) livePlanIDs() map[string]*workorder.WorkOrder {
	live := map[string]*workorder.WorkOrder{}
	orders, _, err := plan.Load(c.projectRoot, c.planRoot)
	if err != nil {
		return live
	}
	for _, wo := range orders {
		live[wo.ID] = wo
	}
	return live
}

// reconcileEntry marks one chunk done in the database because its work order sits in a done
// directory. Running chunks are left to the worker finishing them. It reports the prior status
// ("" for an unknown row) and whether anything changed.
func (c *Controller) reconcileEntry(id string, e doneEntry) (string, bool) {
	rec, _ := c.store.GetChunk(id)
	switch {
	case rec == nil:
		if err := c.store.UpsertChunk(db.ChunkRecord{ID: id, Title: e.wo.Title, Sprint: e.wo.Sprint, Status: "done", Hash: e.wo.Hash, CreatedAt: time.Now().UTC()}); err != nil {
			return "", false
		}
		_ = c.store.SetChunkStatus(id, "done", "reconciled: work order is in "+e.path, "", "")
		c.logReconciled(id, "(new)", e.path)
		return "(new)", true
	case rec.Status == "done" || rec.Status == "running":
		return rec.Status, false
	}
	if err := c.store.SetChunkStatus(id, "done", "reconciled: work order is in "+e.path, "", ""); err != nil {
		return rec.Status, false
	}
	c.logReconciled(id, rec.Status, e.path)
	return rec.Status, true
}

func (c *Controller) logReconciled(id, from, path string) {
	_, _ = c.store.EventLog.Append("queue.reconciled", "reconciler", fmt.Sprintf("chunk %s was %s but its work order is in %s: marked done", id, from, path))
}

// ReconcileDone brings the database in line with work finished outside Shepherd: every work order
// in a done directory is done. A chunk that still has a live copy in the plan tree is left alone.
// It returns the ids it changed and never takes c.mu, so any caller may use it.
func (c *Controller) ReconcileDone() []string {
	entries := c.scanDone()
	if len(entries) == 0 {
		return nil
	}
	live := c.livePlanIDs()
	var changed []string
	for id, e := range entries {
		if _, stillLive := live[id]; stillLive {
			continue
		}
		if _, did := c.reconcileEntry(id, e); did {
			changed = append(changed, id)
		}
	}
	sort.Strings(changed)
	return changed
}

// QueueCheck spot-tests one chunk against the disk, repairs its row if the work order is in a done
// directory, and reports what it found.
func (c *Controller) QueueCheck(ctx context.Context, chunkID string) (control.QueueCheckResult, error) {
	res := control.QueueCheckResult{ChunkID: chunkID}
	rec, _ := c.store.GetChunk(chunkID)
	live := c.livePlanIDs()
	entry, inDone := c.scanDone()[chunkID]
	wo, inPlan := live[chunkID]
	if rec == nil && !inPlan && !inDone {
		return res, fmt.Errorf("unknown chunk %q: not in the database, the plan tree or a done directory", chunkID)
	}
	if rec != nil {
		res.Before = rec.Status
	}
	res.InPlanTree = inPlan
	if inPlan {
		res.PlanPath = wo.Path
	} else if inDone {
		wo = entry.wo
	}
	if inDone {
		res.DonePath = entry.path
		if !inPlan {
			if _, did := c.reconcileEntry(chunkID, entry); did {
				res.Reconciled = true
			}
		}
	}
	res.After = res.Before
	if now, _ := c.store.GetChunk(chunkID); now != nil {
		res.After = now.Status
	}
	switch {
	case res.Reconciled:
		res.Note = "database row was stale; marked done"
	case inDone && inPlan:
		res.Note = "a copy also exists in the plan tree, so the plan copy wins: nothing changed"
	case inDone && res.After == "running":
		res.Note = "work order is in a done directory but the chunk is running: left to the worker"
	case rec == nil:
		res.Note = "in the plan tree but not yet in the database: run queue refresh"
	default:
		res.Note = "consistent"
	}
	if wo != nil {
		for _, dep := range wo.DependsOn {
			st := "unknown (not in database)"
			if r, _ := c.store.GetChunk(dep); r != nil {
				st = r.Status
			}
			res.DependsOn = append(res.DependsOn, dep+"="+st)
		}
	}
	return res, nil
}
