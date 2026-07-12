package skills

import (
	_ "embed"
	"log/slog"
	"os"
)

//go:embed builtin/crush-config/SKILL.md
var crushConfigSkill []byte

//go:embed builtin/jq/SKILL.md
var jqSkill []byte

// embeddedSkillEntries pairs a virtual skill path with its embedded content.
// The virtual path uses the crush:// scheme so builtin skills can be
// distinguished from filesystem-backed skills. Builtin skills have the
// lowest priority: a user skill with the same name overrides the builtin
// in Discover.
var embeddedSkillEntries = []struct {
	path    string
	content []byte
}{
	{path: "crush://skills/crush-config/SKILL.md", content: crushConfigSkill},
	{path: "crush://skills/jq/SKILL.md", content: jqSkill},
}

// ReadSkillFile reads a skill file. If path is a builtin virtual path
// (crush://...), it returns the embedded content; otherwise it reads from disk.
func ReadSkillFile(path string) ([]byte, error) {
	for _, e := range embeddedSkillEntries {
		if e.path == path {
			return e.content, nil
		}
	}
	return os.ReadFile(path)
}

// EmbeddedSkills parses the builtin SKILL.md files embedded into the binary
// and returns them as Skill values. Builtin skills are always available
// without user configuration.
func EmbeddedSkills() []*Skill {
	skills := make([]*Skill, 0, len(embeddedSkillEntries))
	for _, e := range embeddedSkillEntries {
		skill, err := parseContent(e.path, e.content)
		if err != nil {
			slog.Warn("Failed to parse builtin skill", "path", e.path, "error", err)
			continue
		}
		skills = append(skills, skill)
	}
	return skills
}
