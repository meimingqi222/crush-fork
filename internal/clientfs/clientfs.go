// Package clientfs provides a revision-aware, execution-scoped bridge to an
// ACP client's filesystem.
package clientfs

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"
)

var (
	ErrInvalidPath      = errors.New("client filesystem path is invalid")
	ErrPathEscape       = errors.New("client filesystem path escapes workspace")
	ErrRevisionConflict = errors.New("client filesystem revision conflict")
	ErrNotFound         = fs.ErrNotExist
	ErrPayloadTooLarge  = errors.New("client filesystem payload is too large")
	ErrInvalidResponse  = errors.New("client filesystem response is invalid")
	ErrClosed           = errors.New("client filesystem scope is closed")
)

const (
	defaultMaxFileBytes = 64 * 1024 * 1024
	defaultInlineBytes  = 64 * 1024
	maxRevisionBytes    = 1024
	maxSourceURIBytes   = 8192
)

// Caller sends a reverse JSON-RPC call to the connection that owns a scope.
type Caller interface {
	Call(context.Context, string, any) (json.RawMessage, error)
}

// BlobBridge resolves and publishes connection/session-owned Blob handles.
// Publish returns an idempotent release function for the temporary handle.
type BlobBridge interface {
	Resolve(context.Context, string, string) ([]byte, error)
	Publish(context.Context, string, string, []byte) (string, func(), error)
}

// Config defines one root-session filesystem capability.
type Config struct {
	SessionID    string
	Workspace    string
	Caller       Caller
	Blobs        BlobBridge
	MaxFileBytes int64
	InlineBytes  int64
}

// Metadata is the client-supplied identity of the last observed file state.
type Metadata struct {
	Path      string
	SourceURI string
	Revision  string
	MIMEType  string
	Size      int64
	Exists    bool
	IsDir     bool
}

// Scope is safe for concurrent use. It belongs to one connection, root
// session and workspace; it must never be installed as a process global.
type Scope struct {
	mu        sync.RWMutex
	config    Config
	workspace string
	metadata  map[string]Metadata
	locks     map[string]*sync.Mutex
	closed    bool
}

type scopeContextKey struct{}

// WithScope binds a client filesystem to the complete execution tree rooted
// at ctx. Child agents naturally inherit the same root-session capability.
func WithScope(ctx context.Context, scope *Scope) context.Context {
	if scope == nil {
		return ctx
	}
	return context.WithValue(ctx, scopeContextKey{}, scope)
}

// FromContext returns the execution-scoped client filesystem, if any.
func FromContext(ctx context.Context) (*Scope, bool) {
	scope, ok := ctx.Value(scopeContextKey{}).(*Scope)
	return scope, ok && scope != nil
}

// ReadFile selects the client filesystem when a scope is installed and the
// ordinary local filesystem otherwise.
func ReadFile(ctx context.Context, path string) ([]byte, error) {
	if scope, ok := FromContext(ctx); ok {
		return scope.ReadFile(ctx, path)
	}
	return os.ReadFile(path)
}

// Stat selects the client filesystem when a scope is installed and the
// ordinary local filesystem otherwise.
func Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	if scope, ok := FromContext(ctx); ok {
		return scope.Stat(ctx, path)
	}
	return os.Stat(path)
}

// WriteFile performs a revision-checked client write when scoped, falling back
// to os.WriteFile for TUI, CLI and standard ACP execution.
func WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	if scope, ok := FromContext(ctx); ok {
		return scope.WriteFile(ctx, path, data)
	}
	return os.WriteFile(path, data, perm)
}

// MkdirAll is a no-op for a client filesystem because crush/fs/write creates
// missing parents. Local execution preserves os.MkdirAll semantics.
func MkdirAll(ctx context.Context, path string, perm fs.FileMode) error {
	if scope, ok := FromContext(ctx); ok {
		_, err := scope.resolve(path)
		return err
	}
	return os.MkdirAll(path, perm)
}

// MetadataFor returns the latest revision metadata observed by this execution.
func MetadataFor(ctx context.Context, path string) (Metadata, bool) {
	scope, ok := FromContext(ctx)
	if !ok {
		return Metadata{}, false
	}
	return scope.Metadata(path)
}

type remoteFileInfo struct {
	name string
	meta Metadata
}

func (i remoteFileInfo) Name() string { return i.name }
func (i remoteFileInfo) Size() int64  { return i.meta.Size }
func (i remoteFileInfo) Mode() fs.FileMode {
	if i.meta.IsDir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i remoteFileInfo) ModTime() time.Time { return time.Time{} }
func (i remoteFileInfo) IsDir() bool        { return i.meta.IsDir }
func (i remoteFileInfo) Sys() any           { return nil }
