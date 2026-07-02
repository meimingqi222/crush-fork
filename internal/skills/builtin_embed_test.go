package skills

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// skillNames returns the set of skill names in the given slice. It is shared
// by tests that assert on skill presence/absence independent of ordering and
// independent of the builtin skill set.
func skillNames(skills []*Skill) map[string]bool {
	names := make(map[string]bool, len(skills))
	for _, s := range skills {
		names[s.Name] = true
	}
	return names
}

func TestEmbeddedSkills(t *testing.T) {
	t.Parallel()

	skills := EmbeddedSkills()
	require.Len(t, skills, 2)

	byName := make(map[string]*Skill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}

	cr := byName["crush-config"]
	require.NotNil(t, cr)
	require.NotEmpty(t, cr.Description)
	require.NotEmpty(t, cr.Instructions)
	require.Equal(t, "crush://skills/crush-config/SKILL.md", cr.SkillFilePath)

	jq := byName["jq"]
	require.NotNil(t, jq)
	require.NotEmpty(t, jq.Description)
	require.NotEmpty(t, jq.Instructions)
	require.Equal(t, "crush://skills/jq/SKILL.md", jq.SkillFilePath)
}

func TestDiscoverIncludesBuiltinsOnEmptyPaths(t *testing.T) {
	t.Parallel()

	skills := Discover(nil)
	names := skillNames(skills)
	require.True(t, names["crush-config"])
	require.True(t, names["jq"])

	skills = Discover([]string{})
	names = skillNames(skills)
	require.True(t, names["crush-config"])
	require.True(t, names["jq"])
}

func TestDiscoverUserSkillOverridesBuiltin(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create a user skill named crush-config that shadows the builtin.
	writeSkillFile(t, dir, "crush-config", "User-defined crush-config override.")

	skills := Discover([]string{dir})
	// One user skill replaces one builtin, so the total equals the builtin
	// count (user crush-config + builtin jq).
	require.Len(t, skills, len(EmbeddedSkills()))

	names := skillNames(skills)
	require.True(t, names["crush-config"])
	require.True(t, names["jq"])

	// The crush-config entry must be the user's on-disk skill, not the
	// builtin virtual one.
	var user *Skill
	for _, s := range skills {
		if s.Name == "crush-config" {
			user = s
			break
		}
	}
	require.NotNil(t, user)
	require.NotEqual(t, "crush://skills/crush-config/SKILL.md", user.SkillFilePath)

	// The builtin crush-config must be absent (overridden).
	for _, s := range skills {
		require.NotEqual(t, "crush://skills/crush-config/SKILL.md", s.SkillFilePath)
	}
}
