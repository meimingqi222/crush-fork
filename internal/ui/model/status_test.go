package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLayoutStatusMessageKeepsWarningTextForLongErrors(t *testing.T) {
	t.Parallel()

	msg := layoutStatusMessage("WARNING", strings.Repeat("rate limit exceeded ", 8), 120)
	require.NotEmpty(t, strings.TrimSpace(msg))
	require.Contains(t, msg, "rate limit")
}
