package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func UnfinishedOperation(ctx context.Context, workDir string) (bool, error) {
	for _, name := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply"} {
		path, err := Run(ctx, workDir, "rev-parse", "--git-path", name)
		if err != nil {
			return false, fmt.Errorf("inspect unfinished Git operation: %w", err)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workDir, path)
		}
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("inspect %s: %w", name, err)
		}
	}
	unmerged, err := Run(ctx, workDir, "ls-files", "--unmerged", "-z")
	if err != nil {
		return false, fmt.Errorf("inspect unmerged index: %w", err)
	}
	return strings.TrimSpace(unmerged) != "", nil
}
