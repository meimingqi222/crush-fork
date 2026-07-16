package clientfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
)

type readParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
}

type writeParams struct {
	SessionID        string `json:"sessionId"`
	Path             string `json:"path"`
	ExpectedRevision string `json:"expectedRevision"`
	Content          string `json:"content,omitempty"`
	BlobID           string `json:"blobId,omitempty"`
	ClientRequestID  string `json:"clientRequestId"`
}

type statParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
}

type wireFile struct {
	Path      string `json:"path,omitempty"`
	SourceURI string `json:"sourceUri,omitempty"`
	Revision  string `json:"revision"`
	MIMEType  string `json:"mimeType,omitempty"`
	Size      int64  `json:"size"`
	Exists    bool   `json:"exists"`
	IsDir     bool   `json:"isDirectory,omitempty"`
	Content   string `json:"content,omitempty"`
	BlobID    string `json:"blobId,omitempty"`
}

// New creates one connection/root-session capability.
func New(config Config) (*Scope, error) {
	if strings.TrimSpace(config.SessionID) == "" || config.Caller == nil {
		return nil, errors.New("clientfs: session and caller are required")
	}
	workspace, err := ResolvePath(config.Workspace, ".")
	if err != nil {
		return nil, err
	}
	if config.MaxFileBytes <= 0 {
		config.MaxFileBytes = defaultMaxFileBytes
	}
	if config.InlineBytes <= 0 {
		config.InlineBytes = defaultInlineBytes
	}
	if config.InlineBytes > config.MaxFileBytes {
		config.InlineBytes = config.MaxFileBytes
	}
	return &Scope{
		config: config, workspace: workspace, metadata: make(map[string]Metadata),
		locks: make(map[string]*sync.Mutex),
	}, nil
}

// ReadFile reads the client's current buffer and records its revision for a
// later compare-and-swap write.
func (s *Scope) ReadFile(ctx context.Context, requested string) ([]byte, error) {
	path, err := s.resolve(requested)
	if err != nil {
		return nil, err
	}
	raw, err := s.call(ctx, "crush/fs/read", readParams{SessionID: s.config.SessionID, Path: path})
	if err != nil {
		return nil, mapRemoteError(err)
	}
	value, err := s.decodeFile(ctx, raw, path, true)
	if err != nil {
		return nil, err
	}
	if !value.meta.Exists || value.meta.IsDir {
		return nil, ErrNotFound
	}
	s.remember(path, value.meta)
	return value.data, nil
}

// Stat observes the current revision even when a file does not yet exist.
func (s *Scope) Stat(ctx context.Context, requested string) (fs.FileInfo, error) {
	path, err := s.resolve(requested)
	if err != nil {
		return nil, err
	}
	raw, err := s.call(ctx, "crush/fs/stat", statParams{SessionID: s.config.SessionID, Path: path})
	if err != nil {
		return nil, mapRemoteError(err)
	}
	value, err := s.decodeFile(ctx, raw, path, false)
	if err != nil {
		return nil, err
	}
	s.remember(path, value.meta)
	if !value.meta.Exists {
		return nil, ErrNotFound
	}
	return remoteFileInfo{name: filepath.Base(path), meta: value.meta}, nil
}

// WriteFile applies a compare-and-swap against the revision captured by the
// preceding read/stat. If no observation exists, Stat establishes the missing
// or durable-file token first.
func (s *Scope) WriteFile(ctx context.Context, requested string, data []byte) error {
	path, err := s.resolve(requested)
	if err != nil {
		return err
	}
	if int64(len(data)) > s.config.MaxFileBytes {
		return ErrPayloadTooLarge
	}
	unlock := s.lock(path)
	defer unlock()
	meta, ok := s.Metadata(path)
	if !ok {
		_, statErr := s.Stat(ctx, path)
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return statErr
		}
		meta, ok = s.Metadata(path)
	}
	if !ok || meta.Revision == "" {
		return ErrInvalidResponse
	}
	params := writeParams{
		SessionID: s.config.SessionID, Path: path, ExpectedRevision: meta.Revision,
		ClientRequestID: uuid.NewString(),
	}
	var release func()
	if int64(len(data)) > s.config.InlineBytes || !utf8.Valid(data) {
		if s.config.Blobs == nil {
			return ErrPayloadTooLarge
		}
		params.BlobID, release, err = s.config.Blobs.Publish(ctx, s.config.SessionID, meta.SourceURI, data)
		if err != nil {
			return err
		}
		defer release()
	} else {
		params.Content = string(data)
	}
	raw, err := s.call(ctx, "crush/fs/write", params)
	if err != nil {
		return mapRemoteError(err)
	}
	value, err := s.decodeFile(ctx, raw, path, false)
	if err != nil {
		return err
	}
	if !value.meta.Exists || value.meta.IsDir {
		return ErrInvalidResponse
	}
	s.remember(path, value.meta)
	return nil
}

// Metadata returns a copy of the latest observed state for path.
func (s *Scope) Metadata(requested string) (Metadata, bool) {
	path, err := s.resolve(requested)
	if err != nil {
		return Metadata{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, ok := s.metadata[path]
	return meta, ok
}

// Close revokes the connection-owned capability and its revision cache.
func (s *Scope) Close() {
	s.mu.Lock()
	s.closed = true
	clear(s.metadata)
	clear(s.locks)
	s.mu.Unlock()
}

type decodedFile struct {
	meta Metadata
	data []byte
}

func (s *Scope) decodeFile(ctx context.Context, raw json.RawMessage, requested string, content bool) (decodedFile, error) {
	var wire wireFile
	if err := json.Unmarshal(raw, &wire); err != nil {
		return decodedFile{}, ErrInvalidResponse
	}
	if !wire.Exists && wire.Size != 0 {
		return decodedFile{}, ErrInvalidResponse
	}
	if len(wire.Revision) == 0 || len(wire.Revision) > maxRevisionBytes ||
		!utf8.ValidString(wire.Revision) || strings.ContainsRune(wire.Revision, '\x00') ||
		len(wire.SourceURI) > maxSourceURIBytes || !utf8.ValidString(wire.SourceURI) ||
		strings.ContainsRune(wire.SourceURI, '\x00') || len(wire.MIMEType) > 2048 ||
		!utf8.ValidString(wire.MIMEType) || strings.ContainsRune(wire.MIMEType, '\x00') ||
		len(wire.BlobID) > 2048 ||
		wire.Size < 0 || wire.Size > s.config.MaxFileBytes || wire.IsDir && !wire.Exists {
		return decodedFile{}, ErrInvalidResponse
	}
	if wire.Path != "" {
		returned, err := s.resolve(wire.Path)
		if err != nil || !samePath(returned, requested) {
			return decodedFile{}, ErrInvalidResponse
		}
	}
	meta := Metadata{
		Path: requested, SourceURI: wire.SourceURI, Revision: wire.Revision,
		MIMEType: wire.MIMEType, Size: wire.Size, Exists: wire.Exists, IsDir: wire.IsDir,
	}
	if !content || !wire.Exists || wire.IsDir {
		if wire.Content != "" || wire.BlobID != "" {
			return decodedFile{}, ErrInvalidResponse
		}
		return decodedFile{meta: meta}, nil
	}
	if wire.Content != "" && wire.BlobID != "" {
		return decodedFile{}, ErrInvalidResponse
	}
	if wire.Size > 0 && wire.Content == "" && wire.BlobID == "" {
		return decodedFile{}, ErrInvalidResponse
	}
	if int64(len(wire.Content)) > s.config.InlineBytes {
		return decodedFile{}, ErrInvalidResponse
	}
	if wire.Size == 0 && (wire.Content != "" || wire.BlobID != "") {
		return decodedFile{}, ErrInvalidResponse
	}
	var data []byte
	if wire.BlobID != "" {
		if s.config.Blobs == nil {
			return decodedFile{}, ErrInvalidResponse
		}
		resolved, err := s.config.Blobs.Resolve(ctx, s.config.SessionID, wire.BlobID)
		if err != nil {
			return decodedFile{}, err
		}
		data = resolved
	} else {
		data = []byte(wire.Content)
	}
	if int64(len(data)) != wire.Size {
		clear(data)
		return decodedFile{}, ErrInvalidResponse
	}
	return decodedFile{meta: meta, data: data}, nil
}

func (s *Scope) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.RLock()
	closed := s.closed
	caller := s.config.Caller
	s.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	return caller.Call(ctx, method, params)
}

func (s *Scope) resolve(path string) (string, error) {
	s.mu.RLock()
	closed := s.closed
	workspace := s.workspace
	s.mu.RUnlock()
	if closed {
		return "", ErrClosed
	}
	return ResolvePath(workspace, path)
}

func (s *Scope) remember(path string, meta Metadata) {
	s.mu.Lock()
	if !s.closed {
		s.metadata[path] = meta
	}
	s.mu.Unlock()
}

func (s *Scope) lock(path string) func() {
	s.mu.Lock()
	mu := s.locks[path]
	if mu == nil {
		mu = &sync.Mutex{}
		s.locks[path] = mu
	}
	s.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

type codedError interface {
	ClientFSCode() string
}

func mapRemoteError(err error) error {
	if err == nil {
		return nil
	}
	var coded codedError
	if errors.As(err, &coded) {
		switch coded.ClientFSCode() {
		case "CRUSH_REVISION_CONFLICT":
			return fmt.Errorf("%w: remote revision changed", ErrRevisionConflict)
		case "CRUSH_FS_NOT_FOUND":
			return ErrNotFound
		case "CRUSH_INVALID_PATH":
			return ErrInvalidPath
		case "CRUSH_PAYLOAD_TOO_LARGE":
			return ErrPayloadTooLarge
		}
	}
	return err
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
