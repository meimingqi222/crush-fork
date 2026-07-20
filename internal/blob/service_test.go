package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCreateReadReleaseAndOwnership(t *testing.T) {
	t.Parallel()

	data := []byte("0123456789")
	service := New(Config{MaxReadBytes: 4})
	metadata, err := service.Create(t.Context(), "client-a", createInput("session-a", data))
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), metadata.Size)

	read, err := service.Read(t.Context(), "client-a", "session-a", metadata.ID, 2, 4)
	require.NoError(t, err)
	require.Equal(t, []byte("2345"), read.Data)
	require.Equal(t, int64(6), read.NextOffset)
	require.False(t, read.EOF)

	_, err = service.Read(t.Context(), "client-a", "session-b", metadata.ID, 0, 4)
	require.ErrorIs(t, err, ErrOwnerMismatch)
	_, err = service.Read(t.Context(), "client-b", "session-a", metadata.ID, 0, 4)
	require.ErrorIs(t, err, ErrOwnerMismatch)
	require.ErrorIs(t, service.Release("client-a", "session-b", metadata.ID), ErrOwnerMismatch)
	require.NoError(t, service.Release("client-a", "session-a", metadata.ID))
	_, err = service.Read(t.Context(), "client-a", "session-a", metadata.ID, 0, 4)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestValidationExpiryCapacityAndCleanup(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	service := New(Config{
		TTL: time.Minute, MaxBlobBytes: 4, MaxRetainedBytes: 6, MaxBlobs: 2,
		Clock: func() time.Time { return now },
	})
	input := createInput("session-a", []byte("1234"))
	_, err := service.Create(t.Context(), "client-a", input)
	require.NoError(t, err)

	oversized := createInput("session-a", []byte("12345"))
	_, err = service.Create(t.Context(), "client-a", oversized)
	require.ErrorIs(t, err, ErrBlobTooLarge)
	badHash := createInput("session-a", []byte("12"))
	badHash.SHA256 = createInput("session-a", []byte("other")).SHA256
	_, err = service.Create(t.Context(), "client-a", badHash)
	require.ErrorIs(t, err, ErrHashMismatch)
	second := createInput("session-b", []byte("123"))
	_, err = service.Create(t.Context(), "client-b", second)
	require.ErrorIs(t, err, ErrCapacity)

	now = now.Add(time.Minute)
	count, retained := service.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)

	metadata, err := service.Create(t.Context(), "client-a", createInput("session-a", []byte("12")))
	require.NoError(t, err)
	_, err = service.Read(t.Context(), "client-a", "session-a", metadata.ID, -1, 1)
	require.ErrorIs(t, err, ErrInvalidRange)
	service.ReleaseSession("session-a")
	count, retained = service.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)
	service.Close()
	require.ErrorIs(t, service.Release("client-a", "session-a", metadata.ID), ErrClosed)
}

func TestConcurrentReadsAndReleaseAreRaceFree(t *testing.T) {
	t.Parallel()

	service := New(Config{})
	metadata, err := service.Create(t.Context(), "client", createInput("session", []byte("payload")))
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = service.Read(t.Context(), "client", "session", metadata.ID, 0, 7)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = service.Release("client", "session", metadata.ID)
	}()
	wg.Wait()
	count, retained := service.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)
}

func TestUploadReservesCapacityAndCommitsSameHandle(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	data := []byte("segmented payload")
	service := New(Config{TTL: time.Minute, MaxBlobBytes: 32, MaxRetainedBytes: 32, MaxBlobs: 1, MaxUploadChunk: 8, Clock: func() time.Time { return now }})
	input := createInput("session-a", data)
	input.Data = nil
	started, err := service.StartUpload(t.Context(), "client-a", input)
	require.NoError(t, err)
	require.Zero(t, started.NextOffset)
	count, retained := service.Retained()
	require.Equal(t, 1, count)
	require.Equal(t, int64(len(data)), retained)

	_, err = service.Read(t.Context(), "client-a", "session-a", started.UploadID, 0, 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = service.AppendUpload(t.Context(), "client-a", "session-a", started.UploadID, 1, data[:1])
	require.ErrorIs(t, err, ErrUploadOffset)
	chunk, err := service.AppendUpload(t.Context(), "client-a", "session-a", started.UploadID, 0, data[:8])
	require.NoError(t, err)
	require.Equal(t, int64(8), chunk.NextOffset)
	_, err = service.AppendUpload(t.Context(), "client-a", "session-a", started.UploadID, 8, data[8:])
	require.ErrorIs(t, err, ErrChunkTooLarge)
	_, err = service.AppendUpload(t.Context(), "client-a", "session-a", started.UploadID, 8, data[8:16])
	require.NoError(t, err)
	_, err = service.AppendUpload(t.Context(), "client-a", "session-a", started.UploadID, 16, data[16:])
	require.NoError(t, err)
	metadata, err := service.CommitUpload(t.Context(), "client-a", "session-a", started.UploadID)
	require.NoError(t, err)
	require.Equal(t, started.UploadID, metadata.ID)
	read, err := service.Read(t.Context(), "client-a", "session-a", metadata.ID, 0, int64(len(data)))
	require.NoError(t, err)
	require.Equal(t, data, read.Data)
}

func TestUploadOwnershipAbortExpiryAndCapacity(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	data := []byte("1234")
	service := New(Config{TTL: time.Second, MaxBlobBytes: 4, MaxRetainedBytes: 4, MaxBlobs: 1, Clock: func() time.Time { return now }})
	input := createInput("session-a", data)
	input.Data = nil
	started, err := service.StartUpload(t.Context(), "client-a", input)
	require.NoError(t, err)
	_, err = service.StartUpload(t.Context(), "client-b", input)
	require.ErrorIs(t, err, ErrCapacity)
	require.ErrorIs(t, service.AbortUpload("client-b", "session-a", started.UploadID), ErrOwnerMismatch)
	require.NoError(t, service.AbortUpload("client-a", "session-a", started.UploadID))
	count, retained := service.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)

	started, err = service.StartUpload(t.Context(), "client-a", input)
	require.NoError(t, err)
	now = now.Add(time.Second)
	_, err = service.CommitUpload(t.Context(), "client-a", "session-a", started.UploadID)
	require.ErrorIs(t, err, ErrNotFound)
	count, retained = service.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)
}

func TestUploadRejectsEmptyChunksAndUppercaseHashes(t *testing.T) {
	t.Parallel()

	data := []byte("payload")
	service := New(Config{})
	input := createInput("session-a", data)
	uppercase := input
	uppercase.SHA256 = strings.ToUpper(uppercase.SHA256)
	_, err := service.Create(t.Context(), "client-a", uppercase)
	require.ErrorIs(t, err, ErrInvalidInput)
	uppercase.Data = nil
	_, err = service.StartUpload(t.Context(), "client-a", uppercase)
	require.ErrorIs(t, err, ErrInvalidInput)

	input.Data = nil
	started, err := service.StartUpload(t.Context(), "client-a", input)
	require.NoError(t, err)
	_, err = service.AppendUpload(t.Context(), "client-a", "session-a", started.UploadID, 0, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestUploadAcceptsExact64MiBReservationWithoutPreallocation(t *testing.T) {
	t.Parallel()

	const maxBlob = 64 * 1024 * 1024
	service := New(Config{MaxBlobBytes: maxBlob, MaxRetainedBytes: maxBlob, MaxBlobs: 1})
	input := CreateInput{SessionID: "session-a", Size: maxBlob, SHA256: strings.Repeat("0", 64)}
	started, err := service.StartUpload(t.Context(), "client-a", input)
	require.NoError(t, err)
	require.Equal(t, int64(maxBlob), started.Size)
	count, retained := service.Retained()
	require.Equal(t, 1, count)
	require.Equal(t, int64(maxBlob), retained)
	require.NoError(t, service.AbortUpload("client-a", "session-a", started.UploadID))
	count, retained = service.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)

	input.Size++
	_, err = service.StartUpload(t.Context(), "client-a", input)
	require.ErrorIs(t, err, ErrBlobTooLarge)
}

func createInput(sessionID string, data []byte) CreateInput {
	sum := sha256.Sum256(data)
	return CreateInput{
		SessionID: sessionID, MIMEType: "application/octet-stream", Filename: "file.bin",
		Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Data: data,
	}
}
