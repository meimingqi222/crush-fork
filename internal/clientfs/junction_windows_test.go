//go:build windows

package clientfs

import (
	"fmt"
	"os/exec"
)

func createJunction(link, target string) error {
	output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink: %w: %s", err, output)
	}
	return nil
}
