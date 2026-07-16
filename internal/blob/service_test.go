package blob

import (
	"crypto/sha256"
	"encoding/hex"
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

func createInput(sessionID string, data []byte) CreateInput {
	sum := sha256.Sum256(data)
	return CreateInput{
		SessionID: sessionID, MIMEType: "application/octet-stream", Filename: "file.bin",
		Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Data: data,
	}
}
