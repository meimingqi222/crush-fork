package mcplifecycle

import (
	"context"

	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	transport "github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
)

type DefaultBackend struct{}

func NewBackend() DefaultBackend { return DefaultBackend{} }

func (DefaultBackend) Connect(ctx context.Context, store *config.ConfigStore, name string) error {
	return transport.Reconnect(ctx, store, name)
}

func (DefaultBackend) Reconnect(ctx context.Context, store *config.ConfigStore, name string) error {
	return transport.ResetCircuitBreaker(ctx, store, name)
}

func (DefaultBackend) Disable(_ context.Context, store *config.ConfigStore, name string) error {
	return transport.DisableSingle(store, name)
}

func (DefaultBackend) State(name string) (BackendInfo, bool) {
	info, ok := transport.GetState(name)
	if !ok {
		return BackendInfo{}, false
	}
	return BackendInfo{State: backendState(info.State), Counts: Counts{
		Tools: info.Counts.Tools, Prompts: info.Counts.Prompts, Resources: info.Counts.Resources,
	}}, true
}

func (DefaultBackend) Subscribe(ctx context.Context) <-chan BackendEvent {
	output := make(chan BackendEvent, 32)
	input := transport.SubscribeEvents(ctx)
	go func() {
		defer close(output)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-input:
				if !ok {
					return
				}
				value := event.Payload
				mapped := BackendEvent{Name: value.Name}
				if value.Type == transport.EventLogMessage {
					mapped.Type = BackendEventLog
					mapped.Log = BackendLog{
						Timestamp: value.Log.Timestamp, Level: value.Log.Level,
						Logger: value.Log.Logger, Data: value.Log.Data,
					}
				} else if value.Type == transport.EventStateChanged {
					mapped.Type = BackendEventState
					mapped.State = backendState(value.State)
					mapped.Counts = Counts{
						Tools: value.Counts.Tools, Prompts: value.Counts.Prompts,
						Resources: value.Counts.Resources,
					}
				} else {
					continue
				}
				select {
				case output <- mapped:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return output
}

func (DefaultBackend) MarkScoped(name string) { agenttools.MarkMCPServerScoped(name) }

func backendState(state transport.State) BackendState {
	switch state {
	case transport.StateDisabled:
		return BackendDisabled
	case transport.StateStarting:
		return BackendStarting
	case transport.StateConnected:
		return BackendConnected
	case transport.StateNeedsAuth:
		return BackendNeedsAuth
	case transport.StateCached:
		return BackendCached
	case transport.StateCircuitOpen:
		return BackendCircuitOpen
	case transport.StateError:
		return BackendError
	default:
		return BackendError
	}
}
