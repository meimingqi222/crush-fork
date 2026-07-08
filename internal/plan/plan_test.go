package plan

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSlugFromPlanPath(t *testing.T) {
	t.Parallel()

	const sessionID = "sess-123"
	workspaceRoot := t.TempDir()
	plansDir := PlansDir(workspaceRoot)

	tests := []struct {
		name      string
		path      string
		wantSlug  string
		wantMatch bool
	}{
		{
			name:      "custom slug",
			path:      filepath.Join(plansDir, sessionID+"-auth-refactor.md"),
			wantSlug:  "auth-refactor",
			wantMatch: true,
		},
		{
			name:      "default slug",
			path:      filepath.Join(plansDir, sessionID+"-plan.md"),
			wantSlug:  "plan",
			wantMatch: true,
		},
		{
			name:      "multi-segment slug",
			path:      filepath.Join(plansDir, sessionID+"-refactor-auth-flow.md"),
			wantSlug:  "refactor-auth-flow",
			wantMatch: true,
		},
		{
			name:      "path outside plans directory",
			path:      filepath.Join(workspaceRoot, "auth-refactor.md"),
			wantMatch: false,
		},
		{
			name:      "path belonging to a different session",
			path:      filepath.Join(plansDir, "other-session-auth-refactor.md"),
			wantMatch: false,
		},
		{
			name:      "non-md file inside plans directory",
			path:      filepath.Join(plansDir, sessionID+"-auth-refactor.txt"),
			wantMatch: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			slug, ok := SlugFromPlanPath(workspaceRoot, sessionID, tc.path)
			require.Equal(t, tc.wantMatch, ok, "match mismatch for %q", tc.path)
			if tc.wantMatch {
				require.Equal(t, tc.wantSlug, slug, "slug mismatch for %q", tc.path)
			}
		})
	}
}

func TestSlugFromPlanPathRelativePath(t *testing.T) {
	const sessionID = "sess-123"
	workspaceRoot := t.TempDir()
	require.NoError(t, EnsureDir(workspaceRoot))
	plansDir := PlansDir(workspaceRoot)
	customPath := filepath.Join(plansDir, sessionID+"-auth-refactor.md")

	// A relative path should be resolved against the working directory by
	// filepath.Abs; when cwd is the plans directory itself, the relative
	// filename should still match.
	rel := filepath.Base(customPath)
	t.Chdir(plansDir)

	slug, ok := SlugFromPlanPath(workspaceRoot, sessionID, rel)
	require.True(t, ok)
	require.Equal(t, "auth-refactor", slug)
}

func TestSanitizeSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"auth-refactor", "auth-refactor"},
		{"Auth Refactor", "Auth-Refactor"},
		{"", "plan"},
		{"---", "plan"},
		{"a__b", "a__b"},
		{"a/b", "ab"},
		{"café", "caf"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, sanitizeSlug(tc.in))
		})
	}
}

func TestPlanFilePathRoundTrip(t *testing.T) {
	t.Parallel()

	const sessionID = "sess-123"
	workspaceRoot := t.TempDir()

	// PlanFilePath -> SlugFromPlanPath should round-trip for custom and
	// default slugs.
	for _, slug := range []string{"auth-refactor", "plan", ""} {
		path := PlanFilePath(workspaceRoot, sessionID, slug)
		got, ok := SlugFromPlanPath(workspaceRoot, sessionID, path)
		require.True(t, ok, "SlugFromPlanPath should match PlanFilePath output for slug %q", slug)
		// Empty slug sanitizes to "plan".
		want := sanitizeSlug(slug)
		require.Equal(t, want, got)
	}
}
