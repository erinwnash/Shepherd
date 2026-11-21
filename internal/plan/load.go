package plan

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/erinwnash/Shepherd/internal/workorder"
)

// ReservedNames are markdown files in a plan tree that are not chunk work orders.
var ReservedNames = map[string]bool{"epic.md": true, "sprint.md": true, "retro.md": true, "readme.md": true}

// Load reads every work order under planRoot (relative to projectRoot, or absolute). Files that fail to
// parse are returned as "schema" findings rather than aborting, so one lint run shows them all.
func Load(projectRoot, planRoot string) ([]*workorder.WorkOrder, []Finding, error) {
	var orders []*workorder.WorkOrder
	var bad []Finding
	base := filepath.Join(projectRoot, filepath.FromSlash(planRoot))
	if filepath.IsAbs(planRoot) {
		base = planRoot
	}
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return nil, nil, fmt.Errorf("no plan tree at %s (set --plan, or create it)", planRoot)
	}
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow links out of the tree
		}
		if d.IsDir() {
			name := strings.ToLower(d.Name())
			if name == "_done" || name == ".done" || strings.HasSuffix(name, "done") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(p), ".md") || ReservedNames[strings.ToLower(d.Name())] {
			return nil
		}
		rel, rerr := filepath.Rel(projectRoot, p)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			rel, _ = filepath.Rel(base, p) // plan tree outside the project: label relative to it
		}
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		w, perr := workorder.Parse(rel, src)
		if perr != nil {
			bad = append(bad, Finding{Error, rel, "schema", perr.Error()})
			return nil
		}
		orders = append(orders, w)
		return nil
	})
	sort.Slice(orders, func(i, j int) bool { return orders[i].Path < orders[j].Path })
	return orders, bad, err
}

// SprintOrder is the sprint ids in the order their first chunk appears in the tree (paths sort
// by sprint folder, e.g. S16_api-sdk before S17_mcp).
func SprintOrder(orders []*workorder.WorkOrder) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range orders {
		if w.Sprint != "" && !seen[w.Sprint] {
			seen[w.Sprint] = true
			out = append(out, w.Sprint)
		}
	}
	return out
}

// DirRepo is a Repo backed by a project directory.
type DirRepo struct {
	Root   string
	ADRDir string // relative, e.g. "docs/adr"
}

func (r DirRepo) Exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(r.Root, filepath.FromSlash(rel)))
	return err == nil
}

// ADRExists accepts "ADR-0017" or "0017" and looks for docs/adr/0017-*.md.
func (r DirRepo) ADRExists(id string) bool {
	num := strings.TrimPrefix(strings.ToUpper(id), "ADR-")
	if num == "" || strings.Trim(num, "0123456789") != "" {
		return false // digits only: the id is used in a glob
	}
	matches, _ := filepath.Glob(filepath.Join(r.Root, filepath.FromSlash(r.ADRDir), num+"-*.md"))
	return len(matches) > 0
}
