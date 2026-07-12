package httpext

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func parseResponseIDFromEvent(data []byte) string {
	var envelope struct {
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ""
	}
	return envelope.Response.ID
}