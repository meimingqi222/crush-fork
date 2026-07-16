// Package terminal owns interactive PTY/ConPTY resources and retained output.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrNotFound      = errors.New("terminal not found")
	ErrOwnerMismatch = errors.New("terminal owner mismatch")
	ErrInvalidInput  = errors.New("invalid terminal input")
	ErrInvalidOffset = errors.New("invalid terminal offset")
	ErrCapacity      = errors.New("terminal capacity exhausted")
	ErrNotRunning    = errors.New("terminal is not running")
	ErrClosed        = errors.New("terminal manager is closed")
)

const (
	defaultRetainedBytes = 2 * 1024 * 1024
	defaultSnapshotBytes = 2 * 1024 * 1024
	maximumRetainedBytes = 4 * 1024 * 1024
	defaultMaxTerminals  = 256
	defaultRetention     = 10 * time.Minute
	defaultCols          = 80
	defaultRows          = 24
	maxDimension         = 1000
	maxInputBytes        = 1024 * 1024
	maxCommandBytes      = 4096
	maxArgs              = 256
	maxEnv               = 256
	maxMetadataBytes     = 4096
	terminalDrainTimeout = 250 * time.Millisecond
)

type State string

const (
	StateRunning State = "running"
	StateExited  State = "exited"
	StateKilled  State = "killed"
)

type Config struct {
	RetainedBytes int
	SnapshotBytes int
	MaxTerminals  int
	Retention     time.Duration
	Clock         func() time.Time
	Factory       Factory
	OnOutput      func(sessionID, terminalID string, offset uint64, data []byte)
	OnExit        func(sessionID, terminalID string, exit Exit)
}

type OpenRequest struct {
	ClientID  string
	SessionID string
	Command   string
	Args      []string
	CWD       string
	Env       map[string]string
	Cols      int
	Rows      int
}

type Metadata struct {
	ID        string
	SessionID string
	State     State
	Cols      int
	Rows      int
	Offset    uint64
	CreatedAt time.Time
	ExitedAt  time.Time
	ExitCode  int
	Signal    string
	HasExit   bool
	CWD       string
	Command   string
	Args      []string
}

type Snapshot struct {
	Metadata
	StartOffset uint64
	EndOffset   uint64
	Truncated   bool
	Data        []byte
	More        bool
}

type Exit struct {
	State     State
	Code      int
	Signal    string
	Timestamp time.Time
	Offset    uint64
}

type terminal struct {
	mu        sync.Mutex
	writeMu   sync.Mutex
	metadata  Metadata
	clientID  string
	process   Process
	output    byteRing
	removed   bool
	readDone  chan struct{}
	callbacks sync.WaitGroup
}

type Manager struct {
	mu      sync.Mutex
	config  Config
	entries map[string]*terminal
	closed  bool
}

func New(config Config) *Manager {
	if config.RetainedBytes <= 0 {
		config.RetainedBytes = defaultRetainedBytes
	}
	config.RetainedBytes = min(config.RetainedBytes, maximumRetainedBytes)
	if config.SnapshotBytes <= 0 {
		config.SnapshotBytes = defaultSnapshotBytes
	}
	config.SnapshotBytes = min(config.SnapshotBytes, defaultSnapshotBytes, config.RetainedBytes)
	if config.MaxTerminals <= 0 {
		config.MaxTerminals = defaultMaxTerminals
	}
	if config.Retention <= 0 {
		config.Retention = defaultRetention
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Factory == nil {
		config.Factory = nativeFactory{}
	}
	return &Manager{config: config, entries: make(map[string]*terminal)}
}

func (m *Manager) Open(ctx context.Context, request OpenRequest) (Metadata, error) {
	if err := validateOpenRequest(&request); err != nil {
		return Metadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Metadata{}, ErrClosed
	}
	m.pruneLocked(m.config.Clock())
	if len(m.entries) >= m.config.MaxTerminals {
		m.mu.Unlock()
		return Metadata{}, ErrCapacity
	}
	m.mu.Unlock()

	proc, err := m.config.Factory.Start(ctx, request)
	if err != nil {
		return Metadata{}, fmt.Errorf("starting terminal: %w", err)
	}
	now := m.config.Clock()
	current := &terminal{
		metadata: Metadata{
			ID: uuid.NewString(), SessionID: request.SessionID, State: StateRunning,
			Cols: request.Cols, Rows: request.Rows, CreatedAt: now, CWD: request.CWD,
			Command: request.Command, Args: append([]string(nil), request.Args...),
		},
		clientID: request.ClientID, process: proc,
		output: newByteRing(m.config.RetainedBytes), readDone: make(chan struct{}),
	}
	m.mu.Lock()
	if m.closed || len(m.entries) >= m.config.MaxTerminals {
		m.mu.Unlock()
		_ = proc.Kill("kill")
		_ = proc.Close()
		if m.closed {
			return Metadata{}, ErrClosed
		}
		return Metadata{}, ErrCapacity
	}
	m.entries[current.metadata.ID] = current
	m.mu.Unlock()
	go m.readLoop(current, proc)
	go m.waitLoop(current, proc)
	return cloneMetadata(current.metadata), nil
}

func (m *Manager) Input(ctx context.Context, clientID, sessionID, id string, data []byte) (int, error) {
	if len(data) == 0 || len(data) > maxInputBytes {
		return 0, ErrInvalidInput
	}
	current, err := m.owned(clientID, sessionID, id)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	current.mu.Lock()
	proc := current.process
	running := current.metadata.State == StateRunning && proc != nil
	current.mu.Unlock()
	if !running {
		return 0, ErrNotRunning
	}
	current.writeMu.Lock()
	defer current.writeMu.Unlock()
	return proc.Write(data)
}

func (m *Manager) Resize(clientID, sessionID, id string, cols, rows int) (Metadata, error) {
	if !validDimensions(cols, rows) {
		return Metadata{}, ErrInvalidInput
	}
	current, err := m.owned(clientID, sessionID, id)
	if err != nil {
		return Metadata{}, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.metadata.State != StateRunning || current.process == nil {
		return Metadata{}, ErrNotRunning
	}
	if err := current.process.Resize(cols, rows); err != nil {
		return Metadata{}, err
	}
	current.metadata.Cols, current.metadata.Rows = cols, rows
	return cloneMetadata(current.metadata), nil
}

func (m *Manager) Kill(clientID, sessionID, id, signal string) error {
	current, err := m.owned(clientID, sessionID, id)
	if err != nil {
		return err
	}
	current.mu.Lock()
	proc := current.process
	running := current.metadata.State == StateRunning && proc != nil
	current.mu.Unlock()
	if !running {
		return ErrNotRunning
	}
	return proc.Kill(normalizeSignal(signal))
}

func (m *Manager) Snapshot(clientID, sessionID, id string, after uint64) (Snapshot, error) {
	current, err := m.owned(clientID, sessionID, id)
	if err != nil {
		return Snapshot{}, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	data, start, end, truncated, ok := current.output.snapshot(after, m.config.SnapshotBytes)
	if !ok {
		return Snapshot{}, ErrInvalidOffset
	}
	metadata := cloneMetadata(current.metadata)
	metadata.Offset = current.output.endOffset
	return Snapshot{
		Metadata: metadata, StartOffset: start, EndOffset: end,
		Truncated: truncated, Data: data, More: end < current.output.endOffset,
	}, nil
}

func (m *Manager) Get(clientID, sessionID, id string) (Metadata, error) {
	current, err := m.owned(clientID, sessionID, id)
	if err != nil {
		return Metadata{}, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	metadata := cloneMetadata(current.metadata)
	metadata.Offset = current.output.endOffset
	return metadata, nil
}

func (m *Manager) ListSession(sessionID string) []Metadata {
	m.mu.Lock()
	m.pruneLocked(m.config.Clock())
	values := make([]*terminal, 0)
	for _, current := range m.entries {
		if current.metadata.SessionID == sessionID {
			values = append(values, current)
		}
	}
	m.mu.Unlock()
	result := make([]Metadata, 0, len(values))
	for _, current := range values {
		current.mu.Lock()
		metadata := cloneMetadata(current.metadata)
		metadata.Offset = current.output.endOffset
		current.mu.Unlock()
		result = append(result, metadata)
	}
	slices.SortFunc(result, func(a, b Metadata) int { return strings.Compare(a.ID, b.ID) })
	return result
}

func (m *Manager) RetainedBytes() int64 {
	m.mu.Lock()
	values := make([]*terminal, 0, len(m.entries))
	for _, current := range m.entries {
		values = append(values, current)
	}
	m.mu.Unlock()
	var total int64
	for _, current := range values {
		current.mu.Lock()
		total += int64(current.output.length)
		current.mu.Unlock()
	}
	return total
}

func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	values := make([]*terminal, 0, len(m.entries))
	for _, current := range m.entries {
		values = append(values, current)
	}
	m.mu.Unlock()
	count := 0
	for _, current := range values {
		current.mu.Lock()
		if current.metadata.State == StateRunning && current.process != nil {
			count++
		}
		current.mu.Unlock()
	}
	return count
}

func (m *Manager) CloseSession(sessionID string) {
	m.release(func(t *terminal) bool { return t.metadata.SessionID == sessionID })
}
func (m *Manager) CloseClient(clientID string) {
	m.release(func(t *terminal) bool { return t.clientID == clientID })
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	values := make([]*terminal, 0, len(m.entries))
	for _, current := range m.entries {
		current.mu.Lock()
		current.removed = true
		current.mu.Unlock()
		values = append(values, current)
	}
	clear(m.entries)
	m.mu.Unlock()
	for _, current := range values {
		m.closeTerminal(current)
	}
}

func (m *Manager) owned(clientID, sessionID, id string) (*terminal, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	m.pruneLocked(m.config.Clock())
	current := m.entries[id]
	m.mu.Unlock()
	if current == nil {
		return nil, ErrNotFound
	}
	current.mu.Lock()
	owned := !current.removed && current.clientID == clientID && current.metadata.SessionID == sessionID
	current.mu.Unlock()
	if !owned {
		return nil, ErrOwnerMismatch
	}
	return current, nil
}

func (m *Manager) readLoop(current *terminal, proc Process) {
	defer close(current.readDone)
	buffer := make([]byte, 32*1024)
	for {
		n, err := proc.Read(buffer)
		if n > 0 {
			chunk := append([]byte(nil), buffer[:n]...)
			current.mu.Lock()
			offset := current.output.append(chunk)
			current.metadata.Offset = current.output.endOffset
			removed := current.removed
			sessionID, id := current.metadata.SessionID, current.metadata.ID
			if !removed && m.config.OnOutput != nil {
				current.callbacks.Add(1)
			}
			current.mu.Unlock()
			if !removed && m.config.OnOutput != nil {
				func() {
					defer current.callbacks.Done()
					m.config.OnOutput(sessionID, id, offset, chunk)
				}()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// PTY closure is the normal terminal completion signal.
			}
			return
		}
	}
}

func (m *Manager) waitLoop(current *terminal, proc Process) {
	result, err := proc.Wait(context.Background())
	drained := false
	select {
	case <-current.readDone:
		drained = true
	case <-time.After(terminalDrainTimeout):
	}
	_ = proc.Close()
	if !drained {
		select {
		case <-current.readDone:
		case <-time.After(time.Second):
		}
	}
	now := m.config.Clock()
	current.mu.Lock()
	if current.process == proc {
		current.process = nil
	}
	if result.Signal != "" {
		current.metadata.State = StateKilled
	} else {
		current.metadata.State = StateExited
	}
	current.metadata.ExitedAt = now
	current.metadata.ExitCode = result.Code
	current.metadata.Signal = result.Signal
	current.metadata.HasExit = true
	current.metadata.Offset = current.output.endOffset
	removed := current.removed
	exit := Exit{
		State: current.metadata.State, Code: result.Code, Signal: result.Signal,
		Timestamp: now, Offset: current.output.endOffset,
	}
	sessionID, id := current.metadata.SessionID, current.metadata.ID
	if !removed && m.config.OnExit != nil {
		current.callbacks.Add(1)
	}
	current.mu.Unlock()
	if err != nil && result.Signal == "" {
		exit.State = StateExited
	}
	if !removed && m.config.OnExit != nil {
		func() {
			defer current.callbacks.Done()
			m.config.OnExit(sessionID, id, exit)
		}()
	}
}

func (m *Manager) release(match func(*terminal) bool) {
	m.mu.Lock()
	values := make([]*terminal, 0)
	for id, current := range m.entries {
		current.mu.Lock()
		matched := match(current)
		if matched {
			current.removed = true
		}
		current.mu.Unlock()
		if matched {
			delete(m.entries, id)
			values = append(values, current)
		}
	}
	m.mu.Unlock()
	for _, current := range values {
		m.closeTerminal(current)
	}
}

func (m *Manager) closeTerminal(current *terminal) {
	current.callbacks.Wait()
	current.mu.Lock()
	proc := current.process
	current.process = nil
	current.output.clear()
	current.mu.Unlock()
	if proc != nil {
		_ = proc.Kill("kill")
		_ = proc.Close()
	}
}

func (m *Manager) pruneLocked(now time.Time) {
	cutoff := now.Add(-m.config.Retention)
	for id, current := range m.entries {
		current.mu.Lock()
		expired := current.metadata.HasExit && current.metadata.ExitedAt.Before(cutoff)
		if expired {
			current.removed = true
			current.output.clear()
		}
		current.mu.Unlock()
		if expired {
			delete(m.entries, id)
		}
	}
}

func validateOpenRequest(request *OpenRequest) error {
	request.ClientID = strings.TrimSpace(request.ClientID)
	request.SessionID = strings.TrimSpace(request.SessionID)
	request.Command = strings.TrimSpace(request.Command)
	if request.ClientID == "" || request.SessionID == "" || request.Command == "" || len(request.Command) > maxCommandBytes || !utf8.ValidString(request.Command) {
		return ErrInvalidInput
	}
	if len(request.Args) > maxArgs || len(request.Env) > maxEnv || len(request.CWD) > maxMetadataBytes || !utf8.ValidString(request.CWD) {
		return ErrInvalidInput
	}
	if request.Cols == 0 {
		request.Cols = defaultCols
	}
	if request.Rows == 0 {
		request.Rows = defaultRows
	}
	if !validDimensions(request.Cols, request.Rows) {
		return ErrInvalidInput
	}
	for _, arg := range request.Args {
		if len(arg) > maxMetadataBytes || !utf8.ValidString(arg) || strings.ContainsRune(arg, '\x00') {
			return ErrInvalidInput
		}
	}
	for key, value := range request.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || len(key) > 256 || len(value) > maxMetadataBytes || !utf8.ValidString(key) || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return ErrInvalidInput
		}
	}
	if strings.ContainsRune(request.Command, '\x00') || strings.ContainsRune(request.CWD, '\x00') {
		return ErrInvalidInput
	}
	if request.CWD == "" {
		request.CWD, _ = os.Getwd()
	}
	return nil
}

func ValidateOpenRequest(request *OpenRequest) error { return validateOpenRequest(request) }

func validDimensions(cols, rows int) bool {
	return cols > 0 && cols <= maxDimension && rows > 0 && rows <= maxDimension
}

func normalizeSignal(signal string) string {
	switch strings.ToLower(strings.TrimSpace(signal)) {
	case "int", "interrupt", "sigint":
		return "interrupt"
	case "term", "terminate", "sigterm", "":
		return "terminate"
	default:
		return "kill"
	}
}

func cloneMetadata(value Metadata) Metadata {
	value.Args = append([]string(nil), value.Args...)
	return value
}
