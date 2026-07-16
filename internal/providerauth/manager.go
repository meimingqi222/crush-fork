package providerauth

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	defaultMaxLogins  = 128
	defaultRetention  = 10 * time.Minute
	maxProviderID     = 128
	maxAPIKeyBytes    = 64 << 10
	maxChallengeBytes = 2048
)

type Config struct {
	Factories map[FlowKey]FlowFactory
	MaxLogins int
	Retention time.Duration
	Clock     func() time.Time
}

type Manager struct {
	mu        sync.Mutex
	backend   Backend
	factories map[FlowKey]FlowFactory
	logins    map[string]*login
	active    map[string]string
	maxLogins int
	retention time.Duration
	clock     func() time.Time
	closed    bool
}

type login struct {
	id          string
	providerID  string
	owner       string
	status      LoginStatus
	flow        Flow
	sink        EventSink
	flowCtx     context.Context
	flowCancel  context.CancelFunc
	eventCtx    context.Context
	eventCancel context.CancelFunc
	createdAt   time.Time
	finishedAt  time.Time
	done        chan struct{}
	started     bool
	complete    bool
}

func New(backend Backend, config Config) *Manager {
	if config.MaxLogins <= 0 {
		config.MaxLogins = defaultMaxLogins
	}
	if config.Retention <= 0 {
		config.Retention = defaultRetention
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	factories := make(map[FlowKey]FlowFactory, len(config.Factories))
	for key, factory := range config.Factories {
		factories[key] = factory
	}
	return &Manager{
		backend: backend, factories: factories, logins: make(map[string]*login),
		active: make(map[string]string), maxLogins: config.MaxLogins,
		retention: config.Retention, clock: config.Clock,
	}
}

func (m *Manager) Providers() []Provider {
	providers := m.backend.Providers()
	slices.SortFunc(providers, func(a, b Provider) int { return strings.Compare(a.ID, b.ID) })
	return providers
}

func (m *Manager) Models(providerID string) ([]Model, error) {
	if err := validateProviderID(providerID); err != nil {
		return nil, ErrProviderNotFound
	}
	models, err := m.backend.Models(providerID)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(models, func(a, b Model) int { return strings.Compare(a.ID, b.ID) })
	return models, nil
}

func (m *Manager) AuthStatus(providerID string) (AuthStatus, error) {
	if err := validateProviderID(providerID); err != nil {
		return AuthStatus{}, ErrProviderNotFound
	}
	return m.backend.AuthStatus(providerID)
}

// PrepareLogin allocates a login without starting external work. Start must be
// called only after the login response has been written to the client.
func (m *Manager) PrepareLogin(owner, providerID string, method AuthMethod, apiKey string, sink EventSink) (string, error) {
	if owner == "" || sink == nil || validateProviderID(providerID) != nil {
		return "", ErrProviderNotFound
	}
	var flow Flow
	switch method {
	case AuthMethodAPIKey:
		if err := validateAPIKey(apiKey); err != nil {
			return "", err
		}
		flow = &apiKeyFlow{apiKey: apiKey}
	default:
		factory := m.factories[FlowKey{ProviderID: providerID, Method: method}]
		if factory == nil {
			return "", ErrAuthMethodUnsupported
		}
		flow = factory.New(providerID)
		if flow == nil {
			return "", ErrAuthMethodUnsupported
		}
	}
	if !providerSupports(m.backend.Providers(), providerID, method) {
		return "", ErrAuthMethodUnsupported
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", ErrClosed
	}
	m.pruneLocked()
	if _, ok := m.active[providerID]; ok {
		return "", ErrLoginInProgress
	}
	if len(m.logins) >= m.maxLogins {
		m.evictOldestTerminalLocked()
	}
	if len(m.logins) >= m.maxLogins {
		return "", ErrCapacity
	}
	flowCtx, flowCancel := context.WithCancel(context.Background())
	eventCtx, eventCancel := context.WithCancel(context.Background())
	id := uuid.NewString()
	entry := &login{
		id: id, providerID: providerID, owner: owner, status: StatusStarting,
		flow: flow, sink: sink, flowCtx: flowCtx, flowCancel: flowCancel,
		eventCtx: eventCtx, eventCancel: eventCancel, createdAt: m.clock(), done: make(chan struct{}),
	}
	m.logins[id] = entry
	m.active[providerID] = id
	return id, nil
}

func (m *Manager) Start(owner, loginID string) error {
	m.mu.Lock()
	entry := m.logins[loginID]
	if entry == nil || entry.owner != owner {
		m.mu.Unlock()
		return ErrLoginNotFound
	}
	if entry.started || isTerminal(entry.status) {
		m.mu.Unlock()
		return nil
	}
	entry.started = true
	m.mu.Unlock()
	go m.run(entry)
	return nil
}

func (m *Manager) AbortPrepared(owner, loginID string) {
	m.mu.Lock()
	entry := m.logins[loginID]
	if entry == nil || entry.owner != owner || entry.started {
		m.mu.Unlock()
		return
	}
	delete(m.logins, loginID)
	delete(m.active, entry.providerID)
	entry.flowCancel()
	entry.eventCancel()
	entry.status = StatusCancelled
	entry.finishedAt = m.clock()
	entry.complete = true
	close(entry.done)
	m.mu.Unlock()
}

func (m *Manager) CanCancel(owner, loginID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.logins[loginID]
	if entry == nil || entry.owner != owner || isTerminal(entry.status) {
		return ErrLoginNotFound
	}
	return nil
}

func (m *Manager) Cancel(owner, loginID string) error {
	m.mu.Lock()
	entry := m.logins[loginID]
	if entry == nil || entry.owner != owner || isTerminal(entry.status) {
		m.mu.Unlock()
		return ErrLoginNotFound
	}
	entry.flowCancel()
	done := entry.done
	started := entry.started
	if !started {
		entry.started = true
	}
	m.mu.Unlock()
	if !started {
		go m.finish(entry, StatusCancelled, "", "")
	}
	<-done
	return nil
}

func (m *Manager) Logout(providerID string) error {
	if validateProviderID(providerID) != nil {
		return ErrProviderNotFound
	}
	m.mu.Lock()
	var done chan struct{}
	if id := m.active[providerID]; id != "" {
		entry := m.logins[id]
		if entry != nil {
			done = entry.done
			if !isTerminal(entry.status) {
				entry.flowCancel()
				if !entry.started {
					entry.started = true
					go m.finish(entry, StatusCancelled, "", "")
				}
			}
		}
	}
	m.mu.Unlock()
	if done != nil {
		<-done
	}
	return m.backend.ClearCredential(providerID)
}

func (m *Manager) CloseOwner(owner string) {
	entries := m.closeMatching(func(entry *login) bool { return entry.owner == owner })
	for _, entry := range entries {
		<-entry.done
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()
	entries := m.closeMatching(func(*login) bool { return true })
	for _, entry := range entries {
		<-entry.done
	}
}

func (m *Manager) closeMatching(match func(*login) bool) []*login {
	m.mu.Lock()
	entries := make([]*login, 0)
	for _, entry := range m.logins {
		if !match(entry) {
			continue
		}
		entry.eventCancel()
		if isTerminal(entry.status) {
			entries = append(entries, entry)
			continue
		}
		entry.flowCancel()
		if !entry.started {
			entry.started = true
			entry.status = StatusCancelled
			entry.finishedAt = m.clock()
			entry.complete = true
			delete(m.active, entry.providerID)
			close(entry.done)
		}
		entries = append(entries, entry)
	}
	m.mu.Unlock()
	return entries
}

func (m *Manager) run(entry *login) {
	credential, err := entry.flow.Run(entry.flowCtx, func(prompt Prompt) error {
		event, promptErr := challengeEvent(entry, prompt)
		if promptErr != nil {
			return promptErr
		}
		m.setStatus(entry, event.Status)
		return entry.sink(entry.eventCtx, event)
	})
	entry.flow = nil
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(entry.flowCtx.Err(), context.Canceled) {
			m.finish(entry, StatusCancelled, "", "")
			return
		}
		m.finish(entry, StatusFailed, "CRUSH_PROVIDER_LOGIN_FAILED", "Provider authentication failed")
		return
	}
	if entry.flowCtx.Err() != nil {
		m.finish(entry, StatusCancelled, "", "")
		return
	}
	if err := m.backend.SaveCredential(entry.providerID, credential); err != nil {
		m.finish(entry, StatusFailed, "CRUSH_PROVIDER_LOGIN_FAILED", "Provider authentication failed")
		return
	}
	m.finish(entry, StatusAuthenticated, "", "")
}

func (m *Manager) finish(entry *login, status LoginStatus, code, message string) {
	m.mu.Lock()
	if isTerminal(entry.status) {
		m.mu.Unlock()
		return
	}
	entry.status = status
	entry.finishedAt = m.clock()
	m.mu.Unlock()
	if entry.eventCtx.Err() == nil {
		_ = entry.sink(entry.eventCtx, Event{
			LoginID: entry.id, ProviderID: entry.providerID, Status: status,
			ErrorCode: code, Message: message,
		})
	}
	entry.flowCancel()
	entry.eventCancel()
	m.mu.Lock()
	if m.active[entry.providerID] == entry.id {
		delete(m.active, entry.providerID)
	}
	entry.complete = true
	close(entry.done)
	m.mu.Unlock()
}

func (m *Manager) setStatus(entry *login, status LoginStatus) {
	m.mu.Lock()
	if !isTerminal(entry.status) {
		entry.status = status
	}
	m.mu.Unlock()
}

func challengeEvent(entry *login, prompt Prompt) (Event, error) {
	if prompt.Kind != AuthMethodBrowser && prompt.Kind != AuthMethodDeviceCode {
		return Event{}, ErrAuthMethodUnsupported
	}
	if !validPublicURI(prompt.VerificationURI) || len(prompt.UserCode) > maxChallengeBytes || strings.IndexByte(prompt.UserCode, 0) >= 0 {
		return Event{}, errors.New("invalid provider challenge")
	}
	status := StatusWaitingBrowser
	if prompt.Kind == AuthMethodDeviceCode {
		if prompt.UserCode == "" {
			return Event{}, errors.New("invalid provider challenge")
		}
		status = StatusWaitingCode
	}
	return Event{
		LoginID: entry.id, ProviderID: entry.providerID, Status: status,
		VerificationURI: prompt.VerificationURI, UserCode: prompt.UserCode,
		ExpiresAt: prompt.ExpiresAt,
	}, nil
}

func validPublicURI(value string) bool {
	if value == "" || len(value) > maxChallengeBytes || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && parsed.User == nil
}

func validateProviderID(providerID string) error {
	if providerID == "" || len(providerID) > maxProviderID || !utf8.ValidString(providerID) || strings.IndexByte(providerID, 0) >= 0 {
		return ErrProviderNotFound
	}
	for _, r := range providerID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return ErrProviderNotFound
	}
	return nil
}

func validateAPIKey(apiKey string) error {
	if apiKey == "" || len(apiKey) > maxAPIKeyBytes || !utf8.ValidString(apiKey) || strings.IndexByte(apiKey, 0) >= 0 {
		return errors.New("invalid provider api key")
	}
	return nil
}

func providerSupports(providers []Provider, providerID string, method AuthMethod) bool {
	for _, provider := range providers {
		if provider.ID == providerID && slices.Contains(provider.AuthMethods, method) {
			return true
		}
	}
	return false
}

func isTerminal(status LoginStatus) bool {
	return status == StatusAuthenticated || status == StatusFailed || status == StatusCancelled
}

func (m *Manager) pruneLocked() {
	cutoff := m.clock().Add(-m.retention)
	for id, entry := range m.logins {
		if entry.complete && isTerminal(entry.status) && entry.finishedAt.Before(cutoff) {
			delete(m.logins, id)
		}
	}
}

func (m *Manager) evictOldestTerminalLocked() {
	var oldest *login
	for _, entry := range m.logins {
		if !entry.complete || !isTerminal(entry.status) || oldest != nil && !entry.finishedAt.Before(oldest.finishedAt) {
			continue
		}
		oldest = entry
	}
	if oldest != nil {
		delete(m.logins, oldest.id)
	}
}

type apiKeyFlow struct{ apiKey string }

func (f *apiKeyFlow) Run(context.Context, func(Prompt) error) (Credential, error) {
	credential := Credential{APIKey: f.apiKey}
	f.apiKey = ""
	return credential, nil
}
