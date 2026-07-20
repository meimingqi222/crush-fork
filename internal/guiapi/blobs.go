package guiapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/blob"
	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/charmbracelet/crush/internal/idempotency"
	"github.com/charmbracelet/crush/internal/message"
)

const (
	errorBlobNotFound    = "CRUSH_BLOB_NOT_FOUND"
	errorPayloadTooLarge = "CRUSH_PAYLOAD_TOO_LARGE"
	maxInlineBinaryBytes = 4 * 1024 * 1024
	maxBlobChunks        = 4096
	maxAtomicBlobBytes   = 1024 * 1024
	maxUploadChunkBytes  = 1024 * 1024
)

type blobCreateParams struct {
	SessionID       string   `json:"sessionId"`
	MIMEType        string   `json:"mimeType,omitempty"`
	Filename        string   `json:"filename,omitempty"`
	SourceURI       string   `json:"sourceUri,omitempty"`
	Size            int64    `json:"size"`
	SHA256          string   `json:"sha256"`
	Content         string   `json:"content,omitempty"`
	Chunks          []string `json:"chunks,omitempty"`
	ClientRequestID string   `json:"clientRequestId"`
}

type blobReadParams struct {
	SessionID string `json:"sessionId"`
	BlobID    string `json:"blobId"`
	Offset    int64  `json:"offset"`
	Limit     int64  `json:"limit"`
}

type blobReleaseParams struct {
	SessionID       string `json:"sessionId"`
	BlobID          string `json:"blobId"`
	ClientRequestID string `json:"clientRequestId"`
}

type blobUploadStartParams struct {
	SessionID       string `json:"sessionId"`
	MIMEType        string `json:"mimeType,omitempty"`
	Filename        string `json:"filename,omitempty"`
	SourceURI       string `json:"sourceUri,omitempty"`
	Size            int64  `json:"size"`
	SHA256          string `json:"sha256"`
	ClientRequestID string `json:"clientRequestId"`
}

type blobUploadChunkParams struct {
	SessionID       string `json:"sessionId"`
	UploadID        string `json:"uploadId"`
	Offset          int64  `json:"offset"`
	Content         string `json:"content"`
	ClientRequestID string `json:"clientRequestId"`
}

type blobUploadCommitParams struct {
	SessionID       string `json:"sessionId"`
	UploadID        string `json:"uploadId"`
	ClientRequestID string `json:"clientRequestId"`
}

type blobUploadAbortParams = blobUploadCommitParams

type blobUploadStartResult struct {
	UploadID   string `json:"uploadId"`
	SessionID  string `json:"sessionId"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	ExpiresAt  int64  `json:"expiresAt"`
	NextOffset int64  `json:"nextOffset"`
	ChunkSize  int64  `json:"chunkSize"`
}

type blobUploadChunkResult struct {
	UploadID      string `json:"uploadId"`
	NextOffset    int64  `json:"nextOffset"`
	ReceivedBytes int64  `json:"receivedBytes"`
}

type blobUploadAbortResult struct {
	UploadID string `json:"uploadId"`
	Aborted  bool   `json:"aborted"`
}

type blobMetadata struct {
	BlobID    string `json:"blobId"`
	SessionID string `json:"sessionId"`
	MIMEType  string `json:"mimeType,omitempty"`
	Filename  string `json:"filename,omitempty"`
	SourceURI string `json:"sourceUri,omitempty"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	ExpiresAt int64  `json:"expiresAt"`
}

type blobReadResult struct {
	blobMetadata
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	EOF        bool   `json:"eof"`
	Content    string `json:"content"`
}

type blobReleaseResult struct {
	BlobID   string `json:"blobId"`
	Released bool   `json:"released"`
}

func (s *Service) registerBlobHandlers() {
	s.routes["crush/blob/create"] = route{feature: FeatureBlob, handler: s.handleBlobCreate}
	s.routes["crush/blob/read"] = route{feature: FeatureBlob, handler: s.handleBlobRead}
	s.routes["crush/blob/release"] = route{feature: FeatureBlob, handler: s.handleBlobRelease}
	s.routes["crush/blob/upload/start"] = route{feature: FeatureBlobUpload, handler: s.handleBlobUploadStart}
	s.routes["crush/blob/upload/chunk"] = route{feature: FeatureBlobUpload, handler: s.handleBlobUploadChunk}
	s.routes["crush/blob/upload/commit"] = route{feature: FeatureBlobUpload, handler: s.handleBlobUploadCommit}
	s.routes["crush/blob/upload/abort"] = route{feature: FeatureBlobUpload, handler: s.handleBlobUploadAbort}
}

// SetBlobService replaces the connection-owned blob service. It is primarily
// useful for applying stricter policy limits and deterministic clocks.
func (s *Service) SetBlobService(service *blob.Service) {
	s.mu.Lock()
	previous := s.blobs
	previousOwned := s.ownsBlobService
	s.blobs = service
	s.ownsBlobService = false
	s.mu.Unlock()
	if previous != nil && previous != service {
		if previousOwned {
			previous.Close()
		} else {
			previous.ReleaseClient(s.blobOwnerID())
		}
	}
}

func (s *Service) handleBlobCreate(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params blobCreateParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" || params.ClientRequestID == "" || params.Size < 0 || params.SHA256 == "" {
		return nil, invalidParams(errors.New("sessionId, size, sha256, and clientRequestId are required"))
	}
	if params.Content != "" && len(params.Chunks) > 0 {
		return nil, invalidParams(errors.New("content and chunks are mutually exclusive"))
	}
	if rpcErr := s.validateBlobSession(ctx, params.SessionID); rpcErr != nil {
		return nil, rpcErr
	}
	return s.executeBlobMutation(ctx, "crush/blob/create", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		service := s.blobService()
		if service == nil {
			return nil, sourceUnavailable("blob service is unavailable")
		}
		// Keep create as the small, atomic compatibility path. Larger content
		// must use blobUpload so no request can exceed the 4 MiB ACP frame.
		if params.Size > maxAtomicBlobBytes || params.Size > service.MaxBlobBytes() || len(params.Chunks) > maxBlobChunks {
			return nil, protocolError(-32036, errorPayloadTooLarge, nil)
		}
		data, err := decodeBlobContent(params.Content, params.Chunks, params.Size)
		if err != nil {
			return nil, blobError(err)
		}
		metadata, err := service.Create(context.WithoutCancel(ctx), s.blobOwnerID(), blob.CreateInput{
			SessionID: params.SessionID, MIMEType: params.MIMEType, Filename: params.Filename,
			SourceURI: params.SourceURI, Size: params.Size, SHA256: params.SHA256, Data: data,
		})
		clear(data)
		s.recordBlobRetained(ctx)
		if err != nil {
			return nil, blobError(err)
		}
		return projectBlobMetadata(metadata), nil
	})
}

func (s *Service) handleBlobRead(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params blobReadParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" || params.BlobID == "" {
		return nil, invalidParams(errors.New("sessionId and blobId are required"))
	}
	service := s.blobService()
	if service == nil {
		return nil, sourceUnavailable("blob service is unavailable")
	}
	result, err := service.Read(ctx, s.blobOwnerID(), params.SessionID, params.BlobID, params.Offset, params.Limit)
	s.recordBlobRetained(ctx)
	if err != nil {
		return nil, blobError(err)
	}
	return blobReadResult{
		blobMetadata: projectBlobMetadata(result.Metadata), Offset: result.Offset,
		NextOffset: result.NextOffset, EOF: result.EOF,
		Content: base64.StdEncoding.EncodeToString(result.Data),
	}, nil
}

func (s *Service) handleBlobRelease(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params blobReleaseParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" || params.BlobID == "" || params.ClientRequestID == "" {
		return nil, invalidParams(errors.New("sessionId, blobId, and clientRequestId are required"))
	}
	return s.executeBlobMutation(ctx, "crush/blob/release", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		service := s.blobService()
		if service == nil {
			return nil, sourceUnavailable("blob service is unavailable")
		}
		err := service.Release(s.blobOwnerID(), params.SessionID, params.BlobID)
		s.recordBlobRetained(ctx)
		if err != nil {
			return nil, blobError(err)
		}
		return blobReleaseResult{BlobID: params.BlobID, Released: true}, nil
	})
}

func (s *Service) handleBlobUploadStart(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params blobUploadStartParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" || params.ClientRequestID == "" || params.Size < 0 || params.SHA256 == "" {
		return nil, invalidParams(errors.New("sessionId, size, sha256, and clientRequestId are required"))
	}
	if rpcErr := s.validateBlobSession(ctx, params.SessionID); rpcErr != nil {
		return nil, rpcErr
	}
	return s.executeBlobMutation(ctx, "crush/blob/upload/start", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		service := s.blobService()
		if service == nil {
			return nil, sourceUnavailable("blob service is unavailable")
		}
		started, err := service.StartUpload(context.WithoutCancel(ctx), s.blobOwnerID(), blob.CreateInput{
			SessionID: params.SessionID, MIMEType: params.MIMEType, Filename: params.Filename,
			SourceURI: params.SourceURI, Size: params.Size, SHA256: params.SHA256,
		})
		s.recordBlobRetained(ctx)
		if err != nil {
			return nil, blobError(err)
		}
		return projectBlobUploadStart(started), nil
	})
}

func (s *Service) handleBlobUploadChunk(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params blobUploadChunkParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" || params.UploadID == "" || params.Offset < 0 || params.Content == "" || params.ClientRequestID == "" {
		return nil, invalidParams(errors.New("sessionId, uploadId, offset, content, and clientRequestId are required"))
	}
	if int64(base64.StdEncoding.DecodedLen(len(params.Content))) > maxUploadChunkBytes+2 {
		return nil, protocolError(-32036, errorPayloadTooLarge, nil)
	}
	if rpcErr := s.validateBlobSession(ctx, params.SessionID); rpcErr != nil {
		return nil, rpcErr
	}
	// Do not retain encoded attachment data in the idempotency cache. A content
	// hash is sufficient to detect a conflicting retry and results are tiny.
	sum := sha256.Sum256([]byte(params.Content))
	replayPayload := struct {
		SessionID     string
		UploadID      string
		Offset        int64
		ContentSHA256 string
	}{params.SessionID, params.UploadID, params.Offset, hex.EncodeToString(sum[:])}
	return s.executeBlobMutation(ctx, "crush/blob/upload/chunk", params.SessionID, params.ClientRequestID, replayPayload, func() (any, *acp.RPCError) {
		data, err := base64.StdEncoding.DecodeString(params.Content)
		if err != nil {
			return nil, blobError(blob.ErrInvalidInput)
		}
		defer clear(data)
		if int64(len(data)) > maxUploadChunkBytes {
			return nil, protocolError(-32036, errorPayloadTooLarge, nil)
		}
		service := s.blobService()
		if service == nil {
			return nil, sourceUnavailable("blob service is unavailable")
		}
		progress, err := service.AppendUpload(context.WithoutCancel(ctx), s.blobOwnerID(), params.SessionID, params.UploadID, params.Offset, data)
		s.recordBlobRetained(ctx)
		if err != nil {
			return nil, blobError(err)
		}
		return blobUploadChunkResult{UploadID: progress.UploadID, NextOffset: progress.NextOffset, ReceivedBytes: progress.NextOffset}, nil
	})
}

func (s *Service) handleBlobUploadCommit(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params blobUploadCommitParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" || params.UploadID == "" || params.ClientRequestID == "" {
		return nil, invalidParams(errors.New("sessionId, uploadId, and clientRequestId are required"))
	}
	if rpcErr := s.validateBlobSession(ctx, params.SessionID); rpcErr != nil {
		return nil, rpcErr
	}
	return s.executeBlobMutation(ctx, "crush/blob/upload/commit", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		service := s.blobService()
		if service == nil {
			return nil, sourceUnavailable("blob service is unavailable")
		}
		metadata, err := service.CommitUpload(context.WithoutCancel(ctx), s.blobOwnerID(), params.SessionID, params.UploadID)
		s.recordBlobRetained(ctx)
		if err != nil {
			return nil, blobError(err)
		}
		return projectBlobMetadata(metadata), nil
	})
}

func (s *Service) handleBlobUploadAbort(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params blobUploadAbortParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" || params.UploadID == "" || params.ClientRequestID == "" {
		return nil, invalidParams(errors.New("sessionId, uploadId, and clientRequestId are required"))
	}
	if rpcErr := s.validateBlobSession(ctx, params.SessionID); rpcErr != nil {
		return nil, rpcErr
	}
	return s.executeBlobMutation(ctx, "crush/blob/upload/abort", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		service := s.blobService()
		if service == nil {
			return nil, sourceUnavailable("blob service is unavailable")
		}
		err := service.AbortUpload(s.blobOwnerID(), params.SessionID, params.UploadID)
		s.recordBlobRetained(ctx)
		if err != nil {
			return nil, blobError(err)
		}
		return blobUploadAbortResult{UploadID: params.UploadID, Aborted: true}, nil
	})
}

func (s *Service) resolveBlobAttachment(ctx context.Context, sessionID, blobID string) (message.Attachment, error) {
	if blobID == "" {
		return message.Attachment{}, blob.ErrInvalidInput
	}
	service := s.blobService()
	if service == nil {
		return message.Attachment{}, blob.ErrClosed
	}
	metadata, data, err := service.Resolve(ctx, s.blobOwnerID(), sessionID, blobID)
	if err != nil {
		return message.Attachment{}, err
	}
	return message.Attachment{
		FilePath: metadata.SourceURI, FileName: metadata.Filename,
		MimeType: metadata.MIMEType, Content: data,
	}, nil
}

func (s *Service) validateBlobSession(ctx context.Context, sessionID string) *acp.RPCError {
	s.mu.RLock()
	sessions := s.sessions
	s.mu.RUnlock()
	if sessions == nil {
		return sourceUnavailable("session source is unavailable")
	}
	if _, err := sessions.Get(ctx, sessionID); err != nil {
		return sessionSourceError(sessionID, err)
	}
	return nil
}

func (s *Service) executeBlobMutation(ctx context.Context, method, sessionID, requestID string, payload any, fn func() (any, *acp.RPCError)) (any, *acp.RPCError) {
	s.mu.RLock()
	store := s.blobIdempotency
	s.mu.RUnlock()
	if store == nil {
		return nil, sourceUnavailable("blob idempotency service is unavailable")
	}
	outcome, err := store.Execute(ctx, method+"\x00"+sessionID, requestID, payload, func() idempotency.Outcome {
		value, rpcErr := fn()
		return idempotency.Outcome{Value: value, Failure: rpcErr}
	})
	if err != nil {
		if errors.Is(err, idempotency.ErrConflict) {
			return nil, protocolError(-32030, errorIdempotencyConflict, map[string]any{"clientRequestId": requestID})
		}
		if errors.Is(err, idempotency.ErrCapacity) {
			return nil, &acp.RPCError{Code: -32032, Message: errorSessionBusy, Data: ErrorData{Code: errorSessionBusy, Retryable: true}}
		}
		return nil, invalidParams(errors.New("clientRequestId must be a UUID"))
	}
	if rpcErr, _ := outcome.Failure.(*acp.RPCError); rpcErr != nil {
		return nil, rpcErr
	}
	return outcome.Value, nil
}

func decodeBlobContent(content string, chunks []string, declaredSize int64) ([]byte, error) {
	var data []byte
	if len(chunks) == 0 {
		if declaredSize > 0 && int64(base64.StdEncoding.DecodedLen(len(content))) > declaredSize+2 {
			return nil, blob.ErrSizeMismatch
		}
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, blob.ErrInvalidInput
		}
		data = decoded
	} else {
		data = make([]byte, 0, declaredSize)
		for _, chunk := range chunks {
			if int64(base64.StdEncoding.DecodedLen(len(chunk))) > declaredSize-int64(len(data))+2 {
				clear(data)
				return nil, blob.ErrSizeMismatch
			}
			decoded, err := base64.StdEncoding.DecodeString(chunk)
			if err != nil {
				clear(data)
				return nil, blob.ErrInvalidInput
			}
			data = append(data, decoded...)
			clear(decoded)
		}
	}
	if int64(len(data)) != declaredSize {
		clear(data)
		return nil, blob.ErrSizeMismatch
	}
	return data, nil
}

func projectBlobMetadata(value blob.Metadata) blobMetadata {
	return blobMetadata{
		BlobID: value.ID, SessionID: value.SessionID, MIMEType: value.MIMEType,
		Filename: value.Filename, SourceURI: value.SourceURI, Size: value.Size,
		SHA256: value.SHA256, ExpiresAt: value.ExpiresAt.UnixMilli(),
	}
}

func projectBlobUploadStart(value blob.UploadMetadata) blobUploadStartResult {
	return blobUploadStartResult{
		UploadID: value.UploadID, SessionID: value.SessionID, Size: value.Size,
		SHA256: value.SHA256, ExpiresAt: value.ExpiresAt.UnixMilli(), NextOffset: value.NextOffset,
		ChunkSize: maxUploadChunkBytes,
	}
}

func blobError(err error) *acp.RPCError {
	switch {
	case errors.Is(err, blob.ErrNotFound), errors.Is(err, blob.ErrOwnerMismatch):
		return protocolError(-32040, errorBlobNotFound, nil)
	case errors.Is(err, blob.ErrBlobTooLarge), errors.Is(err, blob.ErrCapacity),
		errors.Is(err, blob.ErrChunkTooLarge):
		return protocolError(-32036, errorPayloadTooLarge, nil)
	case errors.Is(err, blob.ErrInvalidInput), errors.Is(err, blob.ErrHashMismatch),
		errors.Is(err, blob.ErrSizeMismatch), errors.Is(err, blob.ErrInvalidRange),
		errors.Is(err, blob.ErrUploadOffset):
		return invalidParams(errors.New("invalid blob request"))
	default:
		return &acp.RPCError{Code: acp.CodeInternalError, Message: "CRUSH_BLOB_FAILED", Data: ErrorData{Code: "CRUSH_BLOB_FAILED", Retryable: true}}
	}
}

func (s *Service) blobService() *blob.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.blobs
}

func (s *Service) blobOwnerID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.blobOwner
}

func (s *Service) releaseSessionBlobs(sessionID string) {
	if service := s.blobService(); service != nil {
		service.ReleaseSession(sessionID)
	}
}

func (s *Service) recordBlobRetained(ctx context.Context) {
	service := s.blobService()
	if service == nil {
		return
	}
	_, retained := service.Retained()
	guimetrics.FromContext(ctx).SetGauge(guimetrics.BlobRetainedBytes, retained, guimetrics.Labels{Kind: "blob"})
}
