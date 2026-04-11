//go:build ignore

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	checks := []struct {
		label string
		args  []string
	}{
		{
			label: "message metadata regression tests",
			args: []string{
				"test",
				"./internal/message",
				"-run",
				"TestFromFantasyMessages_(RoundTripPreservesMetadata|PreservesIDsAfterMessageProviderOptionsCleared)$",
				"-count=1",
			},
		},
	}

	for _, check := range checks {
		fmt.Printf("==> Running %s\n", check.label)
		cmd := exec.Command("go", check.args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Verification failed during %s: %v\n", check.label, err)
			os.Exit(1)
		}
	}

	fmt.Println("Morph compact regression checks passed.")
}
