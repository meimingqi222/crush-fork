package plugin

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/message"
)

type Runtime struct {
	mu               sync.RWMutex
	plugins          []Plugin
	initializedHooks []Hooks
	customTools      map[string]ToolDefinition
}

var (
	defaultRuntimeMu sync.RWMutex
	defaultRuntime   = NewRuntime()
)

func NewRuntime() *Runtime {
	return &Runtime{
		customTools: make(map[string]ToolDefinition),
	}
}

func DefaultRuntime() *Runtime {
	defaultRuntimeMu.RLock()
	defer defaultRuntimeMu.RUnlock()
	return defaultRuntime
}

func SetDefaultRuntime(runtime *Runtime) *Runtime {
	if runtime == nil {
		runtime = NewRuntime()
	}
	defaultRuntimeMu.Lock()
	defer defaultRuntimeMu.Unlock()
	previous := defaultRuntime
	defaultRuntime = runtime
	return previous
}

func (r *Runtime) ensureCustomToolsLocked() {
	if r.customTools == nil {
		r.customTools = make(map[string]ToolDefinition)
	}
}

func (r *Runtime) snapshotInitializedHooks() []Hooks {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Hooks(nil), r.initializedHooks...)
}

func (r *Runtime) snapshotPlugins() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Plugin(nil), r.plugins...)
}

func triggerTransformHooks[Input any, Output any](
	hooks []Hooks,
	ctx context.Context,
	name string,
	input Input,
	output Output,
	resolve func(Hooks) func(context.Context, Input, *Output) error,
) (Output, error) {
	start := time.Now()

	for i, hook := range hooks {
		fn := resolve(hook)
		if fn == nil {
			continue
		}
		hookStart := time.Now()
		if err := fn(ctx, input, &output); err != nil {
			return output, err
		}
		slog.Debug("[PERF] Plugin transform hook completed", "trigger", name, "hook_index", i, "duration", time.Since(hookStart))
	}

	slog.Debug("[PERF] Plugin transform hooks done", "trigger", name, "total_duration", time.Since(start))
	return output, nil
}

func (r *Runtime) Register(p Plugin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins = append(r.plugins, p)
	slog.Debug("Plugin registered", "name", p.Name())
}

func Register(p Plugin) {
	DefaultRuntime().Register(p)
}

func (r *Runtime) Init(ctx context.Context, input PluginInput) error {
	r.mu.Lock()
	r.initializedHooks = nil
	r.customTools = make(map[string]ToolDefinition)
	manuallyRegistered := append([]Plugin(nil), r.plugins...)
	for _, p := range r.plugins {
		if err := p.Close(ctx); err != nil {
			slog.Debug("Failed to close plugin during init", "name", p.Name(), "error", err)
		}
	}
	r.plugins = nil
	r.mu.Unlock()

	configuredPlugins, err := newConfiguredPlugins(input)
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.plugins = append(append([]Plugin(nil), manuallyRegistered...), configuredPlugins...)
	r.mu.Unlock()

	toolSources := make(map[string]string)
	initPlugin := func(p Plugin) {
		slog.Info("Initializing plugin", "name", p.Name())
		h, err := p.Init(ctx, input)
		if err != nil {
			slog.Error("Failed to initialize plugin", "name", p.Name(), "error", err)
			return
		}

		r.mu.Lock()
		defer r.mu.Unlock()
		r.initializedHooks = append(r.initializedHooks, h)
		r.ensureCustomToolsLocked()
		for name, tool := range h.Tools {
			source := "plugin:" + p.Name()
			if existingSource, exists := toolSources[name]; exists {
				slog.Warn("Custom tool registration collision", "tool", name, "existing_source", existingSource, "overriding_source", source)
			}
			r.customTools[name] = tool
			toolSources[name] = source
		}

		slog.Info("Plugin initialized", "name", p.Name(), "tools", len(h.Tools))
	}

	for _, p := range manuallyRegistered {
		initPlugin(p)
	}
	for _, p := range configuredPlugins {
		initPlugin(p)
	}

	localTools, err := DiscoverLocalTools(input.WorkingDir)
	if err != nil {
		slog.Error("Failed to discover local tools", "error", err)
		return err
	}
	r.mu.Lock()
	r.ensureCustomToolsLocked()
	for name, tool := range localTools {
		if existingSource, exists := toolSources[name]; exists {
			slog.Warn("Custom tool registration collision", "tool", name, "existing_source", existingSource, "overriding_source", "local")
		}
		r.customTools[name] = tool
		toolSources[name] = "local"
	}
	r.mu.Unlock()
	if len(localTools) > 0 {
		slog.Info("Local tools loaded", "count", len(localTools))
	}

	return nil
}

func Init(ctx context.Context, input PluginInput) error {
	return DefaultRuntime().Init(ctx, input)
}

func (r *Runtime) GetHooks() Hooks {
	r.mu.RLock()
	defer r.mu.RUnlock()

	hooks := Hooks{Tools: make(map[string]ToolDefinition, len(r.customTools))}
	for name, tool := range r.customTools {
		hooks.Tools[name] = tool
	}
	for _, hook := range r.initializedHooks {
		if hook.ToolBeforeExecute != nil {
			hooks.ToolBeforeExecute = r.TriggerToolBeforeExecute
		}
		if hook.ToolAfterExecute != nil {
			hooks.ToolAfterExecute = r.TriggerToolAfterExecute
		}
		if hook.ChatBeforeRequest != nil {
			hooks.ChatBeforeRequest = r.TriggerChatBeforeRequest
		}
		if hook.ChatAfterResponse != nil {
			hooks.ChatAfterResponse = r.TriggerChatAfterResponse
		}
		if hook.ChatMessagesTransform != nil {
			hooks.ChatMessagesTransform = func(ctx context.Context, input ChatMessagesTransformInput, output *ChatMessagesTransformOutput) error {
				transformed, err := r.TriggerChatMessagesTransform(ctx, input, *output)
				if err != nil {
					return err
				}
				*output = transformed
				return nil
			}
		}
		if hook.ChatSystemTransform != nil {
			hooks.ChatSystemTransform = func(ctx context.Context, input ChatSystemTransformInput, output *ChatSystemTransformOutput) error {
				transformed, err := r.TriggerChatSystemTransform(ctx, input, *output)
				if err != nil {
					return err
				}
				*output = transformed
				return nil
			}
		}
		if hook.SessionCompacting != nil {
			hooks.SessionCompacting = func(ctx context.Context, input SessionCompactingInput, output *SessionCompactingOutput) error {
				transformed, err := r.TriggerSessionCompacting(ctx, input, *output)
				if err != nil {
					return err
				}
				*output = transformed
				return nil
			}
		}
		if hook.PermissionAsk != nil {
			hooks.PermissionAsk = r.TriggerPermissionAsk
		}
		if hook.ShellEnv != nil {
			hooks.ShellEnv = r.TriggerShellEnv
		}
		if hook.MessageCreated != nil {
			hooks.MessageCreated = r.TriggerMessageCreated
		}
	}
	return hooks
}

func GetHooks() Hooks {
	return DefaultRuntime().GetHooks()
}

func (r *Runtime) TriggerToolBeforeExecute(ctx context.Context, input ToolBeforeExecuteInput) (*ToolBeforeExecuteOutput, error) {
	hooks := r.snapshotInitializedHooks()

	currentArgs := input.Args
	changed := false
	for _, hook := range hooks {
		if hook.ToolBeforeExecute == nil {
			continue
		}
		out, err := hook.ToolBeforeExecute(ctx, ToolBeforeExecuteInput{
			Tool:      input.Tool,
			SessionID: input.SessionID,
			CallID:    input.CallID,
			Args:      currentArgs,
		})
		if err != nil {
			return nil, err
		}
		if out == nil {
			continue
		}
		if out.Args != nil {
			currentArgs = out.Args
			changed = true
		}
		if out.Skip {
			return &ToolBeforeExecuteOutput{Args: currentArgs, Skip: true, PreResult: out.PreResult}, nil
		}
	}
	if !changed {
		return nil, nil
	}
	return &ToolBeforeExecuteOutput{Args: currentArgs}, nil
}

func TriggerToolBeforeExecute(ctx context.Context, input ToolBeforeExecuteInput) (*ToolBeforeExecuteOutput, error) {
	return DefaultRuntime().TriggerToolBeforeExecute(ctx, input)
}

func (r *Runtime) TriggerToolAfterExecute(ctx context.Context, input ToolAfterExecuteInput) (*ToolAfterExecuteOutput, error) {
	hooks := r.snapshotInitializedHooks()

	currentResult := input.Result
	currentMetadata := input.Metadata
	changed := false
	for _, hook := range hooks {
		if hook.ToolAfterExecute == nil {
			continue
		}
		out, err := hook.ToolAfterExecute(ctx, ToolAfterExecuteInput{
			Tool:      input.Tool,
			SessionID: input.SessionID,
			CallID:    input.CallID,
			Args:      input.Args,
			Result:    currentResult,
			Metadata:  currentMetadata,
		})
		if err != nil {
			return nil, err
		}
		if out == nil {
			continue
		}
		if out.ResultChanged {
			currentResult = out.Result
			changed = true
		}
		if out.Metadata != nil {
			currentMetadata = out.Metadata
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	return &ToolAfterExecuteOutput{Result: currentResult, ResultChanged: true, Metadata: currentMetadata}, nil
}

func TriggerToolAfterExecute(ctx context.Context, input ToolAfterExecuteInput) (*ToolAfterExecuteOutput, error) {
	return DefaultRuntime().TriggerToolAfterExecute(ctx, input)
}

func (r *Runtime) TriggerChatBeforeRequest(ctx context.Context, input ChatBeforeRequestInput) error {
	hooks := r.snapshotInitializedHooks()

	for _, hook := range hooks {
		if hook.ChatBeforeRequest == nil {
			continue
		}
		if err := hook.ChatBeforeRequest(ctx, input); err != nil {
			return err
		}
	}
	return nil
}

func TriggerChatBeforeRequest(ctx context.Context, input ChatBeforeRequestInput) error {
	return DefaultRuntime().TriggerChatBeforeRequest(ctx, input)
}

func (r *Runtime) TriggerChatAfterResponse(ctx context.Context, input ChatAfterResponseInput) error {
	hooks := r.snapshotInitializedHooks()

	for _, hook := range hooks {
		if hook.ChatAfterResponse == nil {
			continue
		}
		if err := hook.ChatAfterResponse(ctx, input); err != nil {
			return err
		}
	}
	return nil
}

func TriggerChatAfterResponse(ctx context.Context, input ChatAfterResponseInput) error {
	return DefaultRuntime().TriggerChatAfterResponse(ctx, input)
}

func (r *Runtime) TriggerChatMessagesTransform(ctx context.Context, input ChatMessagesTransformInput, output ChatMessagesTransformOutput) (ChatMessagesTransformOutput, error) {
	transformed, err := triggerTransformHooks(r.snapshotInitializedHooks(), ctx, "chat_messages_transform", input, output, func(hook Hooks) func(context.Context, ChatMessagesTransformInput, *ChatMessagesTransformOutput) error {
		return hook.ChatMessagesTransform
	})
	if err != nil {
		return transformed, err
	}
	slog.Debug("[PERF] ChatMessagesTransform all hooks done", "session_id", input.SessionID, "msg_count", len(transformed.Messages))
	return transformed, nil
}

func TriggerChatMessagesTransform(ctx context.Context, input ChatMessagesTransformInput, output ChatMessagesTransformOutput) (ChatMessagesTransformOutput, error) {
	return DefaultRuntime().TriggerChatMessagesTransform(ctx, input, output)
}

func (r *Runtime) TriggerChatSystemTransform(ctx context.Context, input ChatSystemTransformInput, output ChatSystemTransformOutput) (ChatSystemTransformOutput, error) {
	return triggerTransformHooks(r.snapshotInitializedHooks(), ctx, "chat_system_transform", input, output, func(hook Hooks) func(context.Context, ChatSystemTransformInput, *ChatSystemTransformOutput) error {
		return hook.ChatSystemTransform
	})
}

func TriggerChatSystemTransform(ctx context.Context, input ChatSystemTransformInput, output ChatSystemTransformOutput) (ChatSystemTransformOutput, error) {
	return DefaultRuntime().TriggerChatSystemTransform(ctx, input, output)
}

func (r *Runtime) TriggerSessionCompacting(ctx context.Context, input SessionCompactingInput, output SessionCompactingOutput) (SessionCompactingOutput, error) {
	return triggerTransformHooks(r.snapshotInitializedHooks(), ctx, "session_compacting", input, output, func(hook Hooks) func(context.Context, SessionCompactingInput, *SessionCompactingOutput) error {
		return hook.SessionCompacting
	})
}

func TriggerSessionCompacting(ctx context.Context, input SessionCompactingInput, output SessionCompactingOutput) (SessionCompactingOutput, error) {
	return DefaultRuntime().TriggerSessionCompacting(ctx, input, output)
}

func (r *Runtime) TriggerPermissionAsk(input PermissionAskInput) PermissionAskOutput {
	hooks := r.snapshotInitializedHooks()

	decision := PermissionAskOutput{Action: PermissionAsk}
	for _, hook := range hooks {
		if hook.PermissionAsk == nil {
			continue
		}
		out := hook.PermissionAsk(input)
		if out.Action != PermissionAsk {
			decision = out
		}
	}
	return decision
}

func TriggerPermissionAsk(input PermissionAskInput) PermissionAskOutput {
	return DefaultRuntime().TriggerPermissionAsk(input)
}

func (r *Runtime) TriggerShellEnv(ctx context.Context, input ShellEnvInput) map[string]string {
	hooks := r.snapshotInitializedHooks()

	env := make(map[string]string)
	for _, hook := range hooks {
		if hook.ShellEnv == nil {
			continue
		}
		current := hook.ShellEnv(ctx, input)
		if len(current) == 0 {
			continue
		}
		for key, value := range current {
			env[key] = value
		}
	}
	return env
}

func TriggerShellEnv(ctx context.Context, input ShellEnvInput) map[string]string {
	return DefaultRuntime().TriggerShellEnv(ctx, input)
}

func (r *Runtime) TriggerMessageCreated(ctx context.Context, msg message.Message) error {
	hooks := r.snapshotInitializedHooks()

	for _, hook := range hooks {
		if hook.MessageCreated == nil {
			continue
		}
		if err := hook.MessageCreated(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}

func TriggerMessageCreated(ctx context.Context, msg message.Message) error {
	return DefaultRuntime().TriggerMessageCreated(ctx, msg)
}

func (r *Runtime) GetCustomTools() map[string]ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]ToolDefinition, len(r.customTools))
	for k, v := range r.customTools {
		result[k] = v
	}
	return result
}

func GetCustomTools() map[string]ToolDefinition {
	return DefaultRuntime().GetCustomTools()
}

func (r *Runtime) ListPlugins() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := make(map[string]struct{})
	for _, p := range r.plugins {
		seen[p.Name()] = struct{}{}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	return names
}

func ListPlugins() []string {
	return DefaultRuntime().ListPlugins()
}

func (r *Runtime) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, p := range r.plugins {
		if err := p.Close(context.Background()); err != nil {
			slog.Debug("Failed to close plugin during reset", "name", p.Name(), "error", err)
		}
	}

	r.plugins = nil
	r.initializedHooks = nil
	r.customTools = make(map[string]ToolDefinition)
}

func Reset() {
	DefaultRuntime().Reset()
}

func (r *Runtime) Close(ctx context.Context) {
	registeredPlugins := r.snapshotPlugins()
	for _, p := range registeredPlugins {
		if err := p.Close(ctx); err != nil {
			slog.Error("Failed to close plugin", "name", p.Name(), "error", err)
		}
	}
}

func Close(ctx context.Context) {
	DefaultRuntime().Close(ctx)
}
