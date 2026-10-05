package custody

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RestorePartialWork requires quiescent writers and verified caller ownership.
// It restores raw working bytes and the independent index at the exact parent,
// never advances HEAD and never executes checkout filters. An interrupted
// restore is accepted only if its entire resulting working/index state matches.
func RestorePartialWork(ctx context.Context, dir string, p *types.PartialWork) error {
	if p == nil || (p.State != "saved" && p.State != "consumed") {
		return fmt.Errorf("partial work is retained or unknown; inspect its original checkout")
	}
	if err := ValidatePartialWork(ctx, dir, p); err != nil {
		return err
	}
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		return err
	}
	if head != p.ParentHead {
		return fmt.Errorf("rescue %s requires exact parent %s, found %s", p.Ref, p.ParentHead, head)
	}
	actual, err := PreservePartialWork(ctx, dir, p.RunID, p.Step, p.StopID)
	if err == nil && actual.State == "saved" && actual.SHA == p.SHA && actual.IndexSHA == p.IndexSHA {
		return nil
	}
	if err != nil || actual == nil || actual.State != "settled" {
		return fmt.Errorf("target worktree is neither clean nor the exact restored rescue %s; retain %s: %w", p.Ref, dir, err)
	}
	// Resolve every path and blob before touching the target. Git trees can be
	// externally constructed, so restoration never trusts path traversal or an
	// embedded .git entry, nor follows a target symlink to write a child.
	tree, err := git.RunRaw(ctx, dir, "ls-tree", "-r", "-z", p.SHA)
	if err != nil {
		return err
	}
	type file struct {
		path, mode string
		bytes      []byte
	}
	var files []file
	for _, entry := range strings.Split(string(tree), "\x00") {
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\t", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid rescue tree")
		}
		fields := strings.Fields(parts[0])
		if len(fields) != 3 || fields[1] != "blob" {
			return fmt.Errorf("unsupported rescue entry")
		}
		if err = restorePath(parts[1]); err != nil {
			return err
		}
		if fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000" {
			return fmt.Errorf("unsupported rescue mode %s", fields[0])
		}
		data, e := git.RunRaw(ctx, dir, "cat-file", "blob", fields[2])
		if e != nil {
			return e
		}
		files = append(files, file{parts[1], fields[0], data})
	}
	tracked, err := git.RunRaw(ctx, dir, "-c", "core.fsmonitor=false", "ls-files", "--cached", "-z")
	if err != nil {
		return err
	}
	var remove []string
	for _, name := range strings.Split(string(tracked), "\x00") {
		if name != "" {
			if err = restorePath(name); err != nil {
				return err
			}
			remove = append(remove, name)
		}
	}
	gitDir, err := git.Run(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(gitDir, "restore-index-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	index := filepath.Join(scratch, "index")
	if _, err = git.RunWithEnv(ctx, dir, []string{"GIT_INDEX_FILE=" + index}, "-c", "core.fsmonitor=false", "read-tree", p.IndexSHA+"^{tree}"); err != nil {
		return err
	}
	staged, err := os.ReadFile(index)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(gitDir, "index.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("lock restored index: %w", err)
	}
	defer os.Remove(lockPath)
	_, writeErr := lock.Write(staged)
	closeErr := lock.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	symlinks, err := git.Run(ctx, dir, "config", "--type=bool", "--default", "true", "--get", "core.symlinks")
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(remove)))
	dirs := map[string]bool{}
	for _, name := range remove {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			dirs[parent] = true
		}
	}
	var parents []string
	for parent := range dirs {
		parents = append(parents, parent)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(parents)))
	for _, parent := range parents {
		_ = os.Remove(filepath.Join(dir, parent))
	} // Empty directories only.
	for _, f := range files {
		path := filepath.Join(dir, filepath.FromSlash(f.path))
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if f.mode == "120000" && symlinks != "false" {
			err = os.Symlink(string(f.bytes), path)
		} else {
			mode := os.FileMode(0644)
			if f.mode == "100755" {
				mode = 0755
			}
			err = os.WriteFile(path, f.bytes, mode)
		}
		if err != nil {
			return fmt.Errorf("restore %s: %w", f.path, err)
		}
	}
	if err = os.Rename(lockPath, filepath.Join(gitDir, "index")); err != nil {
		return err
	}
	actual, err = PreservePartialWork(ctx, dir, p.RunID, p.Step, p.StopID)
	if err != nil || actual.SHA != p.SHA || actual.IndexSHA != p.IndexSHA {
		return fmt.Errorf("restored state does not match %s; retain %s: %w", p.Ref, dir, err)
	}
	return nil
}

func restorePath(name string) error {
	if !filepath.IsLocal(name) || strings.Contains(name, "\\") {
		return fmt.Errorf("unsafe rescue path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return fmt.Errorf("unsafe rescue path %q", name)
		}
	}
	return nil
}
