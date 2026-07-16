// Package mcplifecycle owns root-session MCP transport capabilities and the
// asynchronous lifecycle exposed to ACP desktop clients.
package mcplifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/charmbracelet/crush/internal/config"
)

var (
	ErrNotFound = errors.New("mcp server not found")
	ErrCapacity = errors.New("mcp lifecycle capacity exhausted")
	ErrClosed   = errors.New("mcp lifecycle service is closed")
)

type Status string

const (
	StatusDisabled     Status = "disabled"
	StatusStarting     Status = "starting"
	StatusConnected    Status = "connected"
	StatusDegraded     Status = "degraded"
	StatusFailed       Status = "failed"
	StatusReconnecting Status = "reconnecting"
	StatusStopping     Status = "stopping"
)

type Scope string

const (
	ScopeStatic  Scope = "static"
	ScopeSession Scope = "session"
)

type Counts struct {
	Tools     int
	Prompts   int
	Resources int
}

type ServerConfig struct {
	Name   string
	Config config.MCPConfig
}

type Server struct {
	ID        string
	Name      string
	Scope     Scope
	Status    Status
	Counts    Counts
	Revision  uint64
	ErrorCode string
	UpdatedAt time.Time
}

type LogEntry struct {
	Sequence  uint64
	Timestamp time.Time
	Level     string
	Logger    string
	Message   string
}

type LogPage struct {
	Entries        []LogEntry
	LatestSequence uint64
	Truncated      bool
}

type BackendState string

const (
	BackendDisabled    BackendState = "disabled"
	BackendStarting    BackendState = "starting"
	BackendConnected   BackendState = "connected"
	BackendNeedsAuth   BackendState = "needs_auth"
	BackendError       BackendState = "error"
	BackendCached      BackendState = "cached"
	BackendCircuitOpen BackendState = "circuit_open"
)

type BackendInfo struct {
	State  BackendState
	Counts Counts
}

type BackendEventType uint8

const (
	BackendEventState BackendEventType = iota
	BackendEventLog
)

type BackendLog struct {
	Timestamp time.Time
	Level     string
	Logger    string
	Data      any
}

type BackendEvent struct {
	Type   BackendEventType
	Name   string
	State  BackendState
	Counts Counts
	Log    BackendLog
}

type Backend interface {
	Connect(context.Context, *config.ConfigStore, string) error
	Reconnect(context.Context, *config.ConfigStore, string) error
	Disable(context.Context, *config.ConfigStore, string) error
	State(string) (BackendInfo, bool)
	Subscribe(context.Context) <-chan BackendEvent
	MarkScoped(string)
}

// Access is the live capability object installed in Agent execution contexts.
// It intentionally reads current service state so replacement revokes already
// cached tool objects without rebuilding a Coordinator-side permission map.
type Access struct {
	service   *Service
	sessionID string
}

func (a *Access) AllowsMCPServer(name string) bool {
	return a != nil && a.service != nil && a.service.allows(a.sessionID, name)
}

func (a *Access) MCPServerScope() string {
	if a == nil {
		return ""
	}
	return a.sessionID
}

func (a *Access) MCPServerRevision() uint64 {
	if a == nil || a.service == nil {
		return 0
	}
	return a.service.revision(a.sessionID)
}
