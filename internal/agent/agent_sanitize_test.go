package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeToolInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantOut   string
		wantClean bool
	}{
		{
			name:      "valid JSON passes through",
			input:     `{"path": "foo.txt", "content": "hello"}`,
			wantOut:   `{"path": "foo.txt", "content": "hello"}`,
			wantClean: false,
		},
		{
			name:      "empty object is valid",
			input:     `{}`,
			wantOut:   `{}`,
			wantClean: false,
		},
		{
			name:      "truncated JSON is replaced",
			input:     `{"path": "foo.txt", "content": "unfinished`,
			wantOut:   `{}`,
			wantClean: true,
		},
		{
			name:      "trailing garbage is replaced",
			input:     `{"path": "x"} trailing`,
			wantOut:   `{}`,
			wantClean: true,
		},
		{
			name:      "empty input is replaced",
			input:     ``,
			wantOut:   `{}`,
			wantClean: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, sanitized := sanitizeToolInput("bash", "call-1", tt.input)
			require.Equal(t, tt.wantOut, out)
			require.Equal(t, tt.wantClean, sanitized)
		})
	}
}
