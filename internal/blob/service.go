// Package blob owns bounded, expiring binary objects for desktop clients.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrNotFound      = errors.New("blob not found")
	ErrOwnerMismatch = errors.New("blob owner mismatch")
	ErrInvalidInput  = errors.New("invalid blob input")
	ErrHashMismatch  = errors.New("blob hash mismatch")
	ErrSizeMismatch  = errors.New("blob size mismatch")
	ErrBlobTooLarge  = errors.New("blob exceeds size limit")
	ErrCapacity      = errors.New("blob capacity exhausted")
	ErrInvalidRange  = errors.New("invalid blob range")
	ErrUploadOffset  = errors.New("invalid upload offset")
	ErrChunkTooLarge = errors.New("upload chunk exceeds size limit")
	ErrClosed        = errors.New("blob service is closed")
)

const (
	defaultTTL              = 10 * time.Minute
	defaultMaxBlobBytes     = 64 * 1024 * 1024
	defaultMaxRetainedBytes = 256 * 1024 * 1024
	defaultMaxBlobs         = 1024
	defaultMaxReadBytes     = 1024 * 1024
	defaultMaxUploadChunk   = 1024 * 1024
	maxMetadataBytes        = 2048
)

type Config struct {
	TTL              time.Duration
	MaxBlobBytes     int64
	MaxRetainedBytes int64
	MaxBlobs         int
	MaxReadBytes     int64
	MaxUploadChunk   int64
	Clock            func() time.Time
}

type CreateInput struct {
	SessionID string
	MIMEType  string
	Filename  string
	SourceURI string
	Size      int64
	SHA256    string
	Data      []byte
}

type Metadata struct {
	ID        string
	SessionID string
	MIMEType  string
	Filename  string
	SourceURI string
	Size      int64
	SHA256    string
	ExpiresAt time.Time
}

type ReadResult struct {
	Metadata   Metadata
	Data       []byte
	Offset     int64
	NextOffset int64
	EOF        bool
}

// UploadMetadata describes a reserved, not-yet-readable Blob upload.
type UploadMetadata struct {
	UploadID   string
	SessionID  string
	Size       int64
	SHA256     string
	ExpiresAt  time.Time
	NextOffset int64
}

type entry struct {
	metadata Metadata
	clientID string
	data     []byte
}

type upload struct {
	metadata   Metadata
	clientID   string
	file       *os.File
	path       string
	digest     hash.Hash
	nextOffset int64
}

type Service struct {
	mu       sync.Mutex
	config   Config
	entries  map[string]*entry
	pending  map[string]*upload
	retained int64
	closed   bool
}

func New(config Config) *Service {
	if config.TTL <= 0 {
		config.TTL = defaultTTL
	}
	if config.MaxBlobBytes <= 0 {
		config.MaxBlobBytes = defaultMaxBlobBytes
	}
	if config.MaxRetainedBytes <= 0 {
		config.MaxRetainedBytes = defaultMaxRetainedBytes
	}
	if config.MaxBlobs <= 0 {
		config.MaxBlobs = defaultMaxBlobs
	}
	if config.MaxReadBytes <= 0 {
		config.MaxReadBytes = defaultMaxReadBytes
	}
	if config.MaxUploadChunk <= 0 {
		config.MaxUploadChunk = defaultMaxUploadChunk
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Service{config: config, entries: make(map[string]*entry), pending: make(map[string]*upload)}
}

// StartUpload reserves Blob capacity before accepting segmented content.
func (s *Service) StartUpload(ctx context.Context, clientID string, input CreateInput) (UploadMetadata, error) {
	if err := ctx.Err(); err != nil {
		return UploadMetadata{}, err
	}
	if strings.TrimSpace(clientID) == "" || len(input.Data) != 0 {
		return UploadMetadata{}, ErrInvalidInput
	}
	if err := validateInput(input); err != nil {
		return UploadMetadata{}, err
	}
	if input.Size > s.config.MaxBlobBytes {
		return UploadMetadata{}, ErrBlobTooLarge
	}
	now := s.config.Clock()
	metadata := Metadata{
		ID: uuid.NewString(), SessionID: input.SessionID, MIMEType: input.MIMEType,
		Filename: input.Filename, SourceURI: input.SourceURI, Size: input.Size,
		SHA256: input.SHA256, ExpiresAt: now.Add(s.config.TTL),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return UploadMetadata{}, ErrClosed
	}
	s.pruneLocked(now)
	if len(s.entries)+len(s.pending) >= s.config.MaxBlobs || input.Size > s.config.MaxRetainedBytes-s.retained {
		return UploadMetadata{}, ErrCapacity
	}
	file, err := os.CreateTemp("", "crush-blob-upload-*")
	if err != nil {
		return UploadMetadata{}, err
	}
	s.pending[metadata.ID] = &upload{
		metadata: metadata, clientID: clientID, file: file, path: file.Name(),
		digest: sha256.New(),
	}
	s.retained += input.Size
	return uploadMetadata(metadata, 0), nil
}

// AppendUpload accepts only contiguous chunks and never exposes pending data.
func (s *Service) AppendUpload(ctx context.Context, clientID, sessionID, uploadID string, offset int64, data []byte) (UploadMetadata, error) {
	if err := ctx.Err(); err != nil {
		return UploadMetadata{}, err
	}
	if len(data) == 0 {
		return UploadMetadata{}, ErrInvalidInput
	}
	if int64(len(data)) > s.config.MaxUploadChunk {
		return UploadMetadata{}, ErrChunkTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return UploadMetadata{}, ErrClosed
	}
	s.pruneLocked(s.config.Clock())
	current := s.pending[uploadID]
	if current == nil {
		return UploadMetadata{}, ErrNotFound
	}
	if current.clientID != clientID || current.metadata.SessionID != sessionID {
		return UploadMetadata{}, ErrOwnerMismatch
	}
	if offset != current.nextOffset || int64(len(data)) > current.metadata.Size-offset {
		return UploadMetadata{}, ErrUploadOffset
	}
	if written, err := current.file.Write(data); err != nil || written != len(data) {
		return UploadMetadata{}, errors.New("failed to stage upload chunk")
	}
	_, _ = current.digest.Write(data)
	current.nextOffset += int64(len(data))
	return uploadMetadata(current.metadata, current.nextOffset), nil
}

// CommitUpload atomically turns a complete pending upload into its readable
// Blob handle. The upload ID becomes the Blob ID.
func (s *Service) CommitUpload(ctx context.Context, clientID, sessionID, uploadID string) (Metadata, error) {
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Metadata{}, ErrClosed
	}
	s.pruneLocked(s.config.Clock())
	current := s.pending[uploadID]
	if current == nil {
		return Metadata{}, ErrNotFound
	}
	if current.clientID != clientID || current.metadata.SessionID != sessionID {
		return Metadata{}, ErrOwnerMismatch
	}
	if current.nextOffset != current.metadata.Size {
		return Metadata{}, ErrSizeMismatch
	}
	if !strings.EqualFold(current.metadata.SHA256, hex.EncodeToString(current.digest.Sum(nil))) {
		return Metadata{}, ErrHashMismatch
	}
	if err := current.file.Sync(); err != nil {
		return Metadata{}, err
	}
	data, err := os.ReadFile(current.path)
	if err != nil {
		return Metadata{}, err
	}
	if int64(len(data)) != current.metadata.Size {
		clear(data)
		return Metadata{}, ErrSizeMismatch
	}
	if err := current.file.Close(); err != nil {
		clear(data)
		return Metadata{}, err
	}
	_ = os.Remove(current.path)
	delete(s.pending, uploadID)
	s.entries[uploadID] = &entry{metadata: current.metadata, clientID: current.clientID, data: data}
	return current.metadata, nil
}

// AbortUpload releases a pending capacity reservation.
func (s *Service) AbortUpload(clientID, sessionID, uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.pruneLocked(s.config.Clock())
	current := s.pending[uploadID]
	if current == nil {
		return ErrNotFound
	}
	if current.clientID != clientID || current.metadata.SessionID != sessionID {
		return ErrOwnerMismatch
	}
	s.deleteUploadLocked(uploadID, current)
	return nil
}

func (s *Service) Create(ctx context.Context, clientID string, input CreateInput) (Metadata, error) {
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if strings.TrimSpace(clientID) == "" {
		return Metadata{}, ErrInvalidInput
	}
	if err := validateInput(input); err != nil {
		return Metadata{}, err
	}
	if input.Size != int64(len(input.Data)) {
		return Metadata{}, ErrSizeMismatch
	}
	if input.Size > s.config.MaxBlobBytes {
		return Metadata{}, ErrBlobTooLarge
	}
	sum := sha256.Sum256(input.Data)
	hash := hex.EncodeToString(sum[:])
	if !strings.EqualFold(input.SHA256, hash) {
		return Metadata{}, ErrHashMismatch
	}
	now := s.config.Clock()
	metadata := Metadata{
		ID: uuid.NewString(), SessionID: input.SessionID, MIMEType: input.MIMEType,
		Filename: input.Filename, SourceURI: input.SourceURI, Size: input.Size,
		SHA256: hash, ExpiresAt: now.Add(s.config.TTL),
	}
	data := append([]byte(nil), input.Data...)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Metadata{}, ErrClosed
	}
	s.pruneLocked(now)
	if len(s.entries) >= s.config.MaxBlobs || input.Size > s.config.MaxRetainedBytes-s.retained {
		return Metadata{}, ErrCapacity
	}
	s.entries[metadata.ID] = &entry{metadata: metadata, clientID: clientID, data: data}
	s.retained += input.Size
	return metadata, nil
}

func (s *Service) Read(ctx context.Context, clientID, sessionID, id string, offset, limit int64) (ReadResult, error) {
	if err := ctx.Err(); err != nil {
		return ReadResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ReadResult{}, ErrClosed
	}
	s.pruneLocked(s.config.Clock())
	current := s.entries[id]
	if current == nil {
		return ReadResult{}, ErrNotFound
	}
	if current.clientID != clientID || current.metadata.SessionID != sessionID {
		return ReadResult{}, ErrOwnerMismatch
	}
	if offset < 0 || offset > current.metadata.Size || limit <= 0 || limit > s.config.MaxReadBytes {
		return ReadResult{}, ErrInvalidRange
	}
	end := min(offset+limit, current.metadata.Size)
	data := append([]byte(nil), current.data[offset:end]...)
	return ReadResult{
		Metadata: current.metadata, Data: data, Offset: offset,
		NextOffset: end, EOF: end == current.metadata.Size,
	}, nil
}

// Resolve returns an owned copy for an in-process consumer. The same owner and
// expiry checks as ranged reads are applied before copying.
func (s *Service) Resolve(ctx context.Context, clientID, sessionID, id string) (Metadata, []byte, error) {
	if err := ctx.Err(); err != nil {
		return Metadata{}, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Metadata{}, nil, ErrClosed
	}
	s.pruneLocked(s.config.Clock())
	current := s.entries[id]
	if current == nil {
		return Metadata{}, nil, ErrNotFound
	}
	if current.clientID != clientID || current.metadata.SessionID != sessionID {
		return Metadata{}, nil, ErrOwnerMismatch
	}
	return current.metadata, append([]byte(nil), current.data...), nil
}

func (s *Service) Release(clientID, sessionID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.pruneLocked(s.config.Clock())
	current := s.entries[id]
	if current == nil {
		return ErrNotFound
	}
	if current.clientID != clientID || current.metadata.SessionID != sessionID {
		return ErrOwnerMismatch
	}
	s.deleteLocked(id, current)
	return nil
}

func (s *Service) ReleaseSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, current := range s.entries {
		if current.metadata.SessionID == sessionID {
			s.deleteLocked(id, current)
		}
	}
	for id, current := range s.pending {
		if current.metadata.SessionID == sessionID {
			s.deleteUploadLocked(id, current)
		}
	}
}

func (s *Service) ReleaseClient(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, current := range s.entries {
		if current.clientID == clientID {
			s.deleteLocked(id, current)
		}
	}
	for id, current := range s.pending {
		if current.clientID == clientID {
			s.deleteUploadLocked(id, current)
		}
	}
}

func (s *Service) Retained() (count int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.pruneLocked(s.config.Clock())
	}
	return len(s.entries) + len(s.pending), s.retained
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	clear(s.entries)
	for id, current := range s.pending {
		s.deleteUploadLocked(id, current)
	}
	s.retained = 0
}

func (s *Service) MaxBlobBytes() int64 { return s.config.MaxBlobBytes }

func (s *Service) MaxUploadChunk() int64 { return s.config.MaxUploadChunk }

func validateInput(input CreateInput) error {
	if strings.TrimSpace(input.SessionID) == "" || input.Size < 0 || len(input.SHA256) != sha256.Size*2 {
		return ErrInvalidInput
	}
	for _, value := range []string{input.MIMEType, input.Filename, input.SourceURI} {
		if !utf8.ValidString(value) || len(value) > maxMetadataBytes || strings.ContainsRune(value, '\x00') {
			return ErrInvalidInput
		}
	}
	if strings.ContainsAny(input.Filename, `/\\`) {
		return ErrInvalidInput
	}
	if input.SHA256 != strings.ToLower(input.SHA256) {
		return ErrInvalidInput
	}
	if _, err := hex.DecodeString(input.SHA256); err != nil {
		return ErrInvalidInput
	}
	return nil
}

func (s *Service) pruneLocked(now time.Time) {
	for id, current := range s.entries {
		if !now.Before(current.metadata.ExpiresAt) {
			s.deleteLocked(id, current)
		}
	}
	for id, current := range s.pending {
		if !now.Before(current.metadata.ExpiresAt) {
			s.deleteUploadLocked(id, current)
		}
	}
}

func (s *Service) deleteLocked(id string, current *entry) {
	delete(s.entries, id)
	s.retained -= current.metadata.Size
	clear(current.data)
}

func (s *Service) deleteUploadLocked(id string, current *upload) {
	delete(s.pending, id)
	s.retained -= current.metadata.Size
	if current.file != nil {
		_ = current.file.Close()
	}
	if current.path != "" {
		_ = os.Remove(current.path)
	}
}

func uploadMetadata(metadata Metadata, nextOffset int64) UploadMetadata {
	return UploadMetadata{
		UploadID: metadata.ID, SessionID: metadata.SessionID, Size: metadata.Size,
		SHA256: metadata.SHA256, ExpiresAt: metadata.ExpiresAt, NextOffset: nextOffset,
	}
}
