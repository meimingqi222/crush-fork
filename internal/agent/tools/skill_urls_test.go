package tools

import (
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/skills"
	"github.com/stretchr/testify/require"
)

func testSkillList(t *testing.T) []*skills.Skill {
	t.Helper()
	skillsDir := t.TempDir()
	return []*skills.Skill{
		{
			Name:          "pdf",
			Description:   "PDF generation and manipulation",
			Path:          filepath.Join(skillsDir, "pdf"),
			SkillFilePath: filepath.Join(skillsDir, "pdf", "SKILL.md"),
		},
		{
			Name:          "postgres",
			Description:   "PostgreSQL database operations",
			Path:          filepath.Join(skillsDir, "postgres"),
			SkillFilePath: filepath.Join(skillsDir, "postgres", "SKILL.md"),
		},
	}
}

func TestResolveSkillURL(t *testing.T) {
	t.Parallel()

	sl := testSkillList(t)

	tests := []struct {
		name        string
		rawURL      string
		wantContain string
		wantErr     bool
	}{
		{
			name:        "skill root resolves to SKILL.md",
			rawURL:      "skill://pdf",
			wantContain: filepath.Join("pdf", "SKILL.md"),
		},
		{
			name:        "skill with relative path",
			rawURL:      "skill://pdf/scripts/convert.py",
			wantContain: filepath.Join("pdf", "scripts", "convert.py"),
		},
		{
			name:    "unknown skill name",
			rawURL:  "skill://unknown",
			wantErr: true,
		},
		{
			name:    "empty skill name",
			rawURL:  "skill://",
			wantErr: true,
		},
		{
			name:    "path traversal rejected",
			rawURL:  "skill://pdf/../../etc/passwd",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveSkillURL(tt.rawURL, sl)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Contains(t, got, tt.wantContain)
		})
	}
}

func TestExpandSkillURLs(t *testing.T) {
	t.Parallel()

	sl := testSkillList(t)

	t.Run("no skill URLs", func(t *testing.T) {
		t.Parallel()
		got := ExpandSkillURLs("echo hello", sl)
		require.Equal(t, "echo hello", got)
	})

	t.Run("single skill URL", func(t *testing.T) {
		t.Parallel()
		got := ExpandSkillURLs("python skill://pdf/scripts/run.py", sl)
		require.Contains(t, got, filepath.Join("pdf", "scripts", "run.py"))
		require.Contains(t, got, "python '")
	})

	t.Run("skill URL at end of command", func(t *testing.T) {
		t.Parallel()
		got := ExpandSkillURLs("cat skill://postgres/SKILL.md", sl)
		require.Contains(t, got, filepath.Join("postgres", "SKILL.md"))
		require.Contains(t, got, "cat '")
	})

	t.Run("empty skill list returns unchanged", func(t *testing.T) {
		t.Parallel()
		got := ExpandSkillURLs("python skill://pdf/scripts/run.py", nil)
		require.Equal(t, "python skill://pdf/scripts/run.py", got)
	})
}

func TestIsSkillURL(t *testing.T) {
	t.Parallel()

	require.True(t, IsSkillURL("skill://pdf"))
	require.True(t, IsSkillURL("skill://pdf/scripts/run.py"))
	require.False(t, IsSkillURL("http://example.com"))
	require.False(t, IsSkillURL("/home/user/skills/pdf"))
}
