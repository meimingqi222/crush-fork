package guiapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/blob"
	"github.com/charmbracelet/crush/internal/clientfs"
)

type clientFSScope struct {
	workspace string
	scope     *clientfs.Scope
}

// SetClientFSCaller attaches the reverse-call transport for revision-aware
// Agent-to-GUI filesystem requests.
func (s *Service) SetClientFSCaller(caller clientfs.Caller) {
	s.mu.Lock()
	previous := make([]*clientfs.Scope, 0, len(s.clientFS))
	for _, entry := range s.clientFS {
		previous = append(previous, entry.scope)
	}
	clear(s.clientFS)
	s.clientFSCaller = caller
	s.mu.Unlock()
	for _, scope := range previous {
		scope.Close()
	}
}

// ClientFSForSession returns the connection-owned capability for one root
// session only when the private clientFS feature was negotiated.
func (s *Service) ClientFSForSession(sessionID, workspace string) *clientfs.Scope {
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if sessionID == "" || workspace == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.negotiated {
		return nil
	}
	if _, ok := s.features[FeatureClientFS]; !ok || s.clientFSCaller == nil {
		return nil
	}
	if current, ok := s.clientFS[sessionID]; ok && sameWorkspace(current.workspace, workspace) {
		return current.scope
	} else if ok {
		current.scope.Close()
		delete(s.clientFS, sessionID)
	}
	scope, err := clientfs.New(clientfs.Config{
		SessionID: sessionID, Workspace: workspace, Caller: s.clientFSCaller,
		Blobs: clientFSBlobBridge{service: s},
	})
	if err != nil {
		return nil
	}
	s.clientFS[sessionID] = clientFSScope{workspace: workspace, scope: scope}
	return scope
}

func (s *Service) releaseSessionClientFS(sessionID string) {
	s.mu.Lock()
	entry, ok := s.clientFS[sessionID]
	if ok {
		delete(s.clientFS, sessionID)
	}
	s.mu.Unlock()
	if ok {
		entry.scope.Close()
	}
}

type clientFSBlobBridge struct {
	service *Service
}

func (b clientFSBlobBridge) Resolve(ctx context.Context, sessionID, blobID string) ([]byte, error) {
	service := b.service.blobService()
	if service == nil {
		return nil, blob.ErrClosed
	}
	_, data, err := service.Resolve(ctx, b.service.blobOwnerID(), sessionID, blobID)
	return data, err
}

func (b clientFSBlobBridge) Publish(ctx context.Context, sessionID, sourceURI string, data []byte) (string, func(), error) {
	service := b.service.blobService()
	if service == nil {
		return "", nil, blob.ErrClosed
	}
	sum := sha256.Sum256(data)
	metadata, err := service.Create(ctx, b.service.blobOwnerID(), blob.CreateInput{
		SessionID: sessionID, MIMEType: "application/octet-stream",
		Filename: filepath.Base(sourceURI), SourceURI: sourceURI, Size: int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]), Data: data,
	})
	if err != nil {
		return "", nil, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() { _ = service.Release(b.service.blobOwnerID(), sessionID, metadata.ID) })
	}
	return metadata.ID, release, nil
}

func sameWorkspace(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
