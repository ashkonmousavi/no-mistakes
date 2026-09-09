package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// stagePipelineChanges guards every pipeline-owned catch-all staging path,
// including Push's leftover commit. Refusal preserves the index and worktree.
func stagePipelineChanges(sctx *pipeline.StepContext) error {
	if len(sctx.Config.ProtectedPaths) > 0 {
		// Disable renames so both source and destination are checked, and list
		// individual untracked files so a protected path inside a new directory
		// cannot hide behind the directory entry. NULs preserve unusual names.
		status, err := stepGitRunRaw(sctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
		if err != nil {
			return fmt.Errorf("check protected_paths: %w", err)
		}
		for _, entry := range strings.Split(strings.TrimSuffix(status, "\x00"), "\x00") {
			if entry == "" {
				continue
			}
			if len(entry) < 4 || entry[2] != ' ' {
				return fmt.Errorf("check protected_paths: invalid git status entry %q", entry)
			}
			if err := assertNoProtectedPaths(sctx, []string{entry[3:]}); err != nil {
				return err
			}
		}
	}
	_, err := stepGitRun(sctx, "add", "-A")
	return err
}

// assertNoProtectedPaths refuses when any of the given repository-relative
// paths matches a protected_paths rule. It is the pattern half of the guard
// above, shared with the path-scoped document correction commit, which already
// knows exactly which paths it is about to stage and so needs no status read
// of its own. Keeping one matcher means a protected path cannot be refused by
// the catch-all staging path and quietly accepted by the scoped one.
func assertNoProtectedPaths(sctx *pipeline.StepContext, paths []string) error {
	if sctx.Config == nil || len(sctx.Config.ProtectedPaths) == 0 {
		return nil
	}
	for _, file := range paths {
		for _, pattern := range sctx.Config.ProtectedPaths {
			if matchIgnorePattern(file, pattern) {
				return &pipeline.ProtectedPathError{Path: file, Rule: pattern}
			}
		}
	}
	return nil
}
