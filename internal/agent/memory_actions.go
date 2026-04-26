package agent

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/charmbracelet/crush/internal/memory"
)

type memoryOperation string

const (
	memoryOperationStore  memoryOperation = "store"
	memoryOperationUpdate memoryOperation = "update"
	memoryOperationDelete memoryOperation = "delete"
	memoryOperationNoop   memoryOperation = "noop"
)

type extractedMemory struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
	Content     string `json:"content,omitempty"`
	Type        string `json:"type,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Action      string `json:"action,omitempty"`
}

func normalizeMemoryAction(action string) memoryOperation {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", string(memoryOperationStore):
		return memoryOperationStore
	case string(memoryOperationUpdate):
		return memoryOperationUpdate
	case string(memoryOperationDelete), "prune", "remove":
		return memoryOperationDelete
	case string(memoryOperationNoop), "skip":
		return memoryOperationNoop
	default:
		return memoryOperationStore
	}
}

func sanitizeExtractedMemories(memories []extractedMemory) []extractedMemory {
	result := make([]extractedMemory, 0, len(memories))
	for _, mem := range memories {
		mem.Key = strings.TrimSpace(mem.Key)
		mem.Description = strings.TrimSpace(mem.Description)
		mem.Content = strings.TrimSpace(mem.Content)
		mem.Scope = strings.TrimSpace(mem.Scope)
		mem.Type = strings.TrimSpace(mem.Type)
		mem.Action = string(normalizeMemoryAction(mem.Action))
		if mem.Key == "" {
			continue
		}
		switch normalizeMemoryAction(mem.Action) {
		case memoryOperationDelete:
			result = append(result, mem)
		case memoryOperationNoop:
			result = append(result, mem)
		default:
			if mem.Content == "" {
				continue
			}
			if mem.Description == "" {
				mem.Description = "Extracted from conversation"
			}
			result = append(result, mem)
		}
	}
	return result
}

func applyExtractedMemories(ctx context.Context, memorySvc memory.Service, memories []extractedMemory, logAttrs ...any) error {
	for _, mem := range sanitizeExtractedMemories(memories) {
		switch normalizeMemoryAction(mem.Action) {
		case memoryOperationNoop:
			continue
		case memoryOperationDelete:
			if err := memorySvc.Delete(ctx, mem.Key); err != nil {
				if err == memory.ErrNotFound {
					slog.Debug("Skipping deletion for missing memory", append(logAttrs, "key", mem.Key)...)
					continue
				}
				return fmt.Errorf("deleting memory %q: %w", mem.Key, err)
			}
			slog.Info("Deleted memory", append(logAttrs, "key", mem.Key)...)
		default:
			params := memory.StoreParams{
				Key:         mem.Key,
				Value:       mem.Content,
				Description: mem.Description,
				Type:        cmp.Or(mem.Type, "general"),
			}
			if mem.Scope != "" {
				params.Scope = mem.Scope
			}
			if err := memorySvc.Store(ctx, params); err != nil {
				return fmt.Errorf("storing memory %q: %w", mem.Key, err)
			}
			slog.Info("Stored memory", append(logAttrs, "key", mem.Key, "action", mem.Action)...)
		}
	}
	return nil
}
