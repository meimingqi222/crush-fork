package httpext

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

func newTurnScopeID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("turn-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
