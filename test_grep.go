package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/charmbracelet/crush/internal/agent/tools"
)

func main() {
	tempDir, _ := os.MkdirTemp("", "grep_test")
	defer os.RemoveAll(tempDir)

	os.WriteFile(filepath.Join(tempDir, "sample.go"), []byte("c.agents[config.AgentCoder] = agent\n"), 0o644)

	result, err := tools.RunGrepSearchForTest(context.Background(), tools.GrepParams{Pattern: "c\\.agents["}, tempDir, 100)
	fmt.Printf("Error: %v\n", err)
	fmt.Printf("Matches: %d\n", len(result.Matches))
	fmt.Printf("Metadata.LiteralText: %v\n", result.Metadata.LiteralText)
	fmt.Printf("Metadata.RecoveredBy: %s\n", result.Metadata.RecoveredBy)
}
