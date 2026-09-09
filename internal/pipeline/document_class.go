package pipeline

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// DefaultDocumentCorrectionPaths is the fail-safe documentation-and-records
// path class: the files a document-step correction may edit, and the files a
// post-review head advance may contain without sending the run back through
// Review.
//
// It is deliberately a PATH class, not an effect judgement. A `.md` file can
// still influence executable behaviour, generated output, or delivery
// authority - contracts/ and openspec/ exist precisely because it does - so
// membership here never certifies "harmless". It answers only the narrower
// question "is this the documentation and records surface the Document gate
// owns". The effect question is answered separately by the finding's class
// (types.FindingClassBehavioural), and a correction inside this class still
// re-runs the project's own test command and the full CI battery at the pushed
// head before anything is claimed about it.
var DefaultDocumentCorrectionPaths = []string{"*.md", "docs/**", "openspec/**", "contracts/**", "README*"}

// MatchPathGlob reports whether file matches one repository path pattern.
//
// The three rules are the ones ignore_patterns has always used, and this is
// their single implementation: a trailing "/**" is a literal subtree prefix
// rather than a glob, a pattern with no "/" matches the basename only, and
// anything else is a whole-path path.Match. It must stay path.Match rather
// than filepath.Match: filepath.Match is separator-dependent, so on Windows a
// "\" would read as a path separator instead of an escape and a pattern would
// match differently from the way config.validatePathInstructionGlob validated
// it.
func MatchPathGlob(file, pattern string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		return file == prefix || strings.HasPrefix(file, prefix+"/")
	}
	if !strings.Contains(pattern, "/") {
		matched, _ := path.Match(pattern, path.Base(file))
		return matched
	}
	matched, _ := path.Match(pattern, file)
	return matched
}

// DocumentCorrectionPaths resolves the documentation-and-records path class
// for a run. The repository may narrow or widen it through
// document.correction_paths, which - like the rest of the document block - is
// read only from the trusted default-branch copy of .no-mistakes.yaml, so a
// pushed branch cannot widen the class that decides what its own corrections
// may touch and what may skip Review.
func DocumentCorrectionPaths(cfg *config.Config) []string {
	if cfg != nil && len(cfg.Document.CorrectionPaths) > 0 {
		return cfg.Document.CorrectionPaths
	}
	return DefaultDocumentCorrectionPaths
}

// IsDocumentClassPath reports whether one repository-relative path is inside
// the documentation-and-records class.
func IsDocumentClassPath(file string, patterns []string) bool {
	file = strings.TrimSpace(file)
	if file == "" {
		return false
	}
	for _, pattern := range patterns {
		if MatchPathGlob(file, pattern) {
			return true
		}
	}
	return false
}

// OutsideDocumentClass returns the paths that are NOT in the
// documentation-and-records class, de-duplicated and sorted so an error
// message names them in a stable order. An empty result means every path given
// is inside the class; callers must treat an EMPTY INPUT as "nothing proven"
// rather than "all clear", which is why the diff callers check for an empty
// path list separately.
func OutsideDocumentClass(files []string, patterns []string) []string {
	seen := map[string]bool{}
	var outside []string
	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" || seen[file] {
			continue
		}
		seen[file] = true
		if !IsDocumentClassPath(file, patterns) {
			outside = append(outside, file)
		}
	}
	sort.Strings(outside)
	return outside
}

// DocumentClassAdvance answers whether the commits between fromHead and toHead
// changed documentation-and-records files and nothing else.
//
// It fails closed in every direction that is not a proven documentation-only
// advance: a git error, an unreadable diff, and - importantly - an EMPTY diff
// all return false. An empty diff between two different commits means the
// advance changed no file content at all (an empty commit, or a revert pair),
// which is not evidence that Review has already read what is being published;
// the ordinary Review restart handles it.
func DocumentClassAdvance(gitRun GitRunner, fromHead, toHead string, patterns []string) (bool, []string, error) {
	changed, err := gitRun("diff", "--name-only", "-z", fromHead+".."+toHead)
	if err != nil {
		return false, nil, fmt.Errorf("read the diff between review-approved head %s and %s: %w", ShortObjectID(fromHead), ShortObjectID(toHead), err)
	}
	files := SplitNULPaths(changed)
	if len(files) == 0 {
		return false, nil, nil
	}
	if outside := OutsideDocumentClass(files, patterns); len(outside) > 0 {
		return false, files, nil
	}
	return true, files, nil
}

// SplitNULPaths splits a NUL-delimited git path payload, preserving raw path
// bytes and git's own order. `-z` is used rather than the newline format
// because git's default core.quotepath escapes a path containing non-ASCII
// bytes, backslashes, or double quotes into a C-style quoted string, and a
// class check run against that quoted spelling would compare the wrong name.
func SplitNULPaths(payload string) []string {
	var files []string
	for _, file := range strings.Split(strings.TrimSuffix(payload, "\x00"), "\x00") {
		if strings.TrimSpace(file) == "" {
			continue
		}
		files = append(files, file)
	}
	return files
}
