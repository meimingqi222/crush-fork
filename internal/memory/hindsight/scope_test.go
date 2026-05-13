package hindsight

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveScope(t *testing.T) {
	t.Parallel()

	require.Equal(t, Scope{BankID: "team"}, ResolveScope("team", "global", "proj-abc123"))
	require.Equal(t, Scope{BankID: "crush"}, ResolveScope("", "global", "proj-abc123"))
	require.Equal(t, Scope{BankID: "team-proj-abc123"}, ResolveScope("team", "per-project", "proj-abc123"))
	require.Equal(t, Scope{
		BankID:          "team",
		RetainTags:      []string{"project:proj-abc123"},
		RecallTags:      []string{"project:proj-abc123"},
		RecallTagsMatch: "any",
	}, ResolveScope("team", "per-project-tagged", "proj-abc123"))
}
