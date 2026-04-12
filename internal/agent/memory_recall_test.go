package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHasMemoryWritesInHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		history []string
		want    bool
	}{
		{
			name: "detects action equals store format",
			history: []string{
				`assistant tool call: long_term_memory action=store key=user/preference`,
			},
			want: true,
		},
		{
			name: "detects json action store format",
			history: []string{
				`{"tool":"long_term_memory","arguments":{"action":"store","key":"project/context"}}`,
			},
			want: true,
		},
		{
			name: "case insensitive detection",
			history: []string{
				`ASSISTANT TOOL CALL: LONG_TERM_MEMORY ACTION=STORE`,
			},
			want: true,
		},
		{
			name: "does not match plain text",
			history: []string{
				`please store this in memory`,
			},
			want: false,
		},
		{
			name: "does not match non-store action",
			history: []string{
				`{"tool":"long_term_memory","arguments":{"action":"search","query":"prefs"}}`,
			},
			want: false,
		},
		{
			name:    "does not match empty history",
			history: []string{},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hasMemoryWritesInHistory(tt.history)
			require.Equal(t, tt.want, got)
		})
	}
}
