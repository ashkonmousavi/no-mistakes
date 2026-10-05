package custody

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// WorkNeedsRescue includes ignored content and unfinished Git operations.
// Unknown states are errors, never permission to remove a checkout.
func WorkNeedsRescue(ctx context.Context, dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if len(entries) == 0 {
		return false, nil
	}
	// Status can invoke a configured clean/process filter to compare a file
	// with the index. Inspect names only, then use raw capture instead.
	filters, err := git.Run(ctx, dir, "config", "--name-only", "--get-regexp", `^filter\..*\.(clean|process)$`)
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return true, err
		}
	} else if filters != "" {
		return true, nil
	}
	status, err := git.RunWithEnv(ctx, dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "-c", "core.fsmonitor=false", "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return true, err
	}
	if status != "" {
		return true, nil
	}
	ignored, err := git.RunRaw(ctx, dir, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return true, err
	}
	if len(ignored) > 0 {
		return true, nil
	}
	gitDir, err := git.Run(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return true, err
	}
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDir, marker)); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return true, err
		}
	}
	return false, nil
}
