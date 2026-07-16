// Package providerauth owns safe provider discovery and asynchronous
// authentication lifecycle for desktop clients.
package providerauth

import (
	"context"
	"errors"
	"time"

	"github.com/charmbracelet/crush/internal/oauth"
)

var (
	ErrProviderNotFound      = errors.New("provider not found")
	ErrAuthMethodUnsupported = errors.New("provider authentication method is unsupported")
	ErrLoginInProgress       = errors.New("provider login is already in progress")
	ErrLoginNotFound         = errors.New("provider login not found")
	ErrCapacity              = errors.New("provider login capacity exhausted")
	ErrClosed                = errors.New("provider authentication service is closed")
)

type AuthMethod string

const (
	AuthMethodAPIKey     AuthMethod = "api_key"
	AuthMethodBrowser    AuthMethod = "browser"
	AuthMethodDeviceCode AuthMethod = "device_code"
)

type LoginStatus string

const (
	StatusStarting       LoginStatus = "starting"
	StatusWaitingBrowser LoginStatus = "waiting_browser"
	StatusWaitingCode    LoginStatus = "waiting_code"
	StatusAuthenticated  LoginStatus = "authenticated"
	StatusFailed         LoginStatus = "failed"
	StatusCancelled      LoginStatus = "cancelled"
)

// Provider is a secret-free provider projection.
type Provider struct {
	ID            string
	Name          string
	Type          string
	AuthMethods   []AuthMethod
	Configured    bool
	Authenticated bool
	Disabled      bool
	ModelCount    int
}

// Model is the capability-only model projection used by desktop clients.
type Model struct {
	ProviderID             string
	ID                     string
	Name                   string
	ContextWindow          int64
	MaxOutputTokens        int64
	CanReason              bool
	ReasoningLevels        []string
	DefaultReasoningEffort string
	SupportsImages         bool
}

type AuthStatus struct {
	ProviderID    string
	Authenticated bool
}

// Prompt is the public portion of an interactive authentication challenge.
// Device codes and exchange authorization codes must never be placed here.
type Prompt struct {
	Kind            AuthMethod
	VerificationURI string
	UserCode        string
	ExpiresAt       time.Time
}

// Credential is transient flow output. Callers must never log or serialize it.
type Credential struct {
	APIKey string
	Token  *oauth.Token
}

type Event struct {
	LoginID         string
	ProviderID      string
	Status          LoginStatus
	VerificationURI string
	UserCode        string
	ExpiresAt       time.Time
	ErrorCode       string
	Message         string
}

type EventSink func(context.Context, Event) error

type Flow interface {
	Run(context.Context, func(Prompt) error) (Credential, error)
}

type FlowFactory interface {
	New(string) Flow
}

type FlowFactoryFunc func(string) Flow

func (f FlowFactoryFunc) New(providerID string) Flow { return f(providerID) }

type FlowKey struct {
	ProviderID string
	Method     AuthMethod
}

// Backend is the secret-holding boundary behind the manager.
type Backend interface {
	Providers() []Provider
	Models(string) ([]Model, error)
	AuthStatus(string) (AuthStatus, error)
	SaveCredential(string, Credential) error
	ClearCredential(string) error
}
