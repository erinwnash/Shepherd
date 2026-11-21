package scope

import "testing"

func TestValidatePattern(t *testing.T) {
	ok := []string{
		"src/api/notes.ts",
		"tests/api/notes.test.ts",
		"supabase/migrations/<next>_x.sql",
		"docs/My File.md",
		"a",
		// Framework route folders (Next.js, SvelteKit, Nuxt) are literal names, never globs.
		"src/app/projects/[projectId]/page.tsx",
		"src/routes/[...rest]/+page.svelte",
		".env.example",
	}
	for _, p := range ok {
		if err := ValidatePattern(p); err != nil {
			t.Errorf("ValidatePattern(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{
		"",
		"/etc/passwd",
		"C:/x.ts",
		`src\api.ts`,
		"../outside.ts",
		"src/../../x",
		"./src/x.ts",
		"src//x.ts",
		"src/x.ts/",
		"-rf",
		"src/-x.ts",
		" src/x.ts",
		"src/x.ts ",
		"src/\x00x.ts",
		"src/*.ts",
		"src/x?.ts",
		"src/<unclosed.ts",
		"~/x",
		// Windows drops trailing dots and spaces, so these alias "src/x" and "src/x/a".
		"src/x.",
		"src/x /a",
		".../projects/x.ts",
	}
	for _, p := range bad {
		if err := ValidatePattern(p); err == nil {
			t.Errorf("ValidatePattern(%q) = nil, want error", p)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"src/a.ts", "src/a.ts", true},
		{"src/a.ts", "src/b.ts", false},
		{"supabase/migrations/<next>_x.sql", "supabase/migrations/0042_x.sql", true},
		{"supabase/migrations/<next>_x.sql", "supabase/migrations/sub/0042_x.sql", false},
		{"supabase/migrations/<next>_x.sql", "supabase/migrations/_x.sql", false},
		{"src/<name>.ts", "src/a.b.ts", true},
		{"src/a.ts", "SRC/a.ts", false},
		// Brackets are literal: "[id]" must not act as a character class.
		{"app/[id]/<f>.tsx", "app/[id]/page.tsx", true},
		{"app/[id]/<f>.tsx", "app/i/page.tsx", false},
		{"app/[id]/<f>.tsx", "app/d/page.tsx", false},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestProtected(t *testing.T) {
	p := NewProtected(nil)
	yes := []string{
		"shepherd.toml",
		"Shepherd.TOML", // case-insensitive file systems (macOS default)
		".shepherd/prompts/generate.md",
		".git/hooks/pre-commit",
		".git",
		".github/workflows/ci.yml",
		".gitlab-ci.yml",
		".githooks/pre-push",
		".husky/pre-commit",
		".gitmodules",
	}
	for _, s := range yes {
		if !p.Is(s) {
			t.Errorf("Is(%q) = false, want true", s)
		}
	}
	no := []string{"src/shepherd.toml.ts", "docs/.github.md", "gitx/a", ".shepherdx/a"}
	for _, s := range no {
		if p.Is(s) {
			t.Errorf("Is(%q) = true, want false", s)
		}
	}
	for _, pat := range []string{"<x>.toml", "<d>/hooks/pre-commit", ".<n>/workflows/ci.yml"} {
		if !p.Covers(pat) {
			t.Errorf("Covers(%q) = false; the placeholder can expand to a protected path", pat)
		}
	}
	for _, pat := range []string{"src/<x>.ts", "<x>.ts"} {
		if p.Covers(pat) {
			t.Errorf("Covers(%q) = true, want false", pat)
		}
	}
	extra := NewProtected([]string{"infra/"})
	if !extra.Is("infra/main.tf") {
		t.Error("extra protected prefix not honoured")
	}
}

func TestOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"src/a.ts", "src/a.ts", true},
		{"src/a.ts", "src/A.ts", true}, // would collide on a case-insensitive FS
		{"src/a.ts", "src/b.ts", false},
		{"m/<n>_x.sql", "m/0001_x.sql", true},
		{"m/<n>_x.sql", "m/<k>_x.sql", true},
		{"m/<n>_x.sql", "m/<k>_y.sql", false},
	}
	for _, c := range cases {
		if got := Overlap(c.a, c.b); got != c.want {
			t.Errorf("Overlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
