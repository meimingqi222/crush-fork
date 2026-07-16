package guiapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/blob"
	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/charmbracelet/crush/internal/turn"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestBlobHandlersHashRangesOwnershipIdempotencyAndCleanup(t *testing.T) {
	t.Parallel()

	service := NewService(nil)
	blobs := blob.New(blob.Config{})
	service.SetBlobService(blobs)
	service.SetSessionContentSources(fixedSessionReader{id: "session-1"}, nil, nil)
	recorder := &blobMetricRecorder{}
	ctx := guimetrics.WithRecorder(t.Context(), recorder)
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureBlob},
	})))
	t.Cleanup(service.Close)

	data := []byte("blob-payload")
	params := blobCreateParams{
		SessionID: "session-1", MIMEType: "application/octet-stream", Filename: "file.bin",
		Size: int64(len(data)), SHA256: sha256Hex(data),
		Chunks:          []string{base64.StdEncoding.EncodeToString(data[:4]), base64.StdEncoding.EncodeToString(data[4:])},
		ClientRequestID: uuid.NewString(),
	}
	created, rpcErr := service.HandleExtension(ctx, "crush/blob/create", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	metadata := created.(blobMetadata)
	require.Equal(t, params.SHA256, metadata.SHA256)
	require.Positive(t, metadata.ExpiresAt)

	replayed, rpcErr := service.HandleExtension(ctx, "crush/blob/create", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	require.Equal(t, metadata, replayed)
	params.Filename = "changed.bin"
	_, rpcErr = service.HandleExtension(ctx, "crush/blob/create", mustRawJSON(t, params))
	require.Equal(t, errorIdempotencyConflict, rpcErr.Message)

	read, rpcErr := service.HandleExtension(ctx, "crush/blob/read", mustRawJSON(t, blobReadParams{
		SessionID: "session-1", BlobID: metadata.BlobID, Offset: 2, Limit: 4,
	}))
	require.Nil(t, rpcErr)
	chunk := read.(blobReadResult)
	require.Equal(t, int64(6), chunk.NextOffset)
	require.False(t, chunk.EOF)
	decoded, err := base64.StdEncoding.DecodeString(chunk.Content)
	require.NoError(t, err)
	require.Equal(t, data[2:6], decoded)

	_, rpcErr = service.HandleExtension(ctx, "crush/blob/read", mustRawJSON(t, blobReadParams{
		SessionID: "session-2", BlobID: metadata.BlobID, Limit: 4,
	}))
	require.Equal(t, errorBlobNotFound, rpcErr.Message)

	release := blobReleaseParams{SessionID: "session-1", BlobID: metadata.BlobID, ClientRequestID: uuid.NewString()}
	firstRelease, rpcErr := service.HandleExtension(ctx, "crush/blob/release", mustRawJSON(t, release))
	require.Nil(t, rpcErr)
	secondRelease, rpcErr := service.HandleExtension(ctx, "crush/blob/release", mustRawJSON(t, release))
	require.Nil(t, rpcErr)
	require.Equal(t, firstRelease, secondRelease)
	count, retained := blobs.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)
	require.Contains(t, recorder.values(), int64(len(data)))
	require.Equal(t, int64(0), recorder.values()[len(recorder.values())-1])
}

func TestBlobValidationExpiryAndConnectionClose(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	blobs := blob.New(blob.Config{TTL: time.Second, MaxBlobBytes: 4, Clock: func() time.Time { return now }})
	service := NewService(nil)
	service.SetBlobService(blobs)
	service.SetSessionContentSources(fixedSessionReader{id: "session-1"}, nil, nil)
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureBlob},
	})))

	data := []byte("data")
	params := blobCreateParams{
		SessionID: "session-1", Size: int64(len(data)), SHA256: sha256Hex(data),
		Content: base64.StdEncoding.EncodeToString(data), ClientRequestID: uuid.NewString(),
	}
	created, rpcErr := service.HandleExtension(t.Context(), "crush/blob/create", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	metadata := created.(blobMetadata)

	bad := params
	bad.ClientRequestID = uuid.NewString()
	bad.SHA256 = sha256Hex([]byte("other"))
	_, rpcErr = service.HandleExtension(t.Context(), "crush/blob/create", mustRawJSON(t, bad))
	require.Equal(t, acpInvalidParamsCode(), rpcErr.Code)
	oversized := params
	oversized.Size = 5
	oversized.Content = strings.Repeat("!", 1024*1024)
	oversized.ClientRequestID = uuid.NewString()
	_, rpcErr = service.HandleExtension(t.Context(), "crush/blob/create", mustRawJSON(t, oversized))
	require.Equal(t, errorPayloadTooLarge, rpcErr.Message)

	now = now.Add(time.Second)
	_, rpcErr = service.HandleExtension(t.Context(), "crush/blob/read", mustRawJSON(t, blobReadParams{
		SessionID: "session-1", BlobID: metadata.BlobID, Limit: 4,
	}))
	require.Equal(t, errorBlobNotFound, rpcErr.Message)

	params.ClientRequestID = uuid.NewString()
	created, rpcErr = service.HandleExtension(t.Context(), "crush/blob/create", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	service.Close()
	count, retained := blobs.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)
	_, rpcErr = service.HandleExtension(t.Context(), "crush/blob/read", mustRawJSON(t, blobReadParams{}))
	require.NotNil(t, rpcErr)
}

func TestTurnStartResolvesOwnedBlobAttachment(t *testing.T) {
	env := newTurnHandlerEnvironment(t)
	require.Nil(t, env.service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureSessionControl, FeatureBlob},
	})))
	data := bytes.Repeat([]byte("attachment"), 128*1024)
	created, rpcErr := env.service.HandleExtension(t.Context(), "crush/blob/create", mustRawJSON(t, blobCreateParams{
		SessionID: env.sessionID, MIMEType: "application/pdf", Filename: "spec.pdf",
		Size: int64(len(data)), SHA256: sha256Hex(data), Content: base64.StdEncoding.EncodeToString(data),
		ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	metadata := created.(blobMetadata)

	result, rpcErr := env.service.HandleExtension(t.Context(), "crush/turn/start", mustRawJSON(t, turnStartParams{
		SessionID: env.sessionID, Content: []turnContentBlock{{Type: "blob", BlobID: metadata.BlobID}},
		ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	started := result.(turn.Turn)
	require.Eventually(t, func() bool { return len(env.runner.attachmentsFor(started.ID)) == 1 }, time.Second, time.Millisecond)
	attachment := env.runner.attachmentsFor(started.ID)[0]
	require.Equal(t, "spec.pdf", attachment.FileName)
	require.Equal(t, "application/pdf", attachment.MimeType)
	require.Equal(t, data, attachment.Content)
	env.runner.complete(started.ID, turn.StatusCompleted)
}

func TestBlobConnectionAndSessionDeletionCleanup(t *testing.T) {
	t.Parallel()

	first := NewService(nil)
	second := NewService(nil)
	shared := blob.New(blob.Config{})
	t.Cleanup(shared.Close)
	first.SetBlobService(shared)
	second.SetBlobService(shared)
	reader := fixedSessionReader{id: "session-1"}
	first.SetSessionContentSources(reader, nil, nil)
	second.SetSessionContentSources(reader, nil, nil)
	selection := experimentalSelection(t, Selection{ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureBlob}})
	require.Nil(t, first.NegotiateExperimental(selection))
	require.Nil(t, second.NegotiateExperimental(selection))
	data := []byte("private")
	created, rpcErr := first.HandleExtension(t.Context(), "crush/blob/create", mustRawJSON(t, blobCreateParams{
		SessionID: "session-1", Size: int64(len(data)), SHA256: sha256Hex(data),
		Content: base64.StdEncoding.EncodeToString(data), ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	metadata := created.(blobMetadata)
	_, rpcErr = second.HandleExtension(t.Context(), "crush/blob/read", mustRawJSON(t, blobReadParams{
		SessionID: "session-1", BlobID: metadata.BlobID, Limit: int64(len(data)),
	}))
	require.Equal(t, errorBlobNotFound, rpcErr.Message)
	first.Close()
	count, retained := shared.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)
	second.Close()

	env := newSessionMutationEnvironment(t)
	require.Nil(t, env.gui.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureBlob, FeatureSessionControl, FeatureSessionSync},
	})))
	owned := blob.New(blob.Config{})
	t.Cleanup(owned.Close)
	env.gui.SetBlobService(owned)
	created, rpcErr = env.gui.HandleExtension(t.Context(), "crush/blob/create", mustRawJSON(t, blobCreateParams{
		SessionID: env.source.ID, Size: int64(len(data)), SHA256: sha256Hex(data),
		Content: base64.StdEncoding.EncodeToString(data), ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	peer := NewService(nil)
	peer.SetBlobService(owned)
	peer.SetSessionContentSources(env.sessions, nil, nil)
	require.Nil(t, peer.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureBlob},
	})))
	t.Cleanup(peer.Close)
	_, rpcErr = peer.HandleExtension(t.Context(), "crush/blob/create", mustRawJSON(t, blobCreateParams{
		SessionID: env.source.ID, Size: int64(len(data)), SHA256: sha256Hex(data),
		Content: base64.StdEncoding.EncodeToString(data), ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	count, _ = owned.Retained()
	require.Equal(t, 2, count)
	_, rpcErr = env.gui.HandleExtension(t.Context(), "crush/session/delete", mustRawJSON(t, sessionDeleteParams{
		SessionID: env.source.ID, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	count, retained = owned.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)
}

type blobMetricRecorder struct {
	mu     sync.Mutex
	gauges []int64
}

func (*blobMetricRecorder) ObserveDuration(guimetrics.Name, time.Duration, guimetrics.Labels) {}
func (*blobMetricRecorder) Add(guimetrics.Name, int64, guimetrics.Labels)                     {}
func (r *blobMetricRecorder) SetGauge(name guimetrics.Name, value int64, _ guimetrics.Labels) {
	if name != guimetrics.BlobRetainedBytes {
		return
	}
	r.mu.Lock()
	r.gauges = append(r.gauges, value)
	r.mu.Unlock()
}

func (r *blobMetricRecorder) values() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.gauges...)
}

func acpInvalidParamsCode() int { return -32602 }

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
