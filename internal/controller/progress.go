package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/erinwnash/Shepherd/internal/control"
	"github.com/erinwnash/Shepherd/internal/notify"
	"github.com/erinwnash/Shepherd/internal/redact"
	"github.com/erinwnash/Shepherd/internal/verify"
)

// Chunk phases, in the order a chunk passes through them.
const (
	phaseAgent  = "agent"  // the model is writing the change
	phaseVerify = "verify" // gates are running on the result
	phaseRepair = "repair" // a repair attempt is writing a fix
	phaseCommit = "commit" // the verified change is being committed or landed
)

const (
	progressTailLines   = 200 // lines of adapter output kept per chunk
	progressTailMaxLine = 400 // longer lines are cut
	progressTailForAPI  = 12  // lines sent to the dashboard in a status poll
	progressDiffPoll    = 5 * time.Second
	progressStallAfter  = 10 * time.Minute
)

// Test seams.
var (
	progressPollInterval = progressDiffPoll
	progressStallWindow  = progressStallAfter
)

// chunkProgress is the live, in-memory view of one chunk being worked on. It is never persisted:
// the durable record is the chunk.phase / chunk.stalled events.
type chunkProgress struct {
	id         string
	lane       string // "main" or "frontrun"
	phase      string
	detail     string
	attempt    int
	started    time.Time
	phaseSince time.Time

	files, added, deleted int
	fingerprint           string
	lastChange            time.Time
	stalled               bool

	tail    []string // completed lines, newest last
	partial string   // the unfinished line
}

type progressTracker struct {
	mu sync.Mutex
	m  map[string]*chunkProgress
}

func (c *Controller) progBegin(id, lane string) {
	now := time.Now()
	c.prog.mu.Lock()
	defer c.prog.mu.Unlock()
	if c.prog.m == nil {
		c.prog.m = map[string]*chunkProgress{}
	}
	c.prog.m[id] = &chunkProgress{id: id, lane: lane, started: now, phaseSince: now, lastChange: now}
}

func (c *Controller) progEnd(id string) {
	c.prog.mu.Lock()
	delete(c.prog.m, id)
	c.prog.mu.Unlock()
}

// progPhase moves a chunk to a new phase, records it as an event, and restarts the stall clock.
func (c *Controller) progPhase(id, phase, detail string, attempt int) {
	now := time.Now()
	c.prog.mu.Lock()
	p := c.prog.m[id]
	if p == nil {
		c.prog.mu.Unlock()
		return
	}
	p.phase, p.detail, p.attempt = phase, detail, attempt
	p.phaseSince, p.lastChange, p.stalled = now, now, false
	elapsed := now.Sub(p.started).Round(time.Second)
	c.prog.mu.Unlock()

	msg := fmt.Sprintf("chunk %s: %s", id, phase)
	if detail != "" {
		msg += " (" + detail + ")"
	}
	if attempt > 1 {
		msg += fmt.Sprintf(" attempt %d", attempt)
	}
	msg += fmt.Sprintf(", %s since start", elapsed)
	_, _ = c.store.EventLog.Append("chunk.phase", "worker", msg)
}

// progOutput feeds adapter output into the chunk's tail. It runs on the adapter's writer goroutine
// and must be quick; redaction happens when the tail is read.
func (c *Controller) progOutput(id string) func([]byte) {
	return func(b []byte) {
		c.prog.mu.Lock()
		defer c.prog.mu.Unlock()
		p := c.prog.m[id]
		if p == nil {
			return
		}
		p.partial += string(b)
		for {
			i := strings.IndexByte(p.partial, '\n')
			if i < 0 {
				break
			}
			line := cleanTailLine(p.partial[:i])
			p.partial = p.partial[i+1:]
			if line == "" {
				continue
			}
			p.tail = append(p.tail, line)
			if len(p.tail) > progressTailLines {
				p.tail = p.tail[len(p.tail)-progressTailLines:]
			}
		}
		if len(p.partial) > 4*progressTailMaxLine {
			p.partial = p.partial[len(p.partial)-progressTailMaxLine:]
		}
	}
}

// cleanTailLine drops control characters (terminal escapes, carriage-return redraws) and bounds the length.
func cleanTailLine(s string) string {
	if i := strings.LastIndexByte(s, '\r'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	var b strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case esc:
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				esc = false
			}
		case r == 0x1b:
			esc = true
		case r == '\t':
			b.WriteString("  ")
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if r := []rune(out); len(r) > progressTailMaxLine {
		out = string(r[:progressTailMaxLine]) + "…"
	}
	return out
}

// progWatch samples the chunk's worktree until ctx ends: how many files changed and by how much.
// A chunk whose agent or repair phase shows no movement for the stall window is reported once.
func (c *Controller) progWatch(ctx context.Context, id string, g *verify.GitRunner) {
	t := time.NewTicker(progressPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.progSample(ctx, id, g)
		}
	}
}

func (c *Controller) progSample(ctx context.Context, id string, g *verify.GitRunner) {
	paths, perr := g.ChangedPaths(ctx)
	stat, serr := g.Numstat(ctx, false, "")
	if perr != nil && serr != nil {
		return
	}
	added, deleted := 0, 0
	var fp strings.Builder
	for _, e := range stat {
		added += e.Added
		deleted += e.Deleted
		fmt.Fprintf(&fp, "%s:%d:%d;", e.Path, e.Added, e.Deleted)
	}
	for _, p := range paths {
		fp.WriteString(p + ";")
	}

	now := time.Now()
	var alert string
	c.prog.mu.Lock()
	p := c.prog.m[id]
	if p == nil {
		c.prog.mu.Unlock()
		return
	}
	p.files, p.added, p.deleted = len(paths), added, deleted
	if fp.String() != p.fingerprint {
		p.fingerprint = fp.String()
		p.lastChange = now
		p.stalled = false
	} else if (p.phase == phaseAgent || p.phase == phaseRepair) && !p.stalled && now.Sub(p.lastChange) >= progressStallWindow {
		p.stalled = true
		alert = fmt.Sprintf("chunk %s has shown no file changes for %s in the %s phase (%d files, +%d/-%d so far)",
			id, now.Sub(p.lastChange).Round(time.Second), p.phase, p.files, p.added, p.deleted)
	}
	c.prog.mu.Unlock()

	if alert != "" {
		_, _ = c.store.EventLog.Append("chunk.stalled", "worker", alert)
		c.notifyEvent(ctx, notify.EventChunkStalled, "Chunk Silent: "+id, alert)
	}
}

// progressSnapshot is the dashboard's view of everything in flight, main lane first.
func (c *Controller) progressSnapshot() []control.ChunkProgress {
	now := time.Now()
	c.prog.mu.Lock()
	defer c.prog.mu.Unlock()
	var out []control.ChunkProgress
	for _, p := range c.prog.m {
		cp := control.ChunkProgress{
			ID: p.id, Lane: p.lane, Phase: p.phase, Detail: p.detail, Attempt: p.attempt,
			PhaseSec: int(now.Sub(p.phaseSince).Seconds()), TotalSec: int(now.Sub(p.started).Seconds()),
			Files: p.files, Added: p.added, Deleted: p.deleted,
			IdleSec: int(now.Sub(p.lastChange).Seconds()), Stalled: p.stalled,
		}
		lines := p.tail
		if p.partial != "" {
			if l := cleanTailLine(p.partial); l != "" {
				lines = append(append([]string{}, lines...), l)
			}
		}
		if len(lines) > progressTailForAPI {
			lines = lines[len(lines)-progressTailForAPI:]
		}
		for _, l := range lines {
			cp.Tail = append(cp.Tail, redact.Sanitize(l))
		}
		out = append(out, cp)
	}
	sortProgress(out)
	return out
}

func sortProgress(s []control.ChunkProgress) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && (s[j].Lane < s[j-1].Lane || (s[j].Lane == s[j-1].Lane && s[j].ID < s[j-1].ID)); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
