package clientfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolvePathConfinesExistingAndNewTargets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600))

	inside, err := ResolvePath(root, filepath.Join("new", "file.txt"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "new", "file.txt"), inside)

	_, err = ResolvePath(root, filepath.Join("..", filepath.Base(outside), "secret.txt"))
	require.ErrorIs(t, err, ErrPathEscape)
	_, err = ResolvePath(root, filepath.Join(outside, "secret.txt"))
	require.ErrorIs(t, err, ErrPathEscape)
}

func TestResolvePathRejectsSymlinkOrJunctionEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if runtime.GOOS == "windows" {
		if err := createJunction(link, outside); err != nil {
			t.Skipf("Junctions unavailable: %v", err)
		}
	} else if err := os.Symlink(outside, link); err != nil {
		t.Skipf("Symlinks unavailable: %v", err)
	}

	_, err := ResolvePath(root, filepath.Join(link, "existing-or-new.txt"))
	require.ErrorIs(t, err, ErrPathEscape)
}

func TestScopeReadsUnsavedContentPreservesURIAndRejectsStaleWrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("durable"), 0o600))
	client := newFakeClient(path, "unsaved", "buffer:7", "untitled://editor/main.go")
	scope, err := New(Config{SessionID: "session-1", Workspace: root, Caller: client})
	require.NoError(t, err)

	content, err := scope.ReadFile(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, "unsaved", string(content))
	metadata, ok := scope.Metadata(path)
	require.True(t, ok)
	require.Equal(t, "buffer:7", metadata.Revision)
	require.Equal(t, "untitled://editor/main.go", metadata.SourceURI)

	client.replace("user changed", "buffer:8")
	err = scope.WriteFile(t.Context(), path, []byte("agent changed"))
	require.ErrorIs(t, err, ErrRevisionConflict)
	require.Equal(t, "user changed", client.content())
}

func TestScopeWriteUsesObservedRevisionAndTemporaryBlob(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "large.txt")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	client := newFakeClient(path, "old", "disk:1", "file:///workspace/large.txt")
	blobs := newFakeBlobs()
	scope, err := New(Config{
		SessionID: "session-1", Workspace: root, Caller: client, Blobs: blobs,
		InlineBytes: 4, MaxFileBytes: 1024,
	})
	require.NoError(t, err)
	_, err = scope.ReadFile(t.Context(), path)
	require.NoError(t, err)

	err = scope.WriteFile(t.Context(), path, []byte("new content"))
	require.NoError(t, err)
	require.Equal(t, "new content", client.content())
	require.Equal(t, "disk:1", client.lastExpected())
	require.Equal(t, 0, blobs.retained())
	metadata, ok := scope.Metadata(path)
	require.True(t, ok)
	require.Equal(t, "client:2", metadata.Revision)
}

func TestScopeCreatesNewFileWithMissingRevisionToken(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "nested", "new.txt")
	client := &missingFileClient{path: path}
	scope, err := New(Config{SessionID: "session-1", Workspace: root, Caller: client})
	require.NoError(t, err)

	err = scope.WriteFile(t.Context(), path, []byte("created"))
	require.NoError(t, err)
	require.Equal(t, "missing:1", client.expected)
	require.NotEmpty(t, client.requestID)
	metadata, ok := scope.Metadata(path)
	require.True(t, ok)
	require.Equal(t, "file:1", metadata.Revision)
}

func TestScopeLifecycleLimitsAndReturnedPathValidation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	client := newFakeClient(path, "oversized", "r1", "file:///main.go")
	scope, err := New(Config{SessionID: "s", Workspace: root, Caller: client, MaxFileBytes: 4})
	require.NoError(t, err)
	_, err = scope.ReadFile(t.Context(), path)
	require.ErrorIs(t, err, ErrInvalidResponse)

	client.mu.Lock()
	client.returnedPath = filepath.Join(t.TempDir(), "other.go")
	client.value = "x"
	client.revision = "r2"
	client.mu.Unlock()
	_, err = scope.ReadFile(t.Context(), path)
	require.ErrorIs(t, err, ErrInvalidResponse)

	scope.Close()
	_, err = scope.ReadFile(t.Context(), path)
	require.ErrorIs(t, err, ErrClosed)
}

func TestScopeConcurrentReadsAndWritesAreRaceFree(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("0"), 0o600))
	client := newFakeClient(path, "0", "client:0", "file:///main.go")
	scope, err := New(Config{SessionID: "s", Workspace: root, Caller: client})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for index := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = scope.ReadFile(context.Background(), path)
			_ = scope.WriteFile(context.Background(), path, []byte(fmt.Sprint(index)))
		}()
	}
	wg.Wait()
}

type fakeRPCError string

func (e fakeRPCError) Error() string        { return string(e) }
func (e fakeRPCError) ClientFSCode() string { return string(e) }

type missingFileClient struct {
	path      string
	expected  string
	requestID string
}

func (c *missingFileClient) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	switch method {
	case "crush/fs/stat":
		return json.Marshal(wireFile{Path: c.path, Revision: "missing:1", Exists: false})
	case "crush/fs/write":
		payload, _ := json.Marshal(params)
		var input writeParams
		_ = json.Unmarshal(payload, &input)
		c.expected = input.ExpectedRevision
		c.requestID = input.ClientRequestID
		return json.Marshal(wireFile{
			Path: c.path, SourceURI: "file:///nested/new.txt", Revision: "file:1",
			Size: int64(len(input.Content)), Exists: true,
		})
	default:
		return nil, errors.New("unexpected method")
	}
}

type fakeClient struct {
	mu           sync.Mutex
	path         string
	returnedPath string
	value        string
	revision     string
	sourceURI    string
	lastRevision string
}

func newFakeClient(path, value, revision, sourceURI string) *fakeClient {
	return &fakeClient{path: path, returnedPath: path, value: value, revision: revision, sourceURI: sourceURI}
}

func (c *fakeClient) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case "crush/fs/read":
		return json.Marshal(wireFile{
			Path: c.returnedPath, SourceURI: c.sourceURI, Revision: c.revision,
			Size: int64(len(c.value)), Exists: true, Content: c.value,
		})
	case "crush/fs/stat":
		return json.Marshal(wireFile{
			Path: c.returnedPath, SourceURI: c.sourceURI, Revision: c.revision,
			Size: int64(len(c.value)), Exists: true,
		})
	case "crush/fs/write":
		payload, _ := json.Marshal(params)
		var input writeParams
		_ = json.Unmarshal(payload, &input)
		c.lastRevision = input.ExpectedRevision
		if input.ExpectedRevision != c.revision {
			return nil, fakeRPCError("CRUSH_REVISION_CONFLICT")
		}
		if input.BlobID != "" {
			c.value = fakeBlobRegistry.resolve(input.BlobID)
		} else {
			c.value = input.Content
		}
		c.revision = "client:2"
		return json.Marshal(wireFile{
			Path: c.path, SourceURI: c.sourceURI, Revision: c.revision,
			Size: int64(len(c.value)), Exists: true,
		})
	default:
		return nil, errors.New("unexpected method")
	}
}

func (c *fakeClient) replace(value, revision string) {
	c.mu.Lock()
	c.value = value
	c.revision = revision
	c.mu.Unlock()
}

func (c *fakeClient) content() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *fakeClient) lastExpected() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRevision
}

var fakeBlobRegistry = &fakeBlobStore{values: make(map[string]string)}

type fakeBlobs struct {
	mu     sync.Mutex
	values map[string][]byte
}

type fakeBlobStore struct {
	mu     sync.Mutex
	values map[string]string
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{values: make(map[string][]byte)} }

func (b *fakeBlobs) Resolve(_ context.Context, _, id string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.values[id]
	if !ok {
		return nil, errors.New("missing blob")
	}
	return append([]byte(nil), value...), nil
}

func (b *fakeBlobs) Publish(_ context.Context, _, _ string, data []byte) (string, func(), error) {
	b.mu.Lock()
	id := fmt.Sprintf("blob-%d", len(b.values)+1)
	b.values[id] = append([]byte(nil), data...)
	b.mu.Unlock()
	fakeBlobRegistry.mu.Lock()
	fakeBlobRegistry.values[id] = string(data)
	fakeBlobRegistry.mu.Unlock()
	return id, func() {
		b.mu.Lock()
		delete(b.values, id)
		b.mu.Unlock()
		fakeBlobRegistry.mu.Lock()
		delete(fakeBlobRegistry.values, id)
		fakeBlobRegistry.mu.Unlock()
	}, nil
}

func (b *fakeBlobs) retained() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.values)
}

func (b *fakeBlobStore) resolve(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.values[id]
}
