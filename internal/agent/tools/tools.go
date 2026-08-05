package tools

import (
	"context"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

type (
	sessionIDContextKey       string
	parentSessionIDContextKey string
	subagentContextKey        string
	messageIDContextKey       string
	messageServiceContextKey  string
	supportsImagesKey         string
	modelNameKey              string
	workingDirContextKey      string
	sessionServiceContextKey  string
	toolCallIDContextKey      string
	agentMemoryContextKey     string
	agentIsolationContextKey  string
	agentBackgroundContextKey string
	visionServiceKey          string
	steeringSignalContextKey  string
)

type sessionLookupService interface {
	Get(context.Context, string) (session.Session, error)
	Save(context.Context, session.Session) (session.Session, error)
	UpdatePlanFilePath(context.Context, string, string) (session.Session, error)
}

const (
	// SessionIDContextKey is the key for the session ID in the context.
	SessionIDContextKey sessionIDContextKey = "session_id"
	// ParentSessionIDContextKey is the key for the parent session ID in the
	// context when a tool is invoked from a subagent.
	ParentSessionIDContextKey parentSessionIDContextKey = "parent_session_id"
	// SubagentContextKey is set to true when the tool is invoked from a subagent.
	SubagentContextKey subagentContextKey = "subagent"
	// MessageIDContextKey is the key for the message ID in the context.
	MessageIDContextKey messageIDContextKey = "message_id"
	// MessageServiceContextKey is the key for the message service in the context.
	MessageServiceContextKey messageServiceContextKey = "message_service"
	// SupportsImagesContextKey is the key for the model's image support capability.
	SupportsImagesContextKey supportsImagesKey = "supports_images"
	// ModelNameContextKey is the key for the model name in the context.
	ModelNameContextKey modelNameKey = "model_name"
	// WorkingDirContextKey is the key for the session-specific working directory.
	WorkingDirContextKey      workingDirContextKey      = "working_dir"
	SessionServiceContextKey  sessionServiceContextKey  = "session_service"
	ToolCallIDContextKey      toolCallIDContextKey      = "tool_call_id"
	AgentMemoryContextKey     agentMemoryContextKey     = "agent_memory"
	AgentIsolationContextKey  agentIsolationContextKey  = "agent_isolation"
	AgentBackgroundContextKey agentBackgroundContextKey = "agent_background"
	// VisionServiceContextKey is the key for the vision helper service in the context.
	VisionServiceContextKey visionServiceKey = "vision_service"
	// SteeringSignalContextKey is the key for the per-run cooperative
	// steering signal. Tools select on the signal's Done() channel to notice
	// a mid-turn steering message and yield at a safe point instead of
	// blocking the run.
	SteeringSignalContextKey steeringSignalContextKey = "steering_signal"
)

// getContextValue is a generic helper that retrieves a typed value from context.
// If the value is not found or has the wrong type, it returns the default value.
func getContextValue[T any](ctx context.Context, key any, defaultValue T) T {
	value := ctx.Value(key)
	if value == nil {
		return defaultValue
	}
	if typedValue, ok := value.(T); ok {
		return typedValue
	}
	return defaultValue
}

// GetSessionFromContext retrieves the session ID from the context.
// GetSessionFromContext retrieves the session ID from the context.
func GetSessionFromContext(ctx context.Context) string {
	return getContextValue(ctx, SessionIDContextKey, "")
}

// GetMessageFromContext retrieves the message ID from the context.
func GetMessageFromContext(ctx context.Context) string {
	return getContextValue(ctx, MessageIDContextKey, "")
}

// GetSupportsImagesFromContext retrieves whether the model supports images from the context.
func GetSupportsImagesFromContext(ctx context.Context) bool {
	return getContextValue(ctx, SupportsImagesContextKey, false)
}

// GetModelNameFromContext retrieves the model name from the context.
func GetModelNameFromContext(ctx context.Context) string {
	return getContextValue(ctx, ModelNameContextKey, "")
}

// GetWorkingDirFromContext retrieves the session-specific working directory from context.
// Returns empty string if not set, in which case tools should fall back to the global working dir.
func GetWorkingDirFromContext(ctx context.Context) string {
	return getContextValue(ctx, WorkingDirContextKey, "")
}

func GetSessionServiceFromContext(ctx context.Context) sessionLookupService {
	return getContextValue(ctx, SessionServiceContextKey, sessionLookupService(nil))
}

func GetMessageServiceFromContext(ctx context.Context) message.Service {
	return getContextValue(ctx, MessageServiceContextKey, message.Service(nil))
}

func GetToolCallIDFromContext(ctx context.Context) string {
	return getContextValue(ctx, ToolCallIDContextKey, "")
}

func GetAgentMemoryFromContext(ctx context.Context) string {
	return getContextValue(ctx, AgentMemoryContextKey, "")
}

func GetAgentIsolationFromContext(ctx context.Context) string {
	return getContextValue(ctx, AgentIsolationContextKey, "")
}

func GetAgentBackgroundFromContext(ctx context.Context) bool {
	return getContextValue(ctx, AgentBackgroundContextKey, false)
}

// VisionDescriber is the interface implemented by the vision helper service.
// Tools use it to describe images when the main model does not support vision.
type VisionDescriber interface {
	// DescribeImage sends the image data to a vision-capable model and returns
	// a text description. The optional prompt customizes the description request.
	DescribeImage(ctx context.Context, data []byte, mimeType string, prompt string) (string, error)
	// IsAvailable returns true when a vision helper model is configured.
	IsAvailable() bool
}

// GetVisionServiceFromContext retrieves the vision helper service from the context.
// Returns nil when no vision helper is configured.
func GetVisionServiceFromContext(ctx context.Context) VisionDescriber {
	return getContextValue(ctx, VisionServiceContextKey, VisionDescriber(nil))
}

// IsSubagentFromContext reports whether the tool is being called from a subagent.
func IsSubagentFromContext(ctx context.Context) bool {
	return getContextValue(ctx, SubagentContextKey, false)
}

// GetParentSessionIDFromContext returns the parent session ID when the tool is
// invoked from a subagent, or empty string otherwise.
func GetParentSessionIDFromContext(ctx context.Context) string {
	return getContextValue(ctx, ParentSessionIDContextKey, "")
}

// SteeringSignal is the per-run cooperative interruption signal exposed to
// tools. Done() closes when a mid-turn steering message arrives; tools may
// select on it and yield at a safe point. The zero value signals nothing.
type SteeringSignal interface {
	Done() <-chan struct{}
}

// GetSteeringSignalFromContext returns the current run's steering signal, or
// nil when no signal is present (e.g. the tool runs outside a session run).
func GetSteeringSignalFromContext(ctx context.Context) SteeringSignal {
	return getContextValue(ctx, SteeringSignalContextKey, SteeringSignal(nil))
}
