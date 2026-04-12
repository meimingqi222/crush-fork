package agent

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// memoryExtractionThrottleTurns controls how many assistant turns pass before
// Extract is called again for the same session.
const memoryExtractionThrottleTurns = 1

var memoryStoreActionPattern = regexp.MustCompile(`(?i)(^|[^a-z0-9_])["']?action["']?\s*[:=]\s*["']?store["']?([^a-z0-9_]|$)`)

func shouldExtractMemories(turnsSinceLastExtraction int) bool {
	return turnsSinceLastExtraction >= memoryExtractionThrottleTurns
}

func isMemoryStoreToolCallLine(line string) bool {
	if line == "" {
		return false
	}
	if !strings.Contains(strings.ToLower(line), "long_term_memory") {
		return false
	}
	return memoryStoreActionPattern.MatchString(line)
}

func hasMemoryWritesInHistory(history []string) bool {
	for _, h := range history {
		if isMemoryStoreToolCallLine(h) {
			return true
		}
	}
	return false
}

func drainPendingExtractions(pendingExtractions *map[string][]context.CancelFunc, timeout time.Duration) {
	allCancels := make([]context.CancelFunc, 0)
	for _, cancels := range *pendingExtractions {
		allCancels = append(allCancels, cancels...)
	}
	if len(allCancels) == 0 {
		return
	}
	time.AfterFunc(timeout, func() {
		for _, cancel := range allCancels {
			cancel()
		}
	})
}
