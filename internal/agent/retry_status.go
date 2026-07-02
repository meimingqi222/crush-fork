package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
)

const (
	retryStatusPrefix      = "Service temporarily unavailable"
	retryStatusErrorPrefix = "Error: "
)

// FormatTransientRetryMessage builds the temporary retry status shown while a
// transient provider failure is being retried.
func FormatTransientRetryMessage(err error, delay time.Duration, attempt, maxAttempts int) string {
	countdown := fmt.Sprintf(
		"Retrying in %d seconds... (attempt %d/%d)",
		int(delay.Seconds()),
		attempt,
		maxAttempts,
	)
	reason := TransientErrorReason(err)
	if reason == "" {
		return retryStatusPrefix + ". " + countdown
	}
	return retryStatusPrefix + ". " + countdown + "\n" + retryStatusErrorPrefix + reason
}

// TransientErrorReason extracts a user-facing reason from a transient error.
func TransientErrorReason(err error) string {
	if err == nil {
		return ""
	}

	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		if msg := strings.TrimSpace(providerErr.Message); msg != "" {
			return msg
		}
		if title := strings.TrimSpace(providerErrorTitle(providerErr)); title != "" {
			return title
		}
	}

	var fantasyErr *fantasy.Error
	if errors.As(err, &fantasyErr) {
		if msg := strings.TrimSpace(fantasyErr.Message); msg != "" {
			return msg
		}
		if title := strings.TrimSpace(fantasyErr.Title); title != "" {
			return title
		}
	}

	return strings.TrimSpace(err.Error())
}

// ParseRetryStatus splits a transient retry status message into its optional
// provider error reason and the full display text.
func ParseRetryStatus(text string) (reason string, display string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, retryStatusPrefix) {
		return "", "", false
	}

	display = text
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, retryStatusErrorPrefix) {
			reason = strings.TrimSpace(strings.TrimPrefix(line, retryStatusErrorPrefix))
			break
		}
	}
	return reason, display, true
}

// RetryStatusCountdown returns the compact retry countdown line without the
// provider error reason.
func RetryStatusCountdown(text string) (countdown string, ok bool) {
	_, display, ok := ParseRetryStatus(text)
	if !ok {
		return "", false
	}
	countdown, _, _ = strings.Cut(display, "\n")
	return strings.TrimSpace(countdown), true
}

// RetryStatusDisplayText returns the user-facing retry status with the
// provider error reason appended when available.
func RetryStatusDisplayText(text string) (display string, ok bool) {
	reason, _, ok := ParseRetryStatus(text)
	if !ok {
		return "", false
	}
	countdown, ok := RetryStatusCountdown(text)
	if !ok {
		return "", false
	}
	if reason != "" {
		return countdown + " — " + reason, true
	}
	return countdown, true
}
