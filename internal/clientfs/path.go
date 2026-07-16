package clientfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

const maxPathBytes = 32 * 1024

// ResolvePath lexically and physically confines requested to workspace. For a
// new path, the nearest existing ancestor is resolved so a symlink or Windows
// junction cannot redirect creation outside the workspace.
func ResolvePath(workspace, requested string) (string, error) {
	if workspace == "" || requested == "" || len(requested) > maxPathBytes ||
		!utf8.ValidString(requested) || strings.ContainsRune(requested, '\x00') {
		return "", ErrInvalidPath
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", ErrInvalidPath
	}
	root = filepath.Clean(root)
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return "", ErrInvalidPath
	}

	target := requested
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", ErrInvalidPath
	}
	target = filepath.Clean(target)
	if !pathWithin(root, target) {
		return "", ErrPathEscape
	}

	resolvedRoot, err := resolveExistingPath(root)
	if err != nil {
		return "", ErrInvalidPath
	}
	ancestor := target
	for {
		_, statErr := os.Lstat(ancestor)
		if statErr == nil {
			break
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return "", ErrInvalidPath
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor || !pathWithin(root, parent) {
			return "", ErrPathEscape
		}
		ancestor = parent
	}
	resolvedAncestor, err := resolveExistingPath(ancestor)
	if err != nil {
		return "", ErrInvalidPath
	}
	if !pathWithin(resolvedRoot, resolvedAncestor) {
		return "", ErrPathEscape
	}
	return target, nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if runtime.GOOS == "windows" {
		// filepath.Rel is normally case-insensitive on Windows, but folding the
		// final comparison also covers differently cased drive/root spellings.
		root = strings.ToLower(filepath.Clean(root))
		target = strings.ToLower(filepath.Clean(target))
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return false
		}
	}
	return true
}
