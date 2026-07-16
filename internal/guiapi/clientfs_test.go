package guiapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/charmbracelet/crush/internal/blob"
	"github.com/charmbracelet/crush/internal/clientfs"
	"github.com/stretchr/testify/require"
)

func TestClientFSScopeNegotiationIsolationBlobAndLifecycle(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "buffer.txt")
	require.NoError(t, os.WriteFile(path, []byte("disk"), 0o600))
	service := NewService(nil)
	caller := &guiClientFSCaller{service: service, path: path, revision: "buffer:1", sourceURI: "untitled:///buffer.txt"}
	service.SetClientFSCaller(caller)

	require.Nil(t, service.ClientFSForSession("session-1", root))
	require.Nil(t, service.ClientFSForSession("session-2", root))
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureClientFS, FeatureBlob},
	})))
	first := service.ClientFSForSession("session-1", root)
	second := service.ClientFSForSession("session-2", root)
	require.NotNil(t, first)
	require.NotNil(t, second)
	require.NotSame(t, first, second)

	data, err := first.ReadFile(t.Context(), path)
	require.NoError(t, err)
	require.Len(t, data, 128*1024)
	metadata, ok := first.Metadata(path)
	require.True(t, ok)
	require.Equal(t, caller.sourceURI, metadata.SourceURI)
	require.Equal(t, "buffer:1", metadata.Revision)
	_, err = second.ReadFile(t.Context(), path)
	require.Error(t, err, "a client FS Blob must remain bound to its root session")

	require.NoError(t, first.WriteFile(t.Context(), path, make([]byte, 96*1024)))
	require.True(t, caller.sawWriteBlob())
	count, _ := service.blobService().Retained()
	require.Equal(t, 1, count, "read Blob remains client-owned until connection cleanup")

	service.releaseSessionClientFS("session-1")
	_, err = first.ReadFile(t.Context(), path)
	require.ErrorIs(t, err, clientfs.ErrClosed)
	_, err = second.Stat(t.Context(), path)
	require.NoError(t, err)

	require.Nil(t, service.NegotiateExperimental(nil))
	_, err = second.Stat(t.Context(), path)
	require.ErrorIs(t, err, clientfs.ErrClosed)
	service.Close()
	count, retained := service.blobService().Retained()
	require.Zero(t, count)
	require.Zero(t, retained)
}

type guiClientFSCaller struct {
	mu           sync.Mutex
	service      *Service
	path         string
	revision     string
	sourceURI    string
	readBlobID   string
	writeBlob    bool
	writeContent []byte
}

func (c *guiClientFSCaller) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case "crush/fs/read":
		if c.readBlobID == "" {
			data := make([]byte, 128*1024)
			sum := sha256.Sum256(data)
			metadata, err := c.service.blobService().Create(ctx, c.service.blobOwnerID(), blob.CreateInput{
				SessionID: "session-1", MIMEType: "text/plain", Filename: "buffer.txt",
				SourceURI: c.sourceURI, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Data: data,
			})
			if err != nil {
				return nil, err
			}
			c.readBlobID = metadata.ID
		}
		return json.Marshal(map[string]any{
			"path": c.path, "sourceUri": c.sourceURI, "revision": c.revision,
			"size": 128 * 1024, "exists": true, "blobId": c.readBlobID,
		})
	case "crush/fs/stat":
		return json.Marshal(map[string]any{
			"path": c.path, "sourceUri": c.sourceURI, "revision": c.revision,
			"size": 128 * 1024, "exists": true,
		})
	case "crush/fs/write":
		payload, _ := json.Marshal(params)
		var input struct {
			SessionID        string `json:"sessionId"`
			ExpectedRevision string `json:"expectedRevision"`
			BlobID           string `json:"blobId"`
		}
		_ = json.Unmarshal(payload, &input)
		if input.ExpectedRevision != c.revision {
			return nil, guiClientFSError("CRUSH_REVISION_CONFLICT")
		}
		if input.BlobID == "" {
			return nil, errors.New("expected Blob write")
		}
		_, data, err := c.service.blobService().Resolve(ctx, c.service.blobOwnerID(), input.SessionID, input.BlobID)
		if err != nil {
			return nil, err
		}
		c.writeBlob = true
		c.writeContent = data
		c.revision = "buffer:2"
		return json.Marshal(map[string]any{
			"path": c.path, "sourceUri": c.sourceURI, "revision": c.revision,
			"size": len(data), "exists": true,
		})
	default:
		return nil, errors.New("unexpected method")
	}
}

func (c *guiClientFSCaller) sawWriteBlob() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeBlob && len(c.writeContent) == 96*1024
}

type guiClientFSError string

func (e guiClientFSError) Error() string        { return string(e) }
func (e guiClientFSError) ClientFSCode() string { return string(e) }
