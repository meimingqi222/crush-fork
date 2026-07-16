//go:build !windows

package clientfs

import "path/filepath"

func resolveExistingPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
