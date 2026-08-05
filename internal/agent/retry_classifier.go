package agent

import (
	"errors"
	"log/slog"
	"time"

	"charm.land/fantasy"
)

// RetryErrorCategory classifies provider errors into categories that
// determine retry behavior and diagnostics.
type RetryErrorCategory string

const (
	RetryCategoryAuth      RetryErrorCategory = "auth"
	RetryCategoryTransport RetryErrorCategory = "transport"
	RetryCategoryContext   RetryErrorCategory = "context_overflow"
	RetryCategoryTool      RetryErrorCategory = "tool_validation"
	RetryCategoryRateLimit RetryErrorCategory = "rate_limit"
	RetryCategoryServer    RetryErrorCategory = "server_error"
	RetryCategoryUnknown   RetryErrorCategory = "unknown"
)

// ClassifyProviderError categorizes a provider error for retry diagnostics.
// It uses the fantasy layer's built-in classification and adds Crush-specific
// categories for context overflow and tool validation errors.
func ClassifyProviderError(err error) RetryErrorCategory {
	if err == nil {
		return RetryCategoryUnknown
	}

	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		// Auth errors (401 or AuthError flag) are handled by OnAuthRefresh.
		if providerErr.AuthError || providerErr.StatusCode == 401 {
			return RetryCategoryAuth
		}
		// Check for rate limiting (429).
		if providerErr.StatusCode == 429 {
			return RetryCategoryRateLimit
		}
		// Check for server errors (5xx).
		if providerErr.StatusCode >= 500 && providerErr.StatusCode < 600 {
			return RetryCategoryServer
		}
		// Transport errors are retryable network-level issues.
		if fantasy.IsTransportError(err) {
			return RetryCategoryTransport
		}
		// Retryable provider errors that don't fit above.
		if providerErr.IsRetryable() {
			return RetryCategoryTransport
		}
	}

	// Context window overflow is detected by message content.
	msg := err.Error()
	if containsAny(msg, "context length", "context window", "maximum context", "token limit exceeded") {
		return RetryCategoryContext
	}

	// Tool validation errors come from our own tool layer, not the provider.
	if containsAny(msg, "tool validation", "invalid tool input", "schema validation") {
		return RetryCategoryTool
	}

	return RetryCategoryUnknown
}

// RetryDiagnostic records a single retry attempt for observability.
type RetryDiagnostic struct {
	Attempt     int                `json:"attempt"`
	Category    RetryErrorCategory `json:"category"`
	Reason      string             `json:"reason"`
	FinalAction string             `json:"final_action,omitempty"`
	Duration    time.Duration      `json:"duration,omitempty"`
}

// LogRetryDiagnostic emits a structured log line for a retry attempt when
// CRUSH_CONTEXT_USAGE_DIAG=1 (reusing the same diagnostic toggle).
func LogRetryDiagnostic(d RetryDiagnostic) {
	if !contextUsageDiagEnabled() {
		return
	}
	slog.Info("Retry diagnostic",
		"attempt", d.Attempt,
		"category", string(d.Category),
		"reason", d.Reason,
		"final_action", d.FinalAction,
		"duration_ms", d.Duration.Milliseconds(),
	)
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
