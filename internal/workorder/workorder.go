// Package workorder parses chunk work orders: YAML frontmatter (control data, validated
// strictly) followed by a markdown body (prompt material, split into "## " sections).
//
// Built from scratch: the schema is Shepherd's own domain (ADR-0005). YAML parsing itself is
// go.yaml.in/yaml/v3, restricted here to a safe subset.
package workorder

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/erinwnash/Shepherd/internal/scope"
	"go.yaml.in/yaml/v3"
)

// MaxFrontmatterBytes caps the frontmatter block; work orders are small by design.
const MaxFrontmatterBytes = 16 * 1024

type Review string

const (
	ReviewLow    Review = "LOW"
	ReviewMedium Review = "MEDIUM"
	ReviewHigh   Review = "HIGH"
)

type Effort string

const (
	EffortS Effort = "S"
	EffortM Effort = "M"
	EffortL Effort = "L"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusBlocked Status = "blocked"
	StatusSkipped Status = "skipped"
)

// File is one declared path. New marks a file the chunk creates ("new:" prefix).
type File struct {
	Path string
	New  bool
}

// WorkOrder is a parsed, validated chunk.
type WorkOrder struct {
	Path          string // where it was read from, for messages
	ID            string
	Title         string
	Epic          string
	Sprint        string
	DependsOn     []string
	Review        Review
	Effort        Effort
	Files         []File
	Tests         []string
	NoTestsReason string
	ReadOnlyTests bool
	ADR           []string
	Status        Status
	Verified      *bool
	Body          string
	Sections      map[string]string // lower-cased "## " heading -> text
	Hash          string            // sha256 of the normalised source (LF, no BOM)
}

// HasSection reports whether the body has a non-empty section with this heading.
func (w *WorkOrder) HasSection(name string) bool {
	return strings.TrimSpace(w.Sections[strings.ToLower(name)]) != ""
}

// ChunkGroup returns the parent group identifier for a chunk ID (e.g. "45A" for "45A.1",
// "51B" for "51B.11", or "24L" for an unsplit chunk "24L").
func ChunkGroup(id string) string {
	if i := strings.LastIndexByte(id, '.'); i > 0 {
		return id[:i]
	}
	return id
}

type frontmatter struct {
	ID            string   `yaml:"id"`
	Title         string   `yaml:"title"`
	Epic          string   `yaml:"epic"`
	Sprint        string   `yaml:"sprint"`
	DependsOn     []string `yaml:"depends_on"`
	Review        string   `yaml:"review"`
	Effort        string   `yaml:"est_effort"`
	Files         []string `yaml:"files"`
	Tests         []string `yaml:"tests"`
	NoTestsReason string   `yaml:"no_tests_reason"`
	ReadOnlyTests bool     `yaml:"read_only_tests"`
	ADR           []string `yaml:"adr"`
	Status        string   `yaml:"status"`
	Verified      *bool    `yaml:"verified,omitempty"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ParseError lists every problem found in one work order.
type ParseError struct {
	Path     string
	Problems []string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, strings.Join(e.Problems, "; "))
}

// Parse reads one work order. Line endings and a UTF-8 BOM are normalised first, so a CRLF
// checkout parses and hashes identically to an LF one.
func Parse(path string, src []byte) (*WorkOrder, error) {
	text := strings.ReplaceAll(string(bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))), "\r\n", "\n")
	fail := func(msg string) (*WorkOrder, error) {
		return nil, &ParseError{Path: path, Problems: []string{msg}}
	}

	if !strings.HasPrefix(text, "---\n") {
		return fail("no frontmatter: the file must start with a '---' line")
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---\n")
	var fm, body string
	switch {
	case end >= 0:
		fm, body = rest[:end+1], rest[end+5:]
	case strings.HasSuffix(rest, "\n---"):
		fm, body = rest[:len(rest)-3], ""
	default:
		return fail("frontmatter is not closed by a '---' line")
	}
	if len(fm) > MaxFrontmatterBytes {
		return fail(fmt.Sprintf("frontmatter is larger than %d bytes", MaxFrontmatterBytes))
	}

	var node yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &node); err != nil {
		return fail("frontmatter: " + err.Error())
	}
	if err := checkSafe(&node); err != nil {
		return fail("frontmatter: " + err.Error())
	}
	var raw frontmatter
	dec := yaml.NewDecoder(strings.NewReader(fm))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return fail("frontmatter: " + err.Error())
	}

	sum := sha256.Sum256([]byte(text))
	w := &WorkOrder{
		Path:          path,
		ID:            raw.ID,
		Title:         strings.TrimSpace(raw.Title),
		Epic:          raw.Epic,
		Sprint:        raw.Sprint,
		DependsOn:     raw.DependsOn,
		NoTestsReason: strings.TrimSpace(raw.NoTestsReason),
		ReadOnlyTests: raw.ReadOnlyTests,
		ADR:           raw.ADR,
		Tests:         raw.Tests,
		Verified:      raw.Verified,
		Body:          body,
		Sections:      sections(body),
		Hash:          hex.EncodeToString(sum[:]),
	}
	var probs []string
	add := func(f string, a ...any) { probs = append(probs, fmt.Sprintf(f, a...)) }

	switch {
	case raw.ID == "":
		add("id is required")
	case !idPattern.MatchString(raw.ID):
		add("id %q: letters, digits, '.', '_' and '-' only, starting with a letter or digit", raw.ID)
	}
	if w.Title == "" {
		add("title is required")
	}
	for _, d := range raw.DependsOn {
		if !idPattern.MatchString(d) {
			add("depends_on: %q is not a valid id", d)
		} else if d == raw.ID {
			add("depends_on: a chunk cannot depend on itself")
		}
	}

	revStr := strings.ToUpper(orDefault(raw.Review, "LOW"))
	if revStr == "MED" {
		revStr = "MEDIUM"
	}
	w.Review = Review(revStr)
	if w.Review != ReviewLow && w.Review != ReviewMedium && w.Review != ReviewHigh {
		add("review %q: want LOW, MEDIUM or HIGH", raw.Review)
	}
	w.Effort = Effort(strings.ToUpper(orDefault(raw.Effort, "S")))
	if w.Effort != EffortS && w.Effort != EffortM && w.Effort != EffortL {
		add("est_effort %q: want S, M or L", raw.Effort)
	}
	w.Status = Status(strings.ToLower(orDefault(raw.Status, "pending")))
	switch w.Status {
	case StatusPending, StatusRunning, StatusDone, StatusBlocked, StatusSkipped:
	default:
		add("status %q: want pending, running, done, blocked or skipped", raw.Status)
	}

	if len(raw.Files) == 0 {
		add("files is required: list every file this chunk may change")
	}
	for _, f := range raw.Files {
		entry := File{Path: f}
		if p, ok := strings.CutPrefix(f, "new:"); ok {
			entry = File{Path: p, New: true}
		}
		if err := scope.ValidatePattern(entry.Path); err != nil {
			add("files: %v", err)
			continue
		}
		w.Files = append(w.Files, entry)
	}
	for _, t := range raw.Tests {
		if strings.HasPrefix(t, "new:") {
			add("tests: %q: mark new test files with new: under files, not tests", t)
			continue
		}
		if err := scope.ValidatePattern(t); err != nil {
			add("tests: %v", err)
		} else if scope.HasPlaceholder(t) {
			add("tests: %q: placeholders are not allowed in test paths", t)
		}
	}

	if len(probs) > 0 {
		return nil, &ParseError{Path: path, Problems: probs}
	}
	return w, nil
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return strings.TrimSpace(s)
}

// checkSafe rejects YAML features that have no place in a work order and make its meaning
// harder to see in review: anchors, aliases, merge keys and explicit tags.
func checkSafe(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode {
		return errors.New("aliases are not allowed")
	}
	if n.Anchor != "" {
		return fmt.Errorf("anchors are not allowed (&%s)", n.Anchor)
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return fmt.Errorf("explicit tags are not allowed (%s)", n.Tag)
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "<<" {
				return errors.New("merge keys are not allowed")
			}
		}
	}
	for _, c := range n.Content {
		if err := checkSafe(c); err != nil {
			return err
		}
	}
	return nil
}

// sections splits a markdown body on "## " headings.
func sections(body string) map[string]string {
	out := map[string]string{}
	cur := ""
	var buf strings.Builder
	flush := func() {
		if cur != "" {
			out[cur] = buf.String()
		}
		buf.Reset()
	}
	for _, line := range strings.Split(body, "\n") {
		if h, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			cur = strings.ToLower(strings.TrimSpace(h))
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	flush()
	return out
}
