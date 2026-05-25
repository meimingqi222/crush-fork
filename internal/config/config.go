package config

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/invopop/jsonschema"
)

const (
	appName              = "crush"
	defaultDataDirectory = ".crush"
	defaultInitializeAs  = "AGENTS.md"
)

var defaultContextPaths = []string{
	".github/copilot-instructions.md",
	".cursorrules",
	".cursor/rules/",
	"CLAUDE.md",
	"CLAUDE.local.md",
	"GEMINI.md",
	"gemini.md",
	"crush.md",
	"crush.local.md",
	"Crush.md",
	"Crush.local.md",
	"CRUSH.md",
	"CRUSH.local.md",
	"AGENTS.md",
	"agents.md",
	"Agents.md",
}

// DefaultContextPaths returns the default set of context file paths
// probed for project-specific AI instructions.
func DefaultContextPaths() []string {
	return append([]string(nil), defaultContextPaths...)
}

type SelectedModelType string

// String returns the string representation of the [SelectedModelType].
func (s SelectedModelType) String() string {
	return string(s)
}

const (
	SelectedModelTypeAutoClassifier SelectedModelType = "auto_classifier"
	SelectedModelTypeLarge          SelectedModelType = "large"
	SelectedModelTypeSmall          SelectedModelType = "small"
	SelectedModelTypeBackground     SelectedModelType = "background_model"
	SelectedModelTypePlan           SelectedModelType = "plan"
	SelectedModelTypeReview         SelectedModelType = "review"
	SelectedModelTypeDesigner       SelectedModelType = "designer"
	SelectedModelTypeLibrarian      SelectedModelType = "librarian"
	SelectedModelTypeQuickTask      SelectedModelType = "quick_task"

	// Deprecated: kept only for backward-compatible config loading.
	SelectedModelTypeHandoff SelectedModelType = "handoff"

	// Deprecated: kept only for backward-compatible config loading.
	SelectedModelTypeAutoClassifierFast SelectedModelType = "auto_classifier_fast"
	// Deprecated: kept only for backward-compatible config loading.
	SelectedModelTypeAutoClassifierReasoning SelectedModelType = "auto_classifier_reasoning"
)

const (
	AgentCoder     string = "coder"
	AgentTask      string = "task"
	AgentGeneral   string = "general"
	AgentExplore   string = "explore"
	AgentPlan      string = "plan"
	AgentReview    string = "review"
	AgentDesigner  string = "designer"
	AgentLibrarian string = "librarian"
	AgentQuickTask string = "quick_task"
)

type AgentMode string

const (
	AgentModePrimary  AgentMode = "primary"
	AgentModeSubagent AgentMode = "subagent"
	AgentModeAll      AgentMode = "all"
)

func NormalizeAgentMode(mode AgentMode) AgentMode {
	switch mode {
	case AgentModePrimary, AgentModeSubagent, AgentModeAll:
		return mode
	default:
		return AgentModeAll
	}
}

func CanonicalSubagentID(id string) string {
	switch strings.TrimSpace(id) {
	case "", AgentTask:
		return AgentExplore
	default:
		return strings.TrimSpace(id)
	}
}

func RequestedSubagentID(id string) string {
	switch strings.TrimSpace(id) {
	case "", AgentTask:
		return AgentExplore
	case "planner":
		return AgentPlan
	case "reviewer":
		return AgentReview
	case "quick", "quick-task":
		return AgentQuickTask
	default:
		return strings.TrimSpace(id)
	}
}

func ResolveSubagentID(agents map[string]Agent, id string) string {
	canonicalID := CanonicalSubagentID(id)
	if _, ok := agents[canonicalID]; ok {
		return canonicalID
	}
	return RequestedSubagentID(id)
}

type SelectedModel struct {
	// The model id as used by the provider API.
	// Required.
	Model string `json:"model" jsonschema:"required,description=The model ID as used by the provider API,example=gpt-4o"`
	// The model provider, same as the key/id used in the providers config.
	// Required.
	Provider string `json:"provider" jsonschema:"required,description=The model provider ID that matches a key in the providers config,example=openai"`

	// Overrides the default model configuration.
	MaxTokens        int64    `json:"max_tokens,omitempty" jsonschema:"description=Maximum number of tokens for model responses,maximum=200000,example=4096"`
	ContextWindow    int64    `json:"context_window,omitempty" jsonschema:"description=Context window override for this model,example=400000"`
	Temperature      *float64 `json:"temperature,omitempty" jsonschema:"description=Sampling temperature,minimum=0,maximum=1,example=0.7"`
	TopP             *float64 `json:"top_p,omitempty" jsonschema:"description=Top-p (nucleus) sampling parameter,minimum=0,maximum=1,example=0.9"`
	TopK             *int64   `json:"top_k,omitempty" jsonschema:"description=Top-k sampling parameter"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty" jsonschema:"description=Frequency penalty to reduce repetition"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty" jsonschema:"description=Presence penalty to increase topic diversity"`

	// Deprecated: Use model's default_reasoning_effort in provider config instead.
	// This field is kept for backward compatibility but is no longer used.
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"description=Deprecated: Use model's default_reasoning_effort in provider config instead,enum=low,enum=medium,enum=high"`

	// MaxPromptTokens optionally overrides the model prompt/input token budget.
	// This is primarily useful for OpenAI-compatible Responses endpoints where
	// max_prompt_tokens can be lower than the advertised context window.
	MaxPromptTokens int64 `json:"max_prompt_tokens,omitempty" jsonschema:"description=Maximum prompt/input token budget override for this model,example=262144"`

	// Think controls whether to enable thinking/reasoning mode for models that
	// support it. When nil (the default), thinking is enabled for all CanReason
	// models. Set to false to explicitly disable thinking across all providers.
	Think *bool `json:"think,omitempty" jsonschema:"description=Enable thinking/reasoning mode for models that support it (applies to all providers)"`

	// Deprecated: Use model's options.provider_options in provider config instead.
	// This field is kept for backward compatibility but is no longer used.
	ProviderOptions map[string]any `json:"provider_options,omitempty" jsonschema:"description=Deprecated: Use model's options.provider_options in provider config instead"`
}

type ProviderConfig struct {
	// The provider's id.
	ID string `json:"id,omitempty" jsonschema:"description=Unique identifier for the provider,example=openai"`
	// The provider's name, used for display purposes.
	Name string `json:"name,omitempty" jsonschema:"description=Human-readable name for the provider,example=OpenAI"`
	// The provider's API endpoint.
	BaseURL string `json:"base_url,omitempty" jsonschema:"description=Base URL for the provider's API,format=uri,example=https://api.openai.com/v1"`
	// The provider type, e.g. "openai", "anthropic", etc. if empty it defaults to openai.
	Type catwalk.Type `json:"type,omitempty" jsonschema:"description=Provider type that determines the API format,enum=openai,enum=openai-compat,enum=anthropic,enum=gemini,enum=azure,enum=vertexai,default=openai"`
	// The provider's API key.
	APIKey string `json:"api_key,omitempty" jsonschema:"description=API key for authentication with the provider,example=$OPENAI_API_KEY"`
	// The original API key template before resolution (for re-resolution on auth errors).
	APIKeyTemplate string `json:"-"`
	// OAuthToken for providers that use OAuth2 authentication.
	OAuthToken *oauth.Token `json:"oauth,omitempty" jsonschema:"description=OAuth2 token for authentication with the provider"`
	// Marks the provider as disabled.
	Disable bool `json:"disable,omitempty" jsonschema:"description=Whether this provider is disabled,default=false"`

	// Custom system prompt prefix.
	SystemPromptPrefix string `json:"system_prompt_prefix,omitempty" jsonschema:"description=Custom prefix to add to system prompts for this provider"`

	// Extra headers to send with each request to the provider.
	ExtraHeaders map[string]string `json:"extra_headers,omitempty" jsonschema:"description=Additional HTTP headers to send with requests"`
	// Extra body
	ExtraBody map[string]any `json:"extra_body,omitempty" jsonschema:"description=Additional fields to include in request bodies, only works with openai-compatible providers"`

	// UseCopilotClient instructs providers to use the GitHub Copilot OAuth HTTP
	// client. This adds the X-Initiator header and applies response normalization
	// for Copilot-specific reasoning fields. Supported provider types:
	// openai-compat, openai (Responses API), and anthropic.
	UseCopilotClient bool `json:"use_copilot_client,omitempty" jsonschema:"description=Use the GitHub Copilot OAuth HTTP client for this provider (openai-compat, openai, anthropic),default=false"`

	ProviderOptions map[string]any `json:"provider_options,omitempty" jsonschema:"description=Additional provider-specific options for this provider"`

	// ResponsesWebSocket enables OpenAI Responses streaming over WebSocket.
	// When false, streaming uses HTTP SSE for compatibility.
	ResponsesWebSocket bool `json:"responses_websocket,omitempty" jsonschema:"description=Use WebSocket transport for OpenAI Responses streaming when supported,default=false"`

	// CopilotService indicates if this provider follows GitHub Copilot billing rules.
	// When true, X-Initiator header is set based on request type:
	// - "user" for direct user prompts (billable)
	// - "agent" for tool calls, sub-agents, auto-summaries (free)
	CopilotService bool `json:"copilot_service,omitempty" jsonschema:"description=Enable GitHub Copilot billing rules with X-Initiator header. When true, only direct user prompts are billable (X-Initiator: user), while tool calls, sub-agents, and auto-summaries are free (X-Initiator: agent),default=false"`

	// Used to pass extra parameters to the provider.
	ExtraParams map[string]string `json:"-"`

	// The provider models
	Models []catwalk.Model `json:"models,omitempty" jsonschema:"description=List of models available from this provider"`
}

// ToProvider converts the [ProviderConfig] to a [catwalk.Provider].
func (pc *ProviderConfig) ToProvider() catwalk.Provider {
	// Convert config provider to provider.Provider format
	provider := catwalk.Provider{
		Name:   pc.Name,
		ID:     catwalk.InferenceProvider(pc.ID),
		Models: make([]catwalk.Model, len(pc.Models)),
	}

	// Convert models
	for i, model := range pc.Models {
		provider.Models[i] = catwalk.Model{
			ID:                     model.ID,
			Name:                   model.Name,
			CostPer1MIn:            model.CostPer1MIn,
			CostPer1MOut:           model.CostPer1MOut,
			CostPer1MInCached:      model.CostPer1MInCached,
			CostPer1MOutCached:     model.CostPer1MOutCached,
			ContextWindow:          model.ContextWindow,
			DefaultMaxTokens:       model.DefaultMaxTokens,
			CanReason:              model.CanReason,
			ReasoningLevels:        model.ReasoningLevels,
			DefaultReasoningEffort: model.DefaultReasoningEffort,
			SupportsImages:         model.SupportsImages,
		}
	}

	return provider
}

func (pc *ProviderConfig) SetupGitHubCopilot() {
	maps.Copy(pc.ExtraHeaders, copilot.Headers())
}

type MCPType string

const (
	MCPStdio MCPType = "stdio"
	MCPSSE   MCPType = "sse"
	MCPHttp  MCPType = "http"
)

type MCPOAuthRegistration struct {
	ClientID     string `json:"client_id,omitempty" jsonschema:"description=Registered OAuth client ID for the MCP server"`
	ClientSecret string `json:"client_secret,omitempty" jsonschema:"description=Registered OAuth client secret for the MCP server"`
}

type MCPOAuthAuthServer struct {
	Issuer                string `json:"issuer,omitempty" jsonschema:"description=OAuth authorization server issuer URL,format=uri"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty" jsonschema:"description=OAuth authorization endpoint for the MCP server,format=uri"`
	TokenEndpoint         string `json:"token_endpoint,omitempty" jsonschema:"description=OAuth token endpoint for the MCP server,format=uri"`
	RegistrationEndpoint  string `json:"registration_endpoint,omitempty" jsonschema:"description=OAuth dynamic client registration endpoint for the MCP server,format=uri"`
}

type MCPOAuthConfig struct {
	Enabled      bool                  `json:"enabled,omitempty" jsonschema:"description=Enable OAuth authentication for HTTP MCP servers,default=false"`
	ClientName   string                `json:"client_name,omitempty" jsonschema:"description=Client name used during MCP OAuth registration,example=Crush"`
	RedirectURL  string                `json:"redirect_url,omitempty" jsonschema:"description=Loopback redirect URL override for MCP OAuth callbacks,format=uri,example=http://127.0.0.1:8913/callback"`
	ClientID     string                `json:"client_id,omitempty" jsonschema:"description=Pre-registered OAuth client ID for the MCP server"`
	ClientSecret string                `json:"client_secret,omitempty" jsonschema:"description=Pre-registered OAuth client secret for the MCP server"`
	Token        *oauth.Token          `json:"token,omitempty" jsonschema:"description=Persisted OAuth token for the MCP server"`
	Registration *MCPOAuthRegistration `json:"registration,omitempty" jsonschema:"description=Persisted OAuth client registration for the MCP server"`
	AuthServer   *MCPOAuthAuthServer   `json:"auth_server,omitempty" jsonschema:"description=Discovered OAuth authorization server metadata for the MCP server"`
	Resource     string                `json:"resource,omitempty" jsonschema:"description=Protected resource identifier for the MCP server,format=uri"`
	Scopes       []string              `json:"scopes,omitempty" jsonschema:"description=OAuth scopes granted for the MCP server"`
}

type MCPConfig struct {
	Command       string            `json:"command,omitempty" jsonschema:"description=Command to execute for stdio MCP servers,example=npx"`
	Env           map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set for the MCP server"`
	Args          []string          `json:"args,omitempty" jsonschema:"description=Arguments to pass to the MCP server command"`
	Type          MCPType           `json:"type" jsonschema:"required,description=Type of MCP connection,enum=stdio,enum=sse,enum=http,default=stdio"`
	URL           string            `json:"url,omitempty" jsonschema:"description=URL for HTTP or SSE MCP servers,format=uri,example=http://localhost:3000/mcp"`
	Disabled      bool              `json:"disabled,omitempty" jsonschema:"description=Whether this MCP server is disabled,default=false"`
	DisabledTools []string          `json:"disabled_tools,omitempty" jsonschema:"description=List of tools from this MCP server to disable,example=get-library-doc"`
	EnabledTools  []string          `json:"enabled_tools,omitempty" jsonschema:"description=List of tools from this MCP server to enable exclusively (whitelist). If empty, all non-disabled tools are enabled.,example=use-library-doc"`
	Timeout       int               `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for MCP server connections,default=15,example=30,example=60,example=120"`
	OAuth         *MCPOAuthConfig   `json:"oauth,omitempty" jsonschema:"description=OAuth configuration for HTTP MCP servers"`

	// TODO: maybe make it possible to get the value from the env
	Headers map[string]string `json:"headers,omitempty" jsonschema:"description=HTTP headers for HTTP/SSE MCP servers"`
}

type LSPConfig struct {
	Disabled    bool              `json:"disabled,omitempty" jsonschema:"description=Whether this LSP server is disabled,default=false"`
	Command     string            `json:"command,omitempty" jsonschema:"description=Command to execute for the LSP server,example=gopls"`
	Args        []string          `json:"args,omitempty" jsonschema:"description=Arguments to pass to the LSP server command"`
	Env         map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set to the LSP server command"`
	FileTypes   []string          `json:"filetypes,omitempty" jsonschema:"description=File types this LSP server handles,example=go,example=mod,example=rs,example=c,example=js,example=ts"`
	RootMarkers []string          `json:"root_markers,omitempty" jsonschema:"description=Files or directories that indicate the project root,example=go.mod,example=package.json,example=Cargo.toml"`
	InitOptions map[string]any    `json:"init_options,omitempty" jsonschema:"description=Initialization options passed to the LSP server during initialize request"`
	Options     map[string]any    `json:"options,omitempty" jsonschema:"description=LSP server-specific settings passed during initialization"`
	Timeout     int               `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for LSP server initialization,default=30,example=60,example=120"`
}

type TUIOptions struct {
	CompactMode bool   `json:"compact_mode,omitempty" jsonschema:"description=Enable compact mode for the TUI interface,default=false"`
	DiffMode    string `json:"diff_mode,omitempty" jsonschema:"description=Diff mode for the TUI interface,enum=unified,enum=split"`
	// Here we can add themes later or any TUI related options

	Completions Completions `json:"completions,omitzero" jsonschema:"description=Completions UI options"`
	Transparent *bool       `json:"transparent,omitempty" jsonschema:"description=Enable transparent background for the TUI interface,default=false"`
}

// MemoryConfig defines configuration for the memory engine.
type MemoryConfig struct {
	// Enabled enables the event-sourced memory engine.
	Enabled *bool `json:"enabled,omitempty" jsonschema:"description=Enable the event-sourced memory engine,default=true"`
	// Backend selects the memory backend. "local" uses the local event log and
	// materialized files. "hindsight" retains raw transcript windows to a remote
	// Hindsight service every N turns and uses Hindsight as the recall/reflect
	// backend. "off" disables memory. When empty, Remote implies "hindsight";
	// otherwise "local".
	Backend string `json:"backend,omitempty" jsonschema:"description=Memory backend to use,enum=local,enum=hindsight,enum=off,default=local"`
	// Remote is the base URL of a remote Hindsight memory service (e.g. http://localhost:8888).
	// Required when backend is "hindsight".
	Remote string `json:"remote,omitempty" jsonschema:"description=Remote Hindsight memory service base URL (e.g. http://localhost:8888),format=uri"`
	// RemoteToken is the Bearer token for authenticating with the remote service.
	// Reads from HINDSIGHT_API_TOKEN environment variable when empty.
	RemoteToken string `json:"remote_token,omitempty" jsonschema:"description=Bearer token for the remote memory service (or set HINDSIGHT_API_TOKEN env var)"`
	// RemoteBankID is the memory bank identifier on the remote service.
	// Defaults to "crush" when empty.
	RemoteBankID string `json:"remote_bank_id,omitempty" jsonschema:"description=Memory bank ID on the remote service,default=crush"`
	// RemoteScoping controls how Hindsight memory is partitioned.
	// "global" uses one shared bank, "per-project" appends the project slug to
	// the bank ID, and "per-project-tagged" uses one bank with project tags.
	RemoteScoping string `json:"remote_scoping,omitempty" jsonschema:"description=Remote Hindsight scoping mode,enum=global,enum=per-project,enum=per-project-tagged,default=per-project-tagged"`
	// RetainEveryNTurns controls how often transcript windows are retained
	// when using the hindsight backend. Defaults to 3.
	RetainEveryNTurns int `json:"retain_every_n_turns,omitempty" jsonschema:"description=Retain transcript window every N turns (hindsight backend),default=3"`

	// MentalModels controls the layered Mental Models materializer that
	// generates stable, low-frequency-updated summary files for user
	// preferences, project conventions, decisions, and known pitfalls.
	// Enabled by default; disable via mental_models.enabled=false.
	MentalModels *MemoryMentalModelsConfig `json:"mental_models,omitempty" jsonschema:"description=Mental Models materializer configuration"`

	// BackgroundMaterialize controls the periodic background materializer
	// that keeps materialized views up to date during long sessions.
	BackgroundMaterialize *MemoryBackgroundMaterializeConfig `json:"background_materialize,omitempty" jsonschema:"description=Background materializer configuration"`

	// CompactionRecall controls active recall during pre-compaction so the
	// summary prompt receives the most relevant past memories.
	CompactionRecall *MemoryCompactionRecallConfig `json:"compaction_recall,omitempty" jsonschema:"description=Compaction-time recall configuration"`

	// Reranker controls optional reranking of FTS5 candidates before they
	// are returned by Retrieve(). Heuristic has zero model cost; embedding
	// uses local lightweight hashing by default and is only wired for the
	// local backend. Hindsight uses its remote recall implementation instead.
	Reranker *MemoryRerankerConfig `json:"reranker,omitempty" jsonschema:"description=Retrieve reranker configuration"`

	// Embeddings controls the optional local embedding reranker. The default
	// hashing backend downloads no model files and only reranks candidates.
	Embeddings *MemoryEmbeddingsConfig `json:"embeddings,omitempty" jsonschema:"description=Local memory embedding reranker configuration"`

	// Rollout controls the per-session rollout summary materializer.
	Rollout *MemoryRolloutConfig `json:"rollout,omitempty" jsonschema:"description=Per-session rollout summary configuration"`
}

// MemoryMentalModelsConfig configures the Mental Models materializer.
type MemoryMentalModelsConfig struct {
	Enabled       *bool   `json:"enabled,omitempty" jsonschema:"description=Enable Mental Models materializer,default=true"`
	MaxBytesShare float64 `json:"max_bytes_share,omitempty" jsonschema:"description=Maximum fraction of recall budget allocated to mental models,default=0.5"`
}

// IsEnabled returns true unless explicitly disabled.
func (c *MemoryMentalModelsConfig) IsEnabled() bool {
	if c == nil || c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// GetMaxBytesShare returns the configured share (0..1] with default 0.5.
func (c *MemoryMentalModelsConfig) GetMaxBytesShare() float64 {
	if c == nil || c.MaxBytesShare <= 0 || c.MaxBytesShare > 1 {
		return 0.5
	}
	return c.MaxBytesShare
}

// MemoryBackgroundMaterializeConfig configures background materialization.
type MemoryBackgroundMaterializeConfig struct {
	Enabled     *bool  `json:"enabled,omitempty" jsonschema:"description=Enable background materializer,default=true"`
	IntervalSec int    `json:"interval_seconds,omitempty" jsonschema:"description=Background materialization interval in seconds,default=300"`
	EveryNTurns int    `json:"every_n_turns,omitempty" jsonschema:"description=Force materialization every N idle turns,default=10"`
	_           string `json:"-"`
}

// IsEnabled returns false unless explicitly enabled.
func (c *MemoryBackgroundMaterializeConfig) IsEnabled() bool {
	if c == nil || c.Enabled == nil {
		return false
	}
	return *c.Enabled
}

// GetIntervalSeconds returns the configured interval (>0) with default 300s.
func (c *MemoryBackgroundMaterializeConfig) GetIntervalSeconds() int {
	if c == nil || c.IntervalSec <= 0 {
		return 300
	}
	return c.IntervalSec
}

// GetEveryNTurns returns the per-turn cadence (>0) with default 10.
func (c *MemoryBackgroundMaterializeConfig) GetEveryNTurns() int {
	if c == nil || c.EveryNTurns <= 0 {
		return 10
	}
	return c.EveryNTurns
}

// MemoryCompactionRecallConfig configures pre-compaction recall.
type MemoryCompactionRecallConfig struct {
	Enabled   *bool `json:"enabled,omitempty" jsonschema:"description=Enable compaction-time recall,default=true"`
	TopK      int   `json:"top_k,omitempty" jsonschema:"description=Maximum events to inject into compaction prompt,default=5"`
	MaxBytes  int   `json:"max_bytes,omitempty" jsonschema:"description=Maximum byte budget for compaction rescue payload,default=2048"`
	UseRerank *bool `json:"use_rerank,omitempty" jsonschema:"description=Use the configured reranker during compaction recall,default=false"`
}

// IsEnabled returns true unless explicitly disabled.
func (c *MemoryCompactionRecallConfig) IsEnabled() bool {
	if c == nil || c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// GetTopK returns the configured top-K with default 5.
func (c *MemoryCompactionRecallConfig) GetTopK() int {
	if c == nil || c.TopK <= 0 {
		return 5
	}
	return c.TopK
}

// GetMaxBytes returns the configured byte budget with default 2048.
func (c *MemoryCompactionRecallConfig) GetMaxBytes() int {
	if c == nil || c.MaxBytes <= 0 {
		return 2048
	}
	return c.MaxBytes
}

// GetUseRerank returns whether to invoke the reranker during compaction recall.
func (c *MemoryCompactionRecallConfig) GetUseRerank() bool {
	if c == nil || c.UseRerank == nil {
		return false
	}
	return *c.UseRerank
}

// MemoryRerankerConfig configures retrieve reranking.
type MemoryRerankerConfig struct {
	Enabled       *bool  `json:"enabled,omitempty" jsonschema:"description=Enable reranker on Retrieve(),default=false"`
	Type          string `json:"type,omitempty" jsonschema:"description=Reranker implementation,enum=heuristic,enum=embedding,enum=hybrid,enum=llm,default=heuristic"`
	MaxCandidates int    `json:"max_candidates,omitempty" jsonschema:"description=Maximum candidates fed to the reranker,default=30"`
}

// IsEnabled returns false unless explicitly enabled.
func (c *MemoryRerankerConfig) IsEnabled() bool {
	if c == nil || c.Enabled == nil {
		return false
	}
	return *c.Enabled
}

// GetType returns the reranker type with default "heuristic".
func (c *MemoryRerankerConfig) GetType() string {
	if c == nil || strings.TrimSpace(c.Type) == "" {
		return "heuristic"
	}
	return strings.ToLower(strings.TrimSpace(c.Type))
}

// GetMaxCandidates returns the configured candidate cap with default 30.
func (c *MemoryRerankerConfig) GetMaxCandidates() int {
	if c == nil || c.MaxCandidates <= 0 {
		return 30
	}
	return c.MaxCandidates
}

// MemoryEmbeddingsConfig configures the local embedding reranker.
type MemoryEmbeddingsConfig struct {
	Enabled    *bool  `json:"enabled,omitempty" jsonschema:"description=Enable local embedding reranker,default=false"`
	Backend    string `json:"backend,omitempty" jsonschema:"description=Embedding backend,enum=hashing,default=hashing"`
	Dimensions int    `json:"dimensions,omitempty" jsonschema:"description=Hashing embedding dimensions,default=384"`
}

// IsEnabled returns false unless explicitly enabled.
func (c *MemoryEmbeddingsConfig) IsEnabled() bool {
	if c == nil || c.Enabled == nil {
		return false
	}
	return *c.Enabled
}

// BackendName returns the embedding backend with default "hashing".
func (c *MemoryEmbeddingsConfig) BackendName() string {
	if c == nil || strings.TrimSpace(c.Backend) == "" {
		return "hashing"
	}
	return strings.ToLower(strings.TrimSpace(c.Backend))
}

// GetDimensions returns the configured vector dimensions with default 384.
func (c *MemoryEmbeddingsConfig) GetDimensions() int {
	if c == nil || c.Dimensions <= 0 {
		return 384
	}
	return c.Dimensions
}

// MemoryRolloutConfig configures per-session rollout summaries.
type MemoryRolloutConfig struct {
	Enabled   *bool `json:"enabled,omitempty" jsonschema:"description=Enable per-session rollout summary materializer,default=true"`
	MaxKeep   int   `json:"max_keep,omitempty" jsonschema:"description=Maximum rollout files retained on disk,default=200"`
	MinEvents int   `json:"min_events,omitempty" jsonschema:"description=Minimum durable events before a rollout summary is written,default=3"`
}

// IsEnabled returns true unless explicitly disabled.
func (c *MemoryRolloutConfig) IsEnabled() bool {
	if c == nil || c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// GetMaxKeep returns the max files retained (>0) with default 200.
func (c *MemoryRolloutConfig) GetMaxKeep() int {
	if c == nil || c.MaxKeep <= 0 {
		return 200
	}
	return c.MaxKeep
}

// GetMinEvents returns the minimum event count threshold with default 3.
func (c *MemoryRolloutConfig) GetMinEvents() int {
	if c == nil || c.MinEvents <= 0 {
		return 3
	}
	return c.MinEvents
}

// BackendName returns the effective memory backend.
func (m *MemoryConfig) BackendName() string {
	if m == nil {
		return "local"
	}
	backend := strings.ToLower(strings.TrimSpace(m.Backend))
	switch backend {
	case "off", "none", "disabled":
		return "off"
	case "hindsight", "remote":
		return "hindsight"
	case "local":
		return "local"
	}
	if strings.TrimSpace(m.Remote) != "" {
		return "hindsight"
	}
	return "local"
}

// RemoteScopingName returns the effective Hindsight scoping mode.
func (m *MemoryConfig) RemoteScopingName() string {
	if m == nil {
		return "per-project-tagged"
	}
	scoping := strings.ToLower(strings.TrimSpace(m.RemoteScoping))
	switch scoping {
	case "global":
		return "global"
	case "per-project", "project":
		return "per-project"
	case "per-project-tagged", "project-tagged", "tagged", "":
		return "per-project-tagged"
	default:
		return "per-project-tagged"
	}
}

// IsEnabled reports whether the memory engine should run. The local engine is
// on by default; users can explicitly disable it with memory.enabled=false or
// memory.backend=off.
func (m *MemoryConfig) IsEnabled() bool {
	if m != nil && m.Enabled != nil && !*m.Enabled {
		return false
	}
	return m.BackendName() != "off"
}

// GetRetainEveryNTurns returns the configured retain interval for the hindsight backend.
// Defaults to 3 if not set or invalid.
func (m *MemoryConfig) GetRetainEveryNTurns() int {
	if m == nil || m.RetainEveryNTurns <= 0 {
		return 3
	}
	return m.RetainEveryNTurns
}

// Completions defines options for the completions UI.
type Completions struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum depth for directory listings,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of items to return for directory listings,default=1000,example=100"`
}

func (c Completions) Limits() (depth, items int) {
	return ptrValOr(c.MaxDepth, 0), ptrValOr(c.MaxItems, 0)
}

type Permissions struct {
	AllowedTools                []string  `json:"allowed_tools,omitempty" jsonschema:"description=List of tools that don't require permission prompts,example=bash,example=read"` // Tools that don't require permission prompts
	SkipRequests                bool      `json:"-"`                                                                                                                              // Automatically accept all permissions (YOLO mode)
	FailClosedOnClassifierError bool      `json:"fail_closed_on_classifier_error,omitempty" jsonschema:"description=Block permission-requiring actions when Auto Mode permission classification is unavailable instead of falling back to manual confirmation,default=false"`
	AutoMode                    *AutoMode `json:"auto_mode,omitempty" jsonschema:"description=Auto Mode policy customization"`
}

type ApprovalPolicy string

const (
	ApprovalPolicyUntrusted ApprovalPolicy = "untrusted"
	ApprovalPolicyOnFailure ApprovalPolicy = "on-failure"
	ApprovalPolicyOnRequest ApprovalPolicy = "on-request"
	ApprovalPolicyGranular  ApprovalPolicy = "granular"
	ApprovalPolicyNever     ApprovalPolicy = "never"
)

type AutoMode struct {
	Environment        []string                `json:"environment,omitempty" jsonschema:"description=Additional environment facts injected into Auto Mode classifier prompts"`
	BlockRules         []string                `json:"block_rules,omitempty" jsonschema:"description=Additional block rules for Auto Mode guards"`
	AllowExceptions    []string                `json:"allow_exceptions,omitempty" jsonschema:"description=Additional allow-list exceptions for Auto Mode guards"`
	ApprovalPolicy     ApprovalPolicy          `json:"approval_policy,omitempty" jsonschema:"description=Codex-style approval policy for Auto Mode decisions,enum=untrusted,enum=on-failure,enum=on-request,enum=granular,enum=never,default=untrusted"`
	Granular           *GranularApprovalConfig `json:"granular,omitempty" jsonschema:"description=Fine-grained approval gates used when approval_policy is granular"`
	ExecPolicyRules    []ExecPolicyRule        `json:"exec_policy_rules,omitempty" jsonschema:"description=Deterministic shell command approval rules evaluated before Auto Mode guardian review"`
	UseGuardianReview  *bool                   `json:"use_guardian_review,omitempty" jsonschema:"description=Use Auto Mode guardian review for policy prompts instead of directly prompting the user,default=true"`
	WorkspaceWriteMode string                  `json:"workspace_write_mode,omitempty" jsonschema:"description=How Auto Mode handles non-sensitive workspace edits,enum=allow,enum=ask,enum=forbid,default=allow"`
}

type GranularApprovalConfig struct {
	SandboxApproval    bool `json:"sandbox_approval,omitempty" jsonschema:"description=Allow shell/sandbox approval prompts in granular Auto Mode,default=true"`
	Rules              bool `json:"rules,omitempty" jsonschema:"description=Allow exec policy rule approval prompts in granular Auto Mode,default=true"`
	SkillApproval      bool `json:"skill_approval,omitempty" jsonschema:"description=Allow skill approval prompts in granular Auto Mode,default=true"`
	RequestPermissions bool `json:"request_permissions,omitempty" jsonschema:"description=Allow request-permissions approval prompts in granular Auto Mode,default=true"`
	MCPElicitations    bool `json:"mcp_elicitations,omitempty" jsonschema:"description=Allow MCP elicitation approval prompts in granular Auto Mode,default=true"`
}

type ExecPolicyRule struct {
	Decision string   `json:"decision" jsonschema:"required,description=Rule decision,enum=allow,enum=prompt,enum=forbid"`
	Exact    []string `json:"exact,omitempty" jsonschema:"description=Exact command names or full command lines matched case-insensitively"`
	Prefix   []string `json:"prefix,omitempty" jsonschema:"description=Command-line prefixes matched case-insensitively"`
	Reason   string   `json:"reason,omitempty" jsonschema:"description=Reason shown when this rule matches"`
}

type TrailerStyle string

const (
	TrailerStyleNone         TrailerStyle = "none"
	TrailerStyleCoAuthoredBy TrailerStyle = "co-authored-by"
	TrailerStyleAssistedBy   TrailerStyle = "assisted-by"
)

type Attribution struct {
	TrailerStyle  TrailerStyle `json:"trailer_style,omitempty" jsonschema:"description=Style of attribution trailer to add to commits,enum=none,enum=co-authored-by,enum=assisted-by,default=assisted-by"`
	CoAuthoredBy  *bool        `json:"co_authored_by,omitempty" jsonschema:"description=Deprecated: use trailer_style instead"`
	GeneratedWith bool         `json:"generated_with,omitempty" jsonschema:"description=Add Generated with Crush line to commit messages and issues and PRs,default=true"`
}

// JSONSchemaExtend marks the co_authored_by field as deprecated in the schema.
func (Attribution) JSONSchemaExtend(schema *jsonschema.Schema) {
	if schema.Properties != nil {
		if prop, ok := schema.Properties.Get("co_authored_by"); ok {
			prop.Deprecated = true
		}
	}
}

type Options struct {
	ContextPaths               []string      `json:"context_paths,omitempty" jsonschema:"description=Paths to files containing context information for the AI,example=.cursorrules,example=CRUSH.md"`
	SkillsPaths                []string      `json:"skills_paths,omitempty" jsonschema:"description=Paths to directories containing Agent Skills (folders with SKILL.md files),example=~/.config/crush/skills,example=./skills"`
	TUI                        *TUIOptions   `json:"tui,omitempty" jsonschema:"description=Terminal user interface options"`
	PreferredPermissionMode    string        `json:"preferred_permission_mode,omitempty" jsonschema:"description=Default interactive permission mode for new sessions,enum=default,enum=auto,enum=yolo,default=auto"`
	PreferredCollaborationMode string        `json:"preferred_collaboration_mode,omitempty" jsonschema:"-"`
	Debug                      bool          `json:"debug,omitempty" jsonschema:"description=Enable debug logging,default=false"`
	DebugLSP                   bool          `json:"debug_lsp,omitempty" jsonschema:"description=Enable debug logging for LSP servers,default=false"`
	DisableAutoSummarize       bool          `json:"disable_auto_summarize,omitempty" jsonschema:"description=Disable automatic conversation summarization,default=false"`
	DataDirectory              string        `json:"data_directory,omitempty" jsonschema:"description=Directory for storing project-scoped application data (defaults to .crush in the working directory; falls back to a safe global workspace path when startup cwd is unsafe),default=.crush,example=.crush"`
	DisabledTools              []string      `json:"disabled_tools,omitempty" jsonschema:"description=List of built-in tools to disable and hide from the agent,example=bash,example=sourcegraph"`
	DisableProviderAutoUpdate  bool          `json:"disable_provider_auto_update,omitempty" jsonschema:"description=Disable providers auto-update,default=false"`
	DisableDefaultProviders    bool          `json:"disable_default_providers,omitempty" jsonschema:"description=Ignore all default/embedded providers. When enabled, providers must be fully specified in the config file with base_url, models, and api_key - no merging with defaults occurs,default=false"`
	Memory                     *MemoryConfig `json:"memory,omitempty" jsonschema:"description=Memory engine configuration"`
	Attribution                *Attribution  `json:"attribution,omitempty" jsonschema:"description=Attribution settings for generated content"`
	DisableMetrics             bool          `json:"disable_metrics,omitempty" jsonschema:"description=Disable sending metrics,default=false"`
	InitializeAs               string        `json:"initialize_as,omitempty" jsonschema:"description=Name of the context file to create/update during project initialization,default=AGENTS.md,example=AGENTS.md,example=CRUSH.md,example=CLAUDE.md,example=docs/LLMs.md"`
	AutoLSP                    *bool         `json:"auto_lsp,omitempty" jsonschema:"description=Automatically setup LSPs based on root markers,default=true"`
	Progress                   *bool         `json:"progress,omitempty" jsonschema:"description=Show indeterminate progress updates during long operations,default=true"`
	DisableNotifications       bool          `json:"disable_notifications,omitempty" jsonschema:"description=Disable desktop notifications,default=false"`
}

type MCPs map[string]MCPConfig

type MCP struct {
	Name string    `json:"name"`
	MCP  MCPConfig `json:"mcp"`
}

func (m MCPs) Sorted() []MCP {
	sorted := make([]MCP, 0, len(m))
	for k, v := range m {
		sorted = append(sorted, MCP{
			Name: k,
			MCP:  v,
		})
	}
	slices.SortFunc(sorted, func(a, b MCP) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sorted
}

type LSPs map[string]LSPConfig

type LSP struct {
	Name string    `json:"name"`
	LSP  LSPConfig `json:"lsp"`
}

func (l LSPs) Sorted() []LSP {
	sorted := make([]LSP, 0, len(l))
	for k, v := range l {
		sorted = append(sorted, LSP{
			Name: k,
			LSP:  v,
		})
	}
	slices.SortFunc(sorted, func(a, b LSP) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sorted
}

func (l LSPConfig) ResolvedEnv() []string {
	return resolveEnvs(l.Env)
}

func (m MCPConfig) ResolvedEnv() []string {
	return resolveEnvs(m.Env)
}

func (m MCPConfig) ResolvedHeaders() map[string]string {
	resolved := make(map[string]string, len(m.Headers))
	resolver := NewShellVariableResolver(env.New())
	for e, v := range m.Headers {
		rv, err := resolver.ResolveValue(v)
		if err != nil {
			slog.Error("Error resolving header variable", "error", err, "variable", e, "value", v)
			resolved[e] = v
			continue
		}
		resolved[e] = rv
	}
	return resolved
}

func (m MCPConfig) OAuthEnabled() bool {
	return m.Type == MCPHttp && m.OAuth != nil && (m.OAuth.Enabled || m.OAuth.Token != nil || m.OAuth.ClientID != "" || m.OAuth.Registration != nil)
}

func (m MCPConfig) SupportsInteractiveAuth() bool {
	return m.Type == MCPHttp
}

type TaskGovernance struct {
	MaxConcurrent  *int  `json:"max_concurrent,omitempty" jsonschema:"description=Maximum number of task graph tasks of this agent type to run concurrently,example=2"`
	TimeoutSeconds *int  `json:"timeout_seconds,omitempty" jsonschema:"description=Maximum number of seconds each task graph task may run before timing out,example=300"`
	FailFast       *bool `json:"fail_fast,omitempty" jsonschema:"description=Stop launching new task graph work after the first task failure,default=false"`

	// Deprecated: RetryBudget is no longer used in the new subagent execution model.
	RetryBudget *int `json:"retry_budget,omitempty" jsonschema:"description=Deprecated: retries are now handled by the parent LLM and this field is ignored.,example=2"`
	// Deprecated: GraphTimeoutSeconds is no longer used in the new subagent execution model.
	GraphTimeoutSeconds *int `json:"graph_timeout_seconds,omitempty" jsonschema:"description=Deprecated: the task graph concept has been removed and this field is ignored.,example=900"`
	// Deprecated: RuntimeBudgetSeconds is no longer used in the new subagent execution model.
	RuntimeBudgetSeconds *int `json:"runtime_budget_seconds,omitempty" jsonschema:"description=Deprecated: shared runtime budgets are no longer enforced and this field is ignored.,example=600"`
	// Deprecated: FailureBudget is no longer used in the new subagent execution model.
	FailureBudget *int `json:"failure_budget,omitempty" jsonschema:"description=Deprecated: failure budgets are no longer enforced and this field is ignored.,example=2"`
	// Deprecated: FailureDomain is no longer used in the new subagent execution model.
	FailureDomain string `json:"failure_domain,omitempty" jsonschema:"description=Deprecated: failure domains are no longer enforced and this field is ignored.,example=task_graph"`
}

// JSONSchemaExtend marks deprecated TaskGovernance fields as deprecated in
// the generated schema so editors can warn users away from them.
func (TaskGovernance) JSONSchemaExtend(schema *jsonschema.Schema) {
	if schema.Properties == nil {
		return
	}
	for _, name := range []string{
		"retry_budget",
		"graph_timeout_seconds",
		"runtime_budget_seconds",
		"failure_budget",
		"failure_domain",
	} {
		if prop, ok := schema.Properties.Get(name); ok {
			prop.Deprecated = true
		}
	}
}

func (t *TaskGovernance) MaxConcurrentLimit() int {
	if t == nil || t.MaxConcurrent == nil || *t.MaxConcurrent <= 0 {
		return 0
	}
	return *t.MaxConcurrent
}

func (t *TaskGovernance) Timeout() time.Duration {
	if t == nil || t.TimeoutSeconds == nil || *t.TimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(*t.TimeoutSeconds) * time.Second
}

func (t *TaskGovernance) RetryBudgetLimit() int {
	if t == nil || t.RetryBudget == nil || *t.RetryBudget <= 0 {
		return 0
	}
	return *t.RetryBudget
}

func (t *TaskGovernance) GraphTimeout() time.Duration {
	if t == nil || t.GraphTimeoutSeconds == nil || *t.GraphTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(*t.GraphTimeoutSeconds) * time.Second
}

func (t *TaskGovernance) FailFastEnabled() bool {
	if t == nil || t.FailFast == nil {
		return false
	}
	return *t.FailFast
}

func (t *TaskGovernance) RuntimeBudget() time.Duration {
	if t == nil || t.RuntimeBudgetSeconds == nil || *t.RuntimeBudgetSeconds <= 0 {
		return 0
	}
	return time.Duration(*t.RuntimeBudgetSeconds) * time.Second
}

func (t *TaskGovernance) FailureBudgetLimit() int {
	if t == nil || t.FailureBudget == nil || *t.FailureBudget <= 0 {
		return 0
	}
	return *t.FailureBudget
}

func (t *TaskGovernance) FailureDomainName() string {
	if t == nil {
		return ""
	}
	return strings.TrimSpace(t.FailureDomain)
}

type Agent struct {
	ID               string    `json:"id,omitempty"`
	Name             string    `json:"name,omitempty"`
	Description      string    `json:"description,omitempty"`
	Role             string    `json:"role,omitempty" jsonschema:"description=Optional role hint used to specialize this agent,example=orchestrator,example=planner,example=reviewer,example=executor"`
	AdditionalPrompt string    `json:"additional_prompt,omitempty" jsonschema:"description=Additional prompt instructions appended to this agent's system prompt"`
	InitialPrompt    string    `json:"initial_prompt,omitempty" jsonschema:"description=Additional initial instructions injected into this agent's system prompt before it runs"`
	Mode             AgentMode `json:"mode,omitempty" jsonschema:"description=Where this agent can run,enum=primary,enum=subagent,enum=all,default=all"`
	Background       *bool     `json:"background,omitempty" jsonschema:"description=Whether this agent is expected to run as a background-style worker hint"`
	Memory           string    `json:"memory,omitempty" jsonschema:"description=Memory scope hint for this agent,example=inherit,example=isolated,example=ephemeral"`
	Isolation        string    `json:"isolation,omitempty" jsonschema:"description=Isolation hint for this agent,example=workspace,example=session,example=process"`
	OmitContextFiles bool      `json:"omit_context_files,omitempty" jsonschema:"description=Skip project and global context file injection for this agent,default=false"`
	// This is the id of the system prompt used by the agent
	Disabled        bool   `json:"disabled,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"description=Reasoning effort level for this agent,enum=low,enum=medium,enum=high,enum=max"`

	Model SelectedModelType `json:"model" jsonschema:"required,description=The model slot to use for this agent (large, small, or a custom key in models),default=large"`

	// The available tools for the agent
	//  if this is nil, all tools are available
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// this tells us which MCPs are available for this agent
	//  if this is empty all mcps are available
	//  the string array is the list of tools from the AllowedMCP the agent has available
	//  if the string array is nil, all tools from the AllowedMCP are available
	AllowedMCP map[string][]string `json:"allowed_mcp,omitempty"`

	TaskGovernance *TaskGovernance `json:"task_governance,omitempty" jsonschema:"description=Task graph execution policy for this agent"`

	// Overrides the context paths for this agent
	ContextPaths []string `json:"context_paths,omitempty"`

	// New fields for subagent system redesign

	// Spawns lists subagent types this agent can spawn (e.g., ["explore"] or ["*"] for any).
	Spawns []string `json:"spawns,omitempty" jsonschema:"description=Subagent types this agent can spawn,example=explore,example=*"`

	// ModelPriority is an ordered list of model preferences for this agent.
	ModelPriority []string `json:"model_priority,omitempty" jsonschema:"description=Ordered model priority list for this agent,example=claude-opus,example=claude-sonnet"`
	// OutputSchema defines the expected structured output schema for this subagent.
	OutputSchema any `json:"output_schema,omitempty" jsonschema:"description=Expected structured output schema for subagent responses"`
	// MaxTurns limits the number of conversation turns for subagent execution. Zero means no limit.
	MaxTurns int `json:"max_turns,omitempty" jsonschema:"description=Maximum turns for subagent execution (0 = no limit),example=10"`
}

type Tools struct {
	Ls   ToolLs   `json:"ls,omitzero"`
	Grep ToolGrep `json:"grep,omitzero"`
}

type ToolLs struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum depth for directory listings,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of items to return for directory listings,default=1000,example=100"`
}

// Limits returns the user-defined max-depth and max-items, or their defaults.
func (t ToolLs) Limits() (depth, items int) {
	return ptrValOr(t.MaxDepth, 0), ptrValOr(t.MaxItems, 0)
}

type ToolGrep struct {
	Timeout *time.Duration `json:"timeout,omitempty"`
}

// GetTimeout returns the user-defined timeout or the default.
func (t ToolGrep) GetTimeout() time.Duration {
	return ptrValOr(t.Timeout, 5*time.Second)
}

type PluginConfig struct {
	Name      string            `json:"name" jsonschema:"required,description=Unique plugin name,example=morph_compact"`
	Type      string            `json:"type,omitempty" jsonschema:"description=Plugin transport type,enum=command,default=command"`
	Mode      string            `json:"mode,omitempty" jsonschema:"description=Plugin execution mode,enum=transient,persistent,default=transient,description=transient spawns a new process per call; persistent reuses a long-running process over stdio"`
	Command   string            `json:"command" jsonschema:"required,description=Command used to invoke the external plugin,example=node"`
	Args      []string          `json:"args,omitempty" jsonschema:"description=Arguments passed to the plugin command"`
	Env       map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables passed to the plugin command"`
	Hooks     []string          `json:"hooks,omitempty" jsonschema:"description=Enabled plugin hooks,example=[\"chat_messages_transform\",\"session_compacting\"]"`
	TimeoutMs int               `json:"timeout_ms,omitempty" jsonschema:"description=Timeout in milliseconds for each plugin invocation,example=60000"`
	CWD       string            `json:"cwd,omitempty" jsonschema:"description=Working directory for the plugin command"`
}

func (p PluginConfig) Timeout() time.Duration {
	if p.TimeoutMs <= 0 {
		return 60 * time.Second
	}
	return time.Duration(p.TimeoutMs) * time.Millisecond
}

// Config holds the configuration for crush.
type SubagentRuntimeConfig struct {
	StructuredCompletionRequired bool   `json:"structured_completion_required,omitempty" jsonschema:"description=Require built-in subagents to call yield,default=true"`
	MissingFinishPolicy          string `json:"missing_finish_policy,omitempty" jsonschema:"description=Policy when a subagent omits yield,enum=warn,enum=fail,enum=retry_then_warn,enum=retry_then_fail,default=retry_then_warn"`
	DefaultRetryPolicy           string `json:"default_retry_policy,omitempty" jsonschema:"description=Default child retry policy,enum=never,enum=read_only_only,enum=idempotent,enum=isolated,default=read_only_only"`
	MaxConcurrency               int    `json:"max_concurrency,omitempty" jsonschema:"description=Maximum subagent concurrency,default=4"`                    // TODO: wire into task graph semaphore
	AllowRecursiveAgents         bool   `json:"allow_recursive_agents,omitempty" jsonschema:"description=Allow child agents to spawn children,default=false"` // TODO: not yet consumed at runtime; recursive agents blocked at tool_registration.go
	DefaultIsolation             string `json:"default_isolation,omitempty" jsonschema:"description=Default child isolation mode,enum=none,enum=worktree,enum=external_sandbox,enum=managed_sandbox,default=none"`
	SafeSummary                  bool   `json:"safe_summary,omitempty" jsonschema:"description=Prefer structured finish summaries over raw child output,default=true"` // TODO: not yet consumed at runtime; structured finish is already preferred when available
}

func (c *SubagentRuntimeConfig) UnmarshalJSON(data []byte) error {
	type rawSubagentRuntimeConfig struct {
		StructuredCompletionRequired *bool  `json:"structured_completion_required,omitempty"`
		MissingFinishPolicy          string `json:"missing_finish_policy,omitempty"`
		DefaultRetryPolicy           string `json:"default_retry_policy,omitempty"`
		MaxConcurrency               int    `json:"max_concurrency,omitempty"`
		AllowRecursiveAgents         *bool  `json:"allow_recursive_agents,omitempty"`
		DefaultIsolation             string `json:"default_isolation,omitempty"`
		SafeSummary                  *bool  `json:"safe_summary,omitempty"`
	}

	var raw rawSubagentRuntimeConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	*c = SubagentRuntimeConfig{
		StructuredCompletionRequired: true,
		MissingFinishPolicy:          raw.MissingFinishPolicy,
		DefaultRetryPolicy:           raw.DefaultRetryPolicy,
		MaxConcurrency:               raw.MaxConcurrency,
		DefaultIsolation:             raw.DefaultIsolation,
		SafeSummary:                  true,
	}
	if raw.StructuredCompletionRequired != nil {
		c.StructuredCompletionRequired = *raw.StructuredCompletionRequired
	}
	if raw.AllowRecursiveAgents != nil {
		c.AllowRecursiveAgents = *raw.AllowRecursiveAgents
	}
	if raw.SafeSummary != nil {
		c.SafeSummary = *raw.SafeSummary
	}
	return nil
}

type Config struct {
	Schema string `json:"$schema,omitempty"`

	// Model slots available to the application.
	Models map[SelectedModelType]SelectedModel `json:"models,omitempty" jsonschema:"description=Model configurations for different model types,example={\"large\":{\"model\":\"gpt-4o\",\"provider\":\"openai\"}}"`

	// Recently used models stored in the data directory config.
	RecentModels map[SelectedModelType][]SelectedModel `json:"recent_models,omitempty" jsonschema:"-"`

	// The providers that are configured
	Providers *csync.Map[string, ProviderConfig] `json:"providers,omitempty" jsonschema:"description=AI provider configurations"`

	MCP MCPs `json:"mcp,omitempty" jsonschema:"description=Model Context Protocol server configurations"`

	LSP LSPs `json:"lsp,omitempty" jsonschema:"description=Language Server Protocol configurations"`

	Options *Options `json:"options,omitempty" jsonschema:"description=General application options"`

	Permissions *Permissions `json:"permissions,omitempty" jsonschema:"description=Permission settings for tool usage"`

	Tools Tools `json:"tools,omitzero" jsonschema:"description=Tool configurations"`

	Hooks []hooks.HookConfig `json:"hooks,omitempty" jsonschema:"description=Tool execution hooks (PreToolUse)"`

	Plugins []PluginConfig `json:"plugins,omitempty" jsonschema:"description=External command plugins for chat or tool lifecycle customization"`

	Agents    map[string]Agent       `json:"agents,omitempty" jsonschema:"description=Named agent configurations, including built-in overrides and custom subagents"`
	Subagents *SubagentRuntimeConfig `json:"subagents,omitempty" jsonschema:"description=Subagent runtime execution controls"`
}

func (c *Config) EffectiveSubagentRuntime() SubagentRuntimeConfig {
	cfg := SubagentRuntimeConfig{
		StructuredCompletionRequired: true,
		MaxConcurrency:               4,
		AllowRecursiveAgents:         false,
		DefaultIsolation:             "none",
		SafeSummary:                  true,
	}
	if c == nil || c.Subagents == nil {
		return cfg
	}
	if c.Subagents.StructuredCompletionRequired == false {
		cfg.StructuredCompletionRequired = false
	}
	if strings.TrimSpace(c.Subagents.MissingFinishPolicy) != "" {
		cfg.MissingFinishPolicy = strings.TrimSpace(c.Subagents.MissingFinishPolicy)
	}
	if strings.TrimSpace(c.Subagents.DefaultRetryPolicy) != "" {
		cfg.DefaultRetryPolicy = strings.TrimSpace(c.Subagents.DefaultRetryPolicy)
	}
	if c.Subagents.MaxConcurrency > 0 {
		cfg.MaxConcurrency = c.Subagents.MaxConcurrency
	}
	if c.Subagents.AllowRecursiveAgents {
		cfg.AllowRecursiveAgents = true
	}
	if strings.TrimSpace(c.Subagents.DefaultIsolation) != "" {
		cfg.DefaultIsolation = strings.TrimSpace(c.Subagents.DefaultIsolation)
	}
	if c.Subagents.SafeSummary == false {
		cfg.SafeSummary = false
	}
	return cfg
}

func (c *Config) EnabledProviders() []ProviderConfig {
	var enabled []ProviderConfig
	for p := range c.Providers.Seq() {
		if !p.Disable {
			enabled = append(enabled, p)
		}
	}
	return enabled
}

// IsConfigured  return true if at least one provider is configured
func (c *Config) IsConfigured() bool {
	return len(c.EnabledProviders()) > 0
}

func (c *Config) GetModel(provider, model string) *catwalk.Model {
	if providerConfig, ok := c.Providers.Get(provider); ok {
		for _, m := range providerConfig.Models {
			if m.ID == model {
				return &m
			}
		}
	}
	// Fallback: look up model in models.dev data and create a catwalk.Model.
	if devData := GetModelsDevData(); len(devData) > 0 {
		if devModel, found := devData.LookupModel(model); found {
			m := devModel.ToCatwalkModel()
			return &m
		}
	}
	return nil
}

func (c *Config) SelectedModelForType(modelType SelectedModelType) (SelectedModel, bool) {
	if c == nil {
		return SelectedModel{}, false
	}
	if model, ok := c.Models[modelType]; ok {
		return model, true
	}
	switch modelType {
	case SelectedModelTypePlan, SelectedModelTypeReview, SelectedModelTypeDesigner:
		model, ok := c.Models[SelectedModelTypeLarge]
		return model, ok
	case SelectedModelTypeLibrarian, SelectedModelTypeQuickTask:
		model, ok := c.Models[SelectedModelTypeSmall]
		return model, ok
	default:
		return SelectedModel{}, false
	}
}

func (c *Config) GetProviderForModel(modelType SelectedModelType) *ProviderConfig {
	model, ok := c.SelectedModelForType(modelType)
	if !ok {
		return nil
	}
	if providerConfig, ok := c.Providers.Get(model.Provider); ok {
		return &providerConfig
	}
	return nil
}

func (c *Config) GetModelByType(modelType SelectedModelType) *catwalk.Model {
	model, ok := c.SelectedModelForType(modelType)
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

func (c *Config) LargeModel() *catwalk.Model {
	model, ok := c.Models[SelectedModelTypeLarge]
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

func (c *Config) SmallModel() *catwalk.Model {
	model, ok := c.Models[SelectedModelTypeSmall]
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

func (c *Config) BackgroundModel() *catwalk.Model {
	model, ok := c.Models[SelectedModelTypeBackground]
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

func (c *Config) AutoClassifierModel() *catwalk.Model {
	model, ok := c.Models[SelectedModelTypeAutoClassifier]
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

const maxRecentModelsPerType = 5

func allToolNames() []string {
	return []string{
		"agent",
		"bash",
		"job_output",
		"job_wait",
		"job_kill",
		"download",
		"edit",
		"lsp_diagnostics",
		"lsp_references",
		"lsp_declaration",
		"lsp_definition",
		"lsp_implementation",
		"lsp_type_definition",
		"lsp_hover",
		"lsp_document_symbols",
		"lsp_workspace_symbols",
		"lsp_code_action",
		"lsp_rename",
		"lsp_format",
		"lsp_restart",
		"agentic_fetch",
		"glob",
		"grep",
		"read",
		"request_user_input",

		"crush_info",
		"crush_logs",
		"retain",
		"recall",
		"reflect",
		"memory_status",
		"tool_search",
		"todos",
		"send_message",
		"task_stop",
		"yield",
		"subtask_result",
		"write",
	}
}

// IsBuiltinTool returns true if the tool name belongs to a built-in or LSP tool.
func IsBuiltinTool(name string) bool {
	return slices.Contains(allToolNames(), name)
}

func resolveResearchTools(tools []string) []string {
	researchTools := []string{
		"agentic_fetch",
		"bash",
		"glob",
		"grep",
		"lsp_declaration",
		"lsp_definition",
		"lsp_diagnostics",
		"lsp_document_symbols",
		"lsp_hover",
		"lsp_implementation",
		"lsp_references",
		"lsp_type_definition",
		"lsp_workspace_symbols",
		"read",
		"yield",
		"tool_search",
	}
	return filterSlice(tools, researchTools, true)
}

func resolveAllowedTools(allTools []string, disabledTools []string) []string {
	if disabledTools == nil {
		return allTools
	}
	// filter out disabled tools (exclude mode)
	return filterSlice(allTools, disabledTools, false)
}

func resolveReadOnlyTools(tools []string) []string {
	readOnlyTools := []string{"bash", "glob", "grep", "read", "tool_search"}
	// filter to only include tools that are in allowedtools (include mode)
	return filterSlice(tools, readOnlyTools, true)
}

var readOnlyResearchToolNames = []string{
	"bash",
	"glob",
	"grep",
	"lsp_declaration",
	"lsp_definition",
	"lsp_diagnostics",
	"lsp_document_symbols",
	"lsp_hover",
	"lsp_implementation",
	"lsp_references",
	"lsp_type_definition",
	"lsp_workspace_symbols",
	"read",
	"yield",
	"tool_search",
}

func resolvePlanningTools(tools []string) []string {
	return filterSlice(tools, readOnlyResearchToolNames, true)
}

func resolveReviewTools(tools []string) []string {
	return filterSlice(tools, readOnlyResearchToolNames, true)
}

func resolvePrimaryTools(tools []string) []string {
	blockedTools := []string{"todos"}
	return filterSlice(tools, blockedTools, false)
}

func resolveSubAgentTools(tools []string) []string {
	blockedTools := []string{"agent", "request_user_input", "todos"}
	return filterSlice(tools, blockedTools, false)
}

func filterSlice(data []string, mask []string, include bool) []string {
	var filtered []string
	for _, s := range data {
		// if include is true, we include items that ARE in the mask
		// if include is false, we include items that are NOT in the mask
		if include == slices.Contains(mask, s) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func builtinAgents(primaryTools, generalTools, exploreTools, contextPaths []string) map[string]Agent {
	planningTools := resolvePlanningTools(primaryTools)
	researchTools := resolveResearchTools(primaryTools)
	reviewTools := resolveReviewTools(primaryTools)

	return map[string]Agent{
		AgentCoder: {
			ID:           AgentCoder,
			Name:         "Coder",
			Description:  "The primary orchestrator agent for coding tasks.",
			Role:         "orchestrator",
			Memory:       "inherit",
			Isolation:    "workspace",
			Mode:         AgentModePrimary,
			Model:        SelectedModelTypeLarge,
			ContextPaths: contextPaths,
			AllowedTools: primaryTools,
		},
		AgentGeneral: {
			ID:               AgentGeneral,
			Name:             "General",
			Description:      "A subagent that executes independent implementation tasks.",
			Role:             "executor",
			AdditionalPrompt: "Act as the executor: implement the delegated task directly, run the most relevant verification you can, and return a concise execution handoff.",
			Memory:           "inherit",
			Isolation:        "session",
			Mode:             AgentModeSubagent,
			Model:            SelectedModelTypeLarge,
			ContextPaths:     contextPaths,
			AllowedTools:     generalTools,
		},
		AgentPlan: {
			ID:               AgentPlan,
			Name:             "Plan",
			Description:      "A read-only architecture and planning subagent for complex multi-file changes. It produces executable implementation plans with concrete files, sequence, edge cases, and verification steps; use it before substantial implementation, not for tiny edits.",
			Role:             "planner",
			AdditionalPrompt: "Act as a read-only software architect: understand requirements, explore relevant code paths, compare alternatives, and return an executable plan with concrete files, ordered steps, edge cases, and verification. Do not edit files or run builds, tests, package managers, or non-git shell commands.",
			Memory:           "inherit",
			Isolation:        "session",
			Mode:             AgentModeSubagent,
			Model:            SelectedModelTypePlan,
			ContextPaths:     contextPaths,
			AllowedTools:     planningTools,
			AllowedMCP:       map[string][]string{},
			Spawns:           []string{AgentExplore},
		},
		AgentReview: {
			ID:               AgentReview,
			Name:             "Review",
			Description:      "A code-review subagent for final bug, regression, correctness, and security analysis. It runs on a large/review-capable model by default, is read-only, and returns patch-anchored findings for the primary agent to verify.",
			Role:             "reviewer",
			AdditionalPrompt: "Act as a code-review specialist: inspect the current diff and relevant surrounding code, identify only provable bugs the author would want fixed before merge, and return patch-anchored findings with file:line evidence, priority, confidence, and a concise verdict. Do not edit files, approve changes blindly, or run builds, tests, package managers, or non-git shell commands.",
			Memory:           "inherit",
			Isolation:        "session",
			Mode:             AgentModeSubagent,
			Model:            SelectedModelTypeReview,
			ContextPaths:     contextPaths,
			AllowedTools:     reviewTools,
			AllowedMCP:       map[string][]string{},
			Spawns:           []string{AgentExplore},
		},
		AgentDesigner: {
			ID:               AgentDesigner,
			Name:             "Designer",
			Description:      "A UI/UX implementation and review subagent for visual refinement, accessibility, interaction states, and frontend polish. It may edit files and run targeted verification when delegated UI work requires it.",
			Role:             "designer",
			AdditionalPrompt: "Act as a UI/UX specialist: reuse existing components and tokens, implement explicit loading/empty/error/disabled/focus states, check accessibility and responsive behavior, avoid generic AI-looking design patterns, and keep changes minimal and consistent with the codebase.",
			Memory:           "inherit",
			Isolation:        "session",
			Mode:             AgentModeSubagent,
			Model:            SelectedModelTypeDesigner,
			ContextPaths:     contextPaths,
			AllowedTools:     generalTools,
		},
		AgentLibrarian: {
			ID:               AgentLibrarian,
			Name:             "Librarian",
			Description:      "A source-verified library/API research subagent. It answers dependency, framework, and external API questions from local source, official documentation, and exact version evidence rather than model memory.",
			Role:             "researcher",
			AdditionalPrompt: "Act as a source-verified librarian: answer dependency, framework, and external API questions by reading local dependencies first, then official documentation or source when needed. Every claim should cite source paths, versions, or URLs. Do not edit project files or rely on training-data memory for API details.",
			Memory:           "inherit",
			Isolation:        "session",
			Mode:             AgentModeSubagent,
			Model:            SelectedModelTypeLibrarian,
			ContextPaths:     contextPaths,
			AllowedTools:     researchTools,
			AllowedMCP:       map[string][]string{},
		},
		AgentQuickTask: {
			ID:               AgentQuickTask,
			Name:             "Quick Task",
			Description:      "A low-cost worker for strictly mechanical, well-scoped updates or data collection. Use it only when the task is small, unambiguous, and does not require deep reasoning or review judgment.",
			Role:             "executor",
			AdditionalPrompt: "Act as a fast mechanical worker: complete only the assigned small, unambiguous task; avoid broad redesign, deep review, or exploratory scope creep; run only narrow verification when needed; return a minimal handoff.",
			Memory:           "inherit",
			Isolation:        "session",
			Mode:             AgentModeSubagent,
			Model:            SelectedModelTypeQuickTask,
			ContextPaths:     contextPaths,
			AllowedTools:     generalTools,
		},
		AgentExplore: {
			ID:               AgentExplore,
			Name:             "Explore",
			Description:      "A read-only research subagent for broad codebase exploration, evidence gathering, pattern hunting, and local git inspection. It reports facts and references for the primary agent to analyze; use the primary agent for final code review and general for implementation, reproduction, build, test, lint, package-manager, or non-git shell commands.",
			Role:             "researcher",
			AdditionalPrompt: "Act as a read-only researcher: gather source-backed context, map relevant files/symbols/history, and return concise evidence with file:line references. Do not provide final code-review approval, make correctness judgments beyond clearly supported facts, edit files, or attempt build, test, lint, package-manager, reproduction, or non-git shell commands.",
			Memory:           "inherit",
			Isolation:        "session",
			Mode:             AgentModeSubagent,
			// Explore is read-only and used for parallel context gathering.
			// Default to the small/fast model so it is cheaper and quicker
			// than the general subagent. Keep its responsibility narrow:
			// gather evidence for the primary model rather than making final
			// review or implementation decisions. Users can override this in
			// crush.json if they want a larger model.
			Model:        SelectedModelTypeSmall,
			ContextPaths: contextPaths,
			AllowedTools: exploreTools,
			// NO MCPs or LSPs by default
			AllowedMCP: map[string][]string{},
		},
	}
}

func mergeAgentConfig(base, override Agent) Agent {
	merged := base

	if override.Name != "" {
		merged.Name = override.Name
	}
	if override.Description != "" {
		merged.Description = override.Description
	}
	if override.Role != "" {
		merged.Role = override.Role
	}
	if override.AdditionalPrompt != "" {
		merged.AdditionalPrompt = override.AdditionalPrompt
	}
	if override.InitialPrompt != "" {
		merged.InitialPrompt = override.InitialPrompt
	}
	if override.Mode != "" {
		merged.Mode = override.Mode
	}
	if override.Background != nil {
		merged.Background = override.Background
	}
	if override.Memory != "" {
		merged.Memory = override.Memory
	}
	if override.Isolation != "" {
		merged.Isolation = override.Isolation
	}
	if override.OmitContextFiles {
		merged.OmitContextFiles = true
	}
	if override.Disabled {
		merged.Disabled = true
	}
	if override.Model != "" {
		merged.Model = override.Model
	}
	if override.AllowedTools != nil {
		merged.AllowedTools = override.AllowedTools
	}
	if override.AllowedMCP != nil {
		merged.AllowedMCP = override.AllowedMCP
	}
	if override.TaskGovernance != nil {
		merged.TaskGovernance = override.TaskGovernance
	}
	if override.ContextPaths != nil {
		merged.ContextPaths = override.ContextPaths
	}
	if override.Spawns != nil {
		merged.Spawns = override.Spawns
	}

	return merged
}

func agentConfigsEqual(a, b Agent) bool {
	return a.ID == b.ID &&
		a.Name == b.Name &&
		a.Description == b.Description &&
		a.Role == b.Role &&
		a.AdditionalPrompt == b.AdditionalPrompt &&
		a.InitialPrompt == b.InitialPrompt &&
		a.Mode == b.Mode &&
		ptrValOr(a.Background, false) == ptrValOr(b.Background, false) &&
		a.Memory == b.Memory &&
		a.Isolation == b.Isolation &&
		a.OmitContextFiles == b.OmitContextFiles &&
		a.Disabled == b.Disabled &&
		a.Model == b.Model &&
		slices.Equal(a.AllowedTools, b.AllowedTools) &&
		taskGovernanceEqual(a.TaskGovernance, b.TaskGovernance) &&
		slices.Equal(a.ContextPaths, b.ContextPaths) &&
		maps.EqualFunc(a.AllowedMCP, b.AllowedMCP, slices.Equal) &&
		slices.Equal(a.Spawns, b.Spawns)
}

func taskGovernanceEqual(a, b *TaskGovernance) bool {
	return effectiveTaskGovernanceInt(a, func(t *TaskGovernance) *int { return t.MaxConcurrent }) == effectiveTaskGovernanceInt(b, func(t *TaskGovernance) *int { return t.MaxConcurrent }) &&
		effectiveTaskGovernanceInt(a, func(t *TaskGovernance) *int { return t.TimeoutSeconds }) == effectiveTaskGovernanceInt(b, func(t *TaskGovernance) *int { return t.TimeoutSeconds }) &&
		effectiveTaskGovernanceInt(a, func(t *TaskGovernance) *int { return t.RetryBudget }) == effectiveTaskGovernanceInt(b, func(t *TaskGovernance) *int { return t.RetryBudget }) &&
		effectiveTaskGovernanceInt(a, func(t *TaskGovernance) *int { return t.GraphTimeoutSeconds }) == effectiveTaskGovernanceInt(b, func(t *TaskGovernance) *int { return t.GraphTimeoutSeconds }) &&
		effectiveTaskGovernanceBool(a, func(t *TaskGovernance) *bool { return t.FailFast }) == effectiveTaskGovernanceBool(b, func(t *TaskGovernance) *bool { return t.FailFast }) &&
		effectiveTaskGovernanceInt(a, func(t *TaskGovernance) *int { return t.RuntimeBudgetSeconds }) == effectiveTaskGovernanceInt(b, func(t *TaskGovernance) *int { return t.RuntimeBudgetSeconds }) &&
		effectiveTaskGovernanceInt(a, func(t *TaskGovernance) *int { return t.FailureBudget }) == effectiveTaskGovernanceInt(b, func(t *TaskGovernance) *int { return t.FailureBudget }) &&
		a.FailureDomainName() == b.FailureDomainName()
}

func effectiveTaskGovernanceInt(t *TaskGovernance, field func(*TaskGovernance) *int) int {
	if t == nil {
		return 0
	}
	value := field(t)
	if value == nil || *value <= 0 {
		return 0
	}
	return *value
}

func effectiveTaskGovernanceBool(t *TaskGovernance, field func(*TaskGovernance) *bool) bool {
	if t == nil {
		return false
	}
	return ptrValOr(field(t), false)
}

func defaultAllowedToolsForAgent(agent Agent, primaryTools, generalTools []string) []string {
	switch NormalizeAgentMode(agent.Mode) {
	case AgentModeSubagent:
		return generalTools
	case AgentModePrimary, AgentModeAll:
		return primaryTools
	default:
		return primaryTools
	}
}

func normalizeConfiguredAgent(key string, agent Agent) (string, Agent, bool) {
	agentID := strings.TrimSpace(agent.ID)
	if agentID == "" {
		agentID = strings.TrimSpace(key)
	}
	if agentID == "" {
		return "", Agent{}, false
	}

	agentID = CanonicalSubagentID(agentID)
	agent.ID = agentID

	if agent.Name == "" {
		runes := []rune(agentID)
		runes[0] = unicode.ToUpper(runes[0])
		agent.Name = string(runes)
	}

	return agentID, agent, true
}

func (c *Config) SetupAgents() {
	allowedTools := resolveAllowedTools(allToolNames(), c.Options.DisabledTools)
	primaryTools := resolvePrimaryTools(allowedTools)
	generalTools := resolveSubAgentTools(primaryTools)
	exploreTools := resolveReadOnlyTools(allowedTools)

	agents := builtinAgents(primaryTools, generalTools, exploreTools, c.Options.ContextPaths)
	for key, configured := range c.Agents {
		agentID, normalized, ok := normalizeConfiguredAgent(key, configured)
		if !ok {
			continue
		}
		if builtin, exists := agents[agentID]; exists {
			if agentConfigsEqual(normalized, builtin) {
				continue
			}
			agents[agentID] = mergeAgentConfig(builtin, normalized)
			continue
		}
		// Only assign defaults for new (non-builtin) agents.
		if normalized.Model == "" {
			normalized.Model = SelectedModelTypeLarge
		}
		if normalized.AllowedTools == nil {
			normalized.AllowedTools = defaultAllowedToolsForAgent(normalized, primaryTools, generalTools)
		}
		if normalized.ContextPaths == nil {
			normalized.ContextPaths = c.Options.ContextPaths
		}
		agents[agentID] = normalized
	}

	c.Agents = agents
}

func (c *ProviderConfig) TestConnection(resolver VariableResolver) error {
	var (
		providerID = catwalk.InferenceProvider(c.ID)
		testURL    = ""
		headers    = make(map[string]string)
		apiKey, _  = resolver.ResolveValue(c.APIKey)
	)

	switch providerID {
	case catwalk.InferenceProviderMiniMax, catwalk.InferenceProviderMiniMaxChina:
		// NOTE: MiniMax has no good endpoint we can use to validate the API key.
		// Let's at least check the pattern.
		if !strings.HasPrefix(apiKey, "sk-") {
			return fmt.Errorf("invalid API key format for provider %s", c.ID)
		}
		return nil
	}

	switch c.Type {
	case catwalk.TypeOpenAI, catwalk.TypeOpenAICompat, catwalk.TypeOpenRouter:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://api.openai.com/v1")

		switch providerID {
		case catwalk.InferenceProviderOpenRouter:
			testURL = baseURL + "/credits"
		default:
			testURL = baseURL + "/models"
		}

		headers["Authorization"] = "Bearer " + apiKey
	case catwalk.TypeAnthropic:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://api.anthropic.com/v1")

		switch providerID {
		case catwalk.InferenceKimiCoding:
			testURL = baseURL + "/v1/models"
		default:
			testURL = baseURL + "/models"
		}

		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
	case catwalk.TypeGoogle:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://generativelanguage.googleapis.com")
		testURL = baseURL + "/v1beta/models?key=" + url.QueryEscape(apiKey)
	case catwalk.TypeBedrock:
		// NOTE: Bedrock has a `/foundation-models` endpoint that we could in
		// theory use, but apparently the authorization is region-specific,
		// so it's not so trivial.
		if strings.HasPrefix(apiKey, "ABSK") { // Bedrock API keys
			return nil
		}
		return errors.New("not a valid bedrock api key")
	case catwalk.TypeVercel:
		// NOTE: Vercel does not validate API keys on the `/models` endpoint.
		if strings.HasPrefix(apiKey, "vck_") { // Vercel API keys
			return nil
		}
		return errors.New("not a valid vercel api key")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &http.Client{}
	req, err := http.NewRequestWithContext(ctx, "GET", testURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for provider %s: %w", c.ID, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range c.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to create request for provider %s: %w", c.ID, err)
	}
	defer resp.Body.Close()

	switch providerID {
	case catwalk.InferenceProviderZAI:
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	default:
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	}
	return nil
}

func resolveEnvs(envs map[string]string) []string {
	resolver := NewShellVariableResolver(env.New())
	for e, v := range envs {
		var err error
		envs[e], err = resolver.ResolveValue(v)
		if err != nil {
			slog.Error("Error resolving environment variable", "error", err, "variable", e, "value", v)
			continue
		}
	}

	res := make([]string, 0, len(envs))
	for k, v := range envs {
		res = append(res, fmt.Sprintf("%s=%s", k, v))
	}
	return res
}

func ptrValOr[T any](t *T, el T) T {
	if t == nil {
		return el
	}
	return *t
}
