package agent

import "regexp"

// secretPatterns holds compiled regexps for known API key / secret formats.
var secretPatterns = []*regexp.Regexp{
	// OpenAI style: sk-proj-... or sk-ant-... or sk-...
	regexp.MustCompile(`sk-(?:proj-|ant-)?[A-Za-z0-9_\-]{20,}`),
	// HuggingFace
	regexp.MustCompile(`hf_[A-Za-z0-9]{30,}`),
	// GitHub PAT (classic)
	regexp.MustCompile(`ghp_[A-Za-z0-9]{36,}`),
	// GitHub PAT (fine-grained)
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{40,}`),
	// AWS Access Key ID
	regexp.MustCompile(`AKIA[A-Z0-9]{16}`),
	// Google API key
	regexp.MustCompile(`AIza[A-Za-z0-9\-_]{35}`),
	// Stripe secret key
	regexp.MustCompile(`sk_(?:live|test)_[A-Za-z0-9]{20,}`),
}

// redactSecrets replaces known secret patterns in text with [REDACTED].
func redactSecrets(text string) string {
	for _, re := range secretPatterns {
		text = re.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}
