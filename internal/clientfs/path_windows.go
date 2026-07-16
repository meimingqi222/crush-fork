//go:build windows

package clientfs

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func resolveExistingPath(path string) (string, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(
		pointer, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0,
	)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, 512)
	for {
		length, finalErr := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if finalErr != nil {
			return "", finalErr
		}
		if length < uint32(len(buffer)) {
			resolved := windows.UTF16ToString(buffer[:length])
			resolved = strings.TrimPrefix(resolved, `\\?\`)
			if strings.HasPrefix(resolved, `UNC\`) {
				resolved = `\\` + strings.TrimPrefix(resolved, `UNC\`)
			}
			return filepath.Clean(resolved), nil
		}
		buffer = make([]uint16, int(length)+1)
	}
}
