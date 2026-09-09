package pipeline

import (
	"errors"
	"strings"
	"testing"
)

// TestDocumentClassAdvance_FailsClosedWithoutProvenDocumentationOnlyChanges
// proves the rule that lets a head advance skip Review is positive evidence
// only: a git failure, an unreadable diff, and an advance that changed no file
// at all all answer "not documentation-only", so the ordinary Review restart
// keeps the head.
func TestDocumentClassAdvance_FailsClosedWithoutProvenDocumentationOnlyChanges(t *testing.T) {
	patterns := DefaultDocumentCorrectionPaths

	t.Run("git error refuses and reports", func(t *testing.T) {
		gitRun := func(args ...string) (string, error) { return "", errors.New("bad object") }
		ok, _, err := DocumentClassAdvance(gitRun, strings.Repeat("a", 40), strings.Repeat("b", 40), patterns)
		if ok {
			t.Fatal("a failed diff must never certify a documentation-only advance")
		}
		if err == nil {
			t.Fatal("a failed diff must be reported, not swallowed")
		}
	})

	t.Run("empty diff is not evidence", func(t *testing.T) {
		gitRun := func(args ...string) (string, error) { return "", nil }
		ok, files, err := DocumentClassAdvance(gitRun, strings.Repeat("a", 40), strings.Repeat("b", 40), patterns)
		if err != nil {
			t.Fatal(err)
		}
		if ok || len(files) != 0 {
			t.Fatal("an advance that changed no file is not proven documentation-only")
		}
	})

	t.Run("one non-documentation path refuses the whole advance", func(t *testing.T) {
		gitRun := func(args ...string) (string, error) {
			return "docs/reference.md\x00internal/handler.go\x00README.md\x00", nil
		}
		ok, files, err := DocumentClassAdvance(gitRun, strings.Repeat("a", 40), strings.Repeat("b", 40), patterns)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("a source file in the advance must send the whole advance back through Review")
		}
		if len(files) != 3 {
			t.Fatalf("files = %v, want the complete changed set for the refusal message", files)
		}
	})

	t.Run("documentation and records only is accepted", func(t *testing.T) {
		gitRun := func(args ...string) (string, error) {
			return "docs/reference.md\x00contracts/rows.md\x00openspec/spec.md\x00README\x00notes.md\x00", nil
		}
		ok, files, err := DocumentClassAdvance(gitRun, strings.Repeat("a", 40), strings.Repeat("b", 40), patterns)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("documentation-and-records advance was refused: %v", files)
		}
	})
}

// TestOutsideDocumentClass_NamesEveryRefusedPathOnce proves the refusal
// message is complete and stable: every path outside the class appears, once,
// in a deterministic order.
func TestOutsideDocumentClass_NamesEveryRefusedPathOnce(t *testing.T) {
	got := OutsideDocumentClass(
		[]string{"internal/b.go", "docs/a.md", "internal/b.go", "  ", "Makefile"},
		DefaultDocumentCorrectionPaths,
	)
	want := []string{"Makefile", "internal/b.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("outside = %v, want %v", got, want)
	}
}

// TestMatchPathGlob_KeepsTheIgnorePatternRules proves the one matcher this
// repository has keeps all three ignore_patterns rules, since the executor's
// path class and the staging guard now both read it.
func TestMatchPathGlob_KeepsTheIgnorePatternRules(t *testing.T) {
	cases := []struct {
		file, pattern string
		want          bool
	}{
		{"docs/guide/a.md", "docs/**", true},
		{"docs", "docs/**", true},
		{"docsx/a.md", "docs/**", false},
		{"deep/nested/a.md", "*.md", true},
		{"deep/nested/a.txt", "*.md", false},
		{"README.md", "README*", true},
		{"docs/README.md", "README*", true},
		{"contracts/rows.md", "contracts/*.md", true},
		{"contracts/deep/rows.md", "contracts/*.md", false},
	}
	for _, tc := range cases {
		if got := MatchPathGlob(tc.file, tc.pattern); got != tc.want {
			t.Errorf("MatchPathGlob(%q, %q) = %v, want %v", tc.file, tc.pattern, got, tc.want)
		}
	}
}
