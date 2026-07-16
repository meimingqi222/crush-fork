package agent

import "context"

type turnIDContextKey struct{}

// WithTurnID binds a caller-owned turn identity to one Coordinator run.
func WithTurnID(ctx context.Context, turnID string) context.Context {
	return context.WithValue(ctx, turnIDContextKey{}, turnID)
}

func turnIDFromContext(ctx context.Context) string {
	return TurnIDFromContext(ctx)
}

// TurnIDFromContext returns a caller-owned GUI turn identity, when present.
func TurnIDFromContext(ctx context.Context) string {
	turnID, _ := ctx.Value(turnIDContextKey{}).(string)
	return turnID
}
