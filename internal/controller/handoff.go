package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/erinwnash/Shepherd/internal/repair"
	"github.com/erinwnash/Shepherd/internal/verify"
)

var unsafeIDChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// handoffDir is where workers leave their per-chunk notes (gitignored, never staged: the
// scope gate and stageChunkFiles already exclude .shepherd/).
func (c *Controller) handoffDir() string {
	return filepath.Join(c.projectRoot, ".shepherd", "handoff")
}

func (c *Controller) handoffFile(chunkID string) string {
	return filepath.Join(c.handoffDir(), unsafeIDChars.ReplaceAllString(chunkID, "_")+".md")
}

func (c *Controller) handoffOn() bool    { return c.cfg.Repair.Handoff.IsEnabled() }
func (c *Controller) workerNoteOn() bool { return c.cfg.Repair.Handoff.WorkerNoteEnabled() }

// handoffInstructions returns the prompt block asking for a note ("" when disabled) and makes
// sure the directory exists so the worker can write to it.
func (c *Controller) handoffInstructions(chunkID string) string {
	if !c.workerNoteOn() {
		return ""
	}
	_ = os.MkdirAll(c.handoffDir(), 0o755)
	rel := ".shepherd/handoff/" + unsafeIDChars.ReplaceAllString(chunkID, "_") + ".md"
	return strings.ReplaceAll(repair.HandoffInstructions(chunkID), ".shepherd/handoff/"+chunkID+".md", rel)
}

// readHandoffNote returns the sanitised, capped note for a chunk, or "".
func (c *Controller) readHandoffNote(chunkID string) string {
	if !c.workerNoteOn() {
		return ""
	}
	f, err := os.Open(c.handoffFile(chunkID))
	if err != nil {
		return ""
	}
	defer f.Close()
	max := c.cfg.Repair.Handoff.MaxNoteBytes
	if max <= 0 {
		max = repair.DefaultMaxNoteBytes
	}
	// Read a little over the cap so truncation lands on a line boundary.
	buf := make([]byte, max*2)
	n, _ := f.Read(buf)
	return repair.SanitizeNote(string(buf[:n]), max)
}

// clearHandoffNote removes a chunk's note (chunk completed, skipped, or restarted at attempt 1).
func (c *Controller) clearHandoffNote(chunkID string) {
	_ = os.Remove(c.handoffFile(chunkID))
}

// maskDirsFor masks Shepherd's state dir from the sandboxed worker, except the handoff folder
// when worker notes are enabled. Children are masked individually so the worker can still
// write its note.
func (c *Controller) maskDirsFor(stateDir string) []string {
	if !c.workerNoteOn() {
		return []string{stateDir}
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return []string{stateDir}
	}
	var out []string
	for _, e := range entries {
		if e.Name() == "handoff" {
			continue
		}
		out = append(out, filepath.Join(stateDir, e.Name()))
	}
	return out
}

// attemptRecord builds the structured record for one attempt from the current tree.
func (c *Controller) attemptRecord(attempt int, tier, model, outcome string, vRes verify.VerifyResult, failures string, paths []string, numstat []verify.NumstatEntry) repair.AttemptRecord {
	rec := repair.AttemptRecord{Attempt: attempt, Tier: tier, Model: model, Outcome: outcome, Failures: failures}
	if !c.handoffOn() {
		return rec
	}
	rec.Failures = failures
	stats := map[string]verify.NumstatEntry{}
	for _, n := range numstat {
		stats[n.Path] = n
	}
	for _, p := range paths {
		if len(rec.ChangedFiles) >= 15 {
			break
		}
		n := stats[p]
		rec.ChangedFiles = append(rec.ChangedFiles, repair.FileChange{Path: sanitizeLine(p), Added: n.Added, Removed: n.Deleted})
	}
	seen := map[string]bool{}
	add := func(s string) {
		s = sanitizeLine(s)
		if s != "" && !seen[s] && len(rec.FailedChecks) < 10 {
			seen[s] = true
			rec.FailedChecks = append(rec.FailedChecks, s)
		}
	}
	for _, g := range vRes.Results {
		if !g.Passed && !g.Skipped {
			add(g.Name)
		}
	}
	for _, n := range repair.ParseFailedChecks(vRes.Errors) {
		add(n)
	}
	return rec
}

func sanitizeLine(s string) string {
	return strings.TrimSpace(repair.SanitizeNote(strings.ReplaceAll(s, "\n", " "), 256))
}

// handoffOptions returns the prompt options for the next repair round.
func (c *Controller) handoffOptions(chunkID string, attempts []repair.AttemptRecord, treeHasChanges bool) repair.HandoffOptions {
	h := c.cfg.Repair.Handoff
	opts := repair.HandoffOptions{
		Enabled:        c.handoffOn(),
		MaxOutputLines: h.MaxOutputLines,
		TreeIntact:     treeHasChanges,
	}
	if !opts.Enabled {
		return opts
	}
	opts.Note = c.readHandoffNote(chunkID)
	if opts.Note != "" && len(attempts) > 0 {
		opts.NoteAuthor = attempts[len(attempts)-1].Tier
	}
	return opts
}
