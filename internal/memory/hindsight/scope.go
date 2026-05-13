package hindsight

import "strings"

const projectTagPrefix = "project:"

// Scope is the resolved Hindsight memory target for a project.
type Scope struct {
	BankID          string
	RetainTags      []string
	RecallTags      []string
	RecallTagsMatch string
}

// ResolveScope converts the configured scoping mode into the concrete bank and
// tag settings used by retain, recall, and reflect requests.
func ResolveScope(baseBankID, scoping, projectLabel string) Scope {
	base := strings.TrimSpace(baseBankID)
	if base == "" {
		base = defaultBankID
	}
	project := strings.TrimSpace(projectLabel)
	if project == "" {
		project = "unknown"
	}

	switch strings.ToLower(strings.TrimSpace(scoping)) {
	case "global":
		return Scope{BankID: base}
	case "per-project", "project":
		return Scope{BankID: base + "-" + project}
	default:
		tag := projectTagPrefix + project
		return Scope{
			BankID:          base,
			RetainTags:      []string{tag},
			RecallTags:      []string{tag},
			RecallTagsMatch: "any",
		}
	}
}
