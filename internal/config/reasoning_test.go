package config

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestReasoningLevelsFromOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		options  []ModelsDevReasoningOption
		expected []string
	}{
		{
			name:     "effort options return values",
			options:  []ModelsDevReasoningOption{{Type: "effort", Values: []string{"high", "max"}}},
			expected: []string{"high", "max"},
		},
		{
			name:     "toggle returns nil",
			options:  []ModelsDevReasoningOption{{Type: "toggle"}},
			expected: nil,
		},
		{
			name:     "budget_tokens returns nil",
			options:  []ModelsDevReasoningOption{{Type: "budget_tokens", Min: ptr(int64(0)), Max: ptr(int64(1000))}},
			expected: nil,
		},
		{
			name:     "empty options returns nil",
			options:  []ModelsDevReasoningOption{},
			expected: nil,
		},
		{
			name:     "empty effort values returns nil",
			options:  []ModelsDevReasoningOption{{Type: "effort", Values: []string{}}},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := reasoningLevelsFromOptions(tt.options)
			require.Equal(t, tt.expected, result)
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestResolveReasoningLevels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		model    catwalk.Model
		expected []string
	}{
		{
			name:     "non-reasoning model keeps empty levels",
			model:    catwalk.Model{ID: "gpt-4o", CanReason: false},
			expected: nil,
		},
		{
			name:     "explicit config levels are preserved",
			model:    catwalk.Model{ID: "custom", CanReason: true, ReasoningLevels: []string{"low", "medium"}},
			expected: []string{"low", "medium"},
		},
		{
			name:     "claude 4.6+ infers adaptive thinking levels",
			model:    catwalk.Model{ID: "claude-sonnet-4.6", CanReason: true},
			expected: []string{"low", "medium", "high"},
		},
		{
			name:     "provider-prefixed claude 4.6+ infers adaptive thinking levels",
			model:    catwalk.Model{ID: "anthropic/claude-sonnet-4.7", CanReason: true},
			expected: []string{"low", "medium", "high"},
		},
		{
			name:     "older claude thinking model has no selectable levels",
			model:    catwalk.Model{ID: "claude-sonnet-4", CanReason: true},
			expected: nil,
		},
		{
			name:     "openai o3 infers reasoning levels",
			model:    catwalk.Model{ID: "o3-mini", CanReason: true},
			expected: []string{"low", "medium", "high"},
		},
		{
			name:     "gpt-5 infers reasoning levels",
			model:    catwalk.Model{ID: "gpt-5", CanReason: true},
			expected: []string{"low", "medium", "high"},
		},
		{
			name:     "gemini infers reasoning levels",
			model:    catwalk.Model{ID: "gemini-2.5-pro", CanReason: true},
			expected: []string{"low", "medium", "high"},
		},
		{
			name:     "glm-5.2 uses native high/max levels",
			model:    catwalk.Model{ID: "glm-5.2", CanReason: true},
			expected: []string{"high", "max"},
		},
		{
			name:     "unknown reasoning model defaults to generic levels",
			model:    catwalk.Model{ID: "deepseek-r1", CanReason: true},
			expected: []string{"low", "medium", "high"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ResolveReasoningLevels(&tt.model)
			require.Equal(t, tt.expected, tt.model.ReasoningLevels)
		})
	}
}
