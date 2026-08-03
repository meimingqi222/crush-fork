package oauth

import (
	"log/slog"
	"time"
)

// minRefreshBuffer is the minimum time before expiry to trigger a refresh.
const minRefreshBuffer = 60

// Token represents an OAuth2 token.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
}

// SetExpiresAt calculates and sets the ExpiresAt field based on the
// current time and ExpiresIn. If ExpiresIn is invalid it falls back to
// a valid ExpiresAt if present, otherwise marks the token as expired.
func (t *Token) SetExpiresAt() {
	if t.ExpiresIn > 0 {
		t.ExpiresAt = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second).Unix()
		return
	}

	// ExpiresIn is missing or invalid. Trust a valid ExpiresAt if the
	// provider still returned one (some return exp directly).
	if t.ExpiresAt > 0 {
		slog.Warn("OAuth token has invalid expires_in but valid expires_at, using expires_at",
			"expires_in", t.ExpiresIn, "expires_at", t.ExpiresAt)
		return
	}

	// Neither field is usable. Mark as expired so the caller is forced
	// to refresh rather than operating with a fabricated lifetime.
	slog.Warn("OAuth token has no valid expiry information, marking as expired",
		"expires_in", t.ExpiresIn, "expires_at", t.ExpiresAt)
	t.ExpiresAt = 0
}

// IsExpired checks if the token is expired or about to expire. It
// uses a buffer of max(expires_in/10, minRefreshBuffer) seconds to
// trigger proactive refresh before the token actually expires.
func (t *Token) IsExpired() bool {
	buffer := max(int64(t.ExpiresIn)/10, minRefreshBuffer)
	return time.Now().Unix() >= (t.ExpiresAt - buffer)
}

// SetExpiresIn calculates and sets the ExpiresIn field based on the ExpiresAt field.
func (t *Token) SetExpiresIn() {
	t.ExpiresIn = int(time.Until(time.Unix(t.ExpiresAt, 0)).Seconds())
}
