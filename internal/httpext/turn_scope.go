package httpext

import "context"

type turnScopeKey struct{}

// WithResponsesWebSocketTurnScope tags outbound HTTP requests with the current
// user-turn identifier for x-codex-turn-state sticky routing. Must not be
// replayed across different user turns.
func WithResponsesWebSocketTurnScope(ctx context.Context, scope string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if scope == "" {
		return ctx
	}
	return context.WithValue(ctx, turnScopeKey{}, scope)
}

func responsesWebSocketTurnScope(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(turnScopeKey{}).(string)
	return v
}