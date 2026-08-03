package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		content          string
		wantName         string
		wantDesc         string
		wantLicense      string
		wantCompat       string
		wantMeta         map[string]string
		wantWhenToUse    string
		wantAllowedTools []string
		wantArguments    []string
		wantArgHint      string
		wantModel        string
		wantContext      SkillContext
		wantInstr        string
		wantErr          bool
	}{
		{
			name: "full skill",
			content: `---
name: pdf-processing
description: Extracts text and tables from PDF files, fills PDF forms, and merges multiple PDFs.
license: Apache-2.0
compatibility: Requires python 3.8+, pdfplumber, pdfrw libraries
metadata:
  author: example-org
  version: "1.0"
---

# PDF Processing

## When to use this skill
Use this skill when the user needs to work with PDF files.
`,
			wantName:    "pdf-processing",
			wantDesc:    "Extracts text and tables from PDF files, fills PDF forms, and merges multiple PDFs.",
			wantLicense: "Apache-2.0",
			wantCompat:  "Requires python 3.8+, pdfplumber, pdfrw libraries",
			wantMeta:    map[string]string{"author": "example-org", "version": "1.0"},
			wantInstr:   "# PDF Processing\n\n## When to use this skill\nUse this skill when the user needs to work with PDF files.",
		},
		{
			name: "skill with extended fields",
			content: `---
name: cherry-pick-pr
description: Cherry-picks a PR to a release branch
when_to_use: "Use when the user wants to cherry-pick a PR to a release branch. Examples: cherry-pick to release, CP this PR, hotfix."
allowed-tools:
  - Bash(gh:*)
  - Read
  - Edit
arguments:
  - pr_number
  - target_branch
argument-hint: "[pr_number] [target_branch]"
model: opus
context: fork
---

# Cherry-pick PR

## Inputs
- $pr_number: The PR number to cherry-pick
- $target_branch: The target branch

## Steps
1. Fetch the PR
2. Cherry-pick to target branch
`,
			wantName:         "cherry-pick-pr",
			wantDesc:         "Cherry-picks a PR to a release branch",
			wantWhenToUse:    "Use when the user wants to cherry-pick a PR to a release branch. Examples: cherry-pick to release, CP this PR, hotfix.",
			wantAllowedTools: []string{"Bash(gh:*)", "Read", "Edit"},
			wantArguments:    []string{"pr_number", "target_branch"},
			wantArgHint:      "[pr_number] [target_branch]",
			wantModel:        "opus",
			wantContext:      SkillContextFork,
			wantInstr:        "# Cherry-pick PR\n\n## Inputs\n- $pr_number: The PR number to cherry-pick\n- $target_branch: The target branch\n\n## Steps\n1. Fetch the PR\n2. Cherry-pick to target branch",
		},
		{
			name: "minimal skill",
			content: `---
name: my-skill
description: A simple skill for testing.
---

# My Skill

Instructions here.
`,
			wantName:  "my-skill",
			wantDesc:  "A simple skill for testing.",
			wantInstr: "# My Skill\n\nInstructions here.",
		},
		{
			name:    "no frontmatter",
			content: "# Just Markdown\n\nNo frontmatter here.",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Write content to temp file.
			dir := t.TempDir()
			path := filepath.Join(dir, "SKILL.md")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o644))

			skill, err := Parse(path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			require.Equal(t, tt.wantName, skill.Name)
			require.Equal(t, tt.wantDesc, skill.Description)
			require.Equal(t, tt.wantLicense, skill.License)
			require.Equal(t, tt.wantCompat, skill.Compatibility)

			if tt.wantMeta != nil {
				require.Equal(t, tt.wantMeta, skill.Metadata)
			}

			if tt.wantWhenToUse != "" {
				require.Equal(t, tt.wantWhenToUse, skill.WhenToUse)
			}

			if tt.wantAllowedTools != nil {
				require.Equal(t, tt.wantAllowedTools, skill.AllowedTools)
			}

			if tt.wantArguments != nil {
				require.Equal(t, tt.wantArguments, skill.Arguments)
			}

			if tt.wantArgHint != "" {
				require.Equal(t, tt.wantArgHint, skill.ArgumentHint)
			}

			if tt.wantModel != "" {
				require.Equal(t, tt.wantModel, skill.Model)
			}

			if tt.wantContext != "" {
				require.Equal(t, tt.wantContext, skill.Context)
			}

			require.Equal(t, tt.wantInstr, skill.Instructions)
		})
	}
}

func TestSkillValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		skill   Skill
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid skill",
			skill: Skill{
				Name:        "pdf-processing",
				Description: "Processes PDF files.",
				Path:        "/skills/pdf-processing",
			},
		},
		{
			name:    "missing name",
			skill:   Skill{Description: "Some description."},
			wantErr: true,
			errMsg:  "name is required",
		},
		{
			name:    "missing description",
			skill:   Skill{Name: "my-skill", Path: "/skills/my-skill"},
			wantErr: true,
			errMsg:  "description is required",
		},
		{
			name:    "name too long",
			skill:   Skill{Name: strings.Repeat("a", 65), Description: "Some description."},
			wantErr: true,
			errMsg:  "exceeds",
		},
		{
			name:    "valid name - mixed case",
			skill:   Skill{Name: "MySkill", Description: "Some description.", Path: "/skills/MySkill"},
			wantErr: false,
		},
		{
			name:    "invalid name - starts with hyphen",
			skill:   Skill{Name: "-my-skill", Description: "Some description."},
			wantErr: true,
			errMsg:  "alphanumeric with hyphens",
		},
		{
			name:    "name doesn't match directory",
			skill:   Skill{Name: "my-skill", Description: "Some description.", Path: "/skills/other-skill"},
			wantErr: true,
			errMsg:  "must match directory",
		},
		{
			name:    "description too long",
			skill:   Skill{Name: "my-skill", Description: strings.Repeat("a", 1025), Path: "/skills/my-skill"},
			wantErr: true,
			errMsg:  "description exceeds",
		},
		{
			name:    "compatibility too long",
			skill:   Skill{Name: "my-skill", Description: "desc", Compatibility: strings.Repeat("a", 501), Path: "/skills/my-skill"},
			wantErr: true,
			errMsg:  "compatibility exceeds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.skill.Validate()
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create valid skill 1.
	skill1Dir := filepath.Join(tmpDir, "skill-one")
	require.NoError(t, os.MkdirAll(skill1Dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skill1Dir, "SKILL.md"), []byte(`---
name: skill-one
description: First test skill.
---
# Skill One
`), 0o644))

	// Create valid skill 2 in nested directory.
	skill2Dir := filepath.Join(tmpDir, "nested", "skill-two")
	require.NoError(t, os.MkdirAll(skill2Dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skill2Dir, "SKILL.md"), []byte(`---
name: skill-two
description: Second test skill.
---
# Skill Two
`), 0o644))

	// Create invalid skill (won't be included).
	invalidDir := filepath.Join(tmpDir, "invalid-dir")
	require.NoError(t, os.MkdirAll(invalidDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(invalidDir, "SKILL.md"), []byte(`---
name: wrong-name
description: Name doesn't match directory.
---
`), 0o644))

	skills := Discover([]string{tmpDir})
	// Two user skills plus the builtin skills embedded in the binary.
	require.Len(t, skills, 2+len(EmbeddedSkills()))

	names := make(map[string]bool)
	for _, s := range skills {
		names[s.Name] = true
	}
	require.True(t, names["skill-one"])
	require.True(t, names["skill-two"])
	require.True(t, names["crush-config"])
	require.True(t, names["jq"])
}

func TestToPromptXML(t *testing.T) {
	t.Parallel()

	skills := []*Skill{
		{Name: "pdf-processing", Description: "Extracts text from PDFs.", SkillFilePath: "/skills/pdf-processing/SKILL.md"},
		{Name: "data-analysis", Description: "Analyzes datasets & charts.", SkillFilePath: "/skills/data-analysis/SKILL.md"},
	}

	xml := ToPromptXML(skills, 100_000)

	require.Contains(t, xml, "<available_skills>")
	require.Contains(t, xml, "<name>pdf-processing</name>")
	require.Contains(t, xml, "<description>Extracts text from PDFs.</description>")
	require.Contains(t, xml, "&amp;") // XML escaping
}

func TestToPromptXMLEmpty(t *testing.T) {
	t.Parallel()
	require.Empty(t, ToPromptXML(nil, 100_000))
	require.Empty(t, ToPromptXML([]*Skill{}, 100_000))
}

func TestToPromptXMLExtended(t *testing.T) {
	t.Parallel()

	skills := []*Skill{
		{
			Name:         "cherry-pick-pr",
			Description:  "Cherry-picks a PR to a release branch",
			WhenToUse:    "Use when the user wants to cherry-pick a PR. Examples: 'CP this PR'.",
			AllowedTools: []string{"Bash(gh:*)", "Read", "Edit"},
			Arguments:    []string{"pr_number", "target_branch"},
			ArgumentHint: "[pr_number] [target_branch]",
			Model:        "opus",
			Context:      SkillContextFork,
		},
	}

	xml := ToPromptXML(skills, 100_000)

	require.Contains(t, xml, "<name>cherry-pick-pr</name>")
	require.Contains(t, xml, "<when_to_use>")
	require.Contains(t, xml, "<allowed_tools>")
	require.Contains(t, xml, "<tool>Bash(gh:*)</tool>")
	require.Contains(t, xml, "<arguments>")
	require.Contains(t, xml, "<arg>pr_number</arg>")
	require.Contains(t, xml, "<argument_hint>")
	require.Contains(t, xml, "<model>opus</model>")
	require.Contains(t, xml, "<context>fork</context>")
}

func TestToPromptXMLBudgetTruncates(t *testing.T) {
	t.Parallel()

	skills := []*Skill{
		{Name: "one", Description: "First skill with a fairly long description to consume budget.", SkillFilePath: "/s1/SKILL.md"},
		{Name: "two", Description: "Second skill that should be omitted under a tiny budget.", SkillFilePath: "/s2/SKILL.md"},
	}

	// A budget that fits the first entry but not both.
	xml := ToPromptXML(skills, 80)

	require.Contains(t, xml, "<name>one</name>")
	require.NotContains(t, xml, "<name>two</name>")
	require.Contains(t, xml, "omitted")
	require.Contains(t, xml, "two")
}

func TestSubstituteArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		content  string
		args     string
		argNames []string
		want     string
	}{
		{
			name:    "full arguments",
			content: "Hello $ARGUMENTS",
			args:    "world",
			want:    "Hello world",
		},
		{
			name:    "indexed arguments",
			content: "First: $ARGUMENTS[0], Second: $ARGUMENTS[1]",
			args:    "foo bar",
			want:    "First: foo, Second: bar",
		},
		{
			name:    "shorthand indexed",
			content: "First: $0, Second: $1",
			args:    "foo bar",
			want:    "First: foo, Second: bar",
		},
		{
			name:     "named arguments",
			content:  "PR: $pr_number, Branch: $target_branch",
			args:     "123 main",
			argNames: []string{"pr_number", "target_branch"},
			want:     "PR: 123, Branch: main",
		},
		{
			name:    "quoted arguments",
			content: "Value: $0",
			args:    `"hello world"`,
			want:    "Value: hello world",
		},
		{
			name:    "no placeholder appends arguments",
			content: "No placeholders here",
			args:    "some args",
			want:    "No placeholders here\n\nARGUMENTS: some args",
		},
		{
			name:    "empty args no change",
			content: "Content",
			args:    "",
			want:    "Content",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SubstituteArguments(tt.content, tt.args, tt.argNames)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestParseArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args string
		want []string
	}{
		{
			name: "simple",
			args: "foo bar baz",
			want: []string{"foo", "bar", "baz"},
		},
		{
			name: "double quoted",
			args: `foo "hello world" baz`,
			want: []string{"foo", "hello world", "baz"},
		},
		{
			name: "single quoted",
			args: `foo 'hello world' baz`,
			want: []string{"foo", "hello world", "baz"},
		},
		{
			name: "escaped",
			args: `foo\ bar baz`,
			want: []string{"foo bar", "baz"},
		},
		{
			name: "empty",
			args: "",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseArguments(tt.args)
			require.Equal(t, tt.want, got)
		})
	}
}
