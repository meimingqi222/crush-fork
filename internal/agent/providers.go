package agent

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/httpext"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/oauth/copilot"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	openaisdk "github.com/charmbracelet/openai-go/option"
	"github.com/qjebbs/go-jsons"
)

var anthropicEnvMu sync.Mutex

func getProviderOptions(model Model, providerCfg config.ProviderConfig, agentCfg ...config.Agent) fantasy.ProviderOptions {
	options := fantasy.ProviderOptions{}

	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catwalkOpts := []byte("{}")

	if model.CatwalkCfg.Options.ProviderOptions != nil {
		data, err := json.Marshal(model.CatwalkCfg.Options.ProviderOptions)
		if err == nil {
			catwalkOpts = data
		}
	}

	if providerCfg.ProviderOptions != nil {
		data, err := json.Marshal(providerCfg.ProviderOptions)
		if err == nil {
			providerCfgOpts = data
		}
	}

	if model.ModelCfg.ProviderOptions != nil {
		data, err := json.Marshal(model.ModelCfg.ProviderOptions)
		if err == nil {
			cfgOpts = data
		}
	}

	readers := []io.Reader{
		bytes.NewReader(catwalkOpts),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	}

	got, err := jsons.Merge(readers)
	if err != nil {
		slog.Error("Could not merge call config", "err", err)
		return options
	}

	mergedOptions := make(map[string]any)

	err = json.Unmarshal([]byte(got), &mergedOptions)
	if err != nil {
		slog.Error("Could not create config for call", "err", err)
		return options
	}

	providerType := providerCfg.Type
	if providerType == "hyper" {
		if strings.Contains(model.CatwalkCfg.ID, "claude") {
			providerType = anthropic.Name
		} else if strings.Contains(model.CatwalkCfg.ID, "gpt") {
			providerType = openai.Name
		} else if strings.Contains(model.CatwalkCfg.ID, "gemini") {
			providerType = google.Name
		} else {
			providerType = openaicompat.Name
		}
	}

	// Reasoning effort: use agent config if set, then user selection,
	// then fall back to model's default.
	reasoningEffort := ""
	for _, a := range agentCfg {
		if strings.TrimSpace(a.ReasoningEffort) != "" {
			reasoningEffort = a.ReasoningEffort
			break
		}
	}
	if reasoningEffort == "" {
		reasoningEffort = model.ModelCfg.ReasoningEffort
	}
	if reasoningEffort == "" {
		reasoningEffort = model.CatwalkCfg.DefaultReasoningEffort
	}
	shouldSetEffort := reasoningEffort != "" && model.CatwalkCfg.CanReason &&
		(len(model.CatwalkCfg.ReasoningLevels) == 0 || slices.Contains(model.CatwalkCfg.ReasoningLevels, reasoningEffort))

	switch providerType {
	case openai.Name, azure.Name:
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			// Explicitly disabled: clear any reasoning params from provider config too.
			delete(mergedOptions, "reasoning_effort")
		} else {
			_, hasReasoningEffort := mergedOptions["reasoning_effort"]
			if !hasReasoningEffort && model.CatwalkCfg.CanReason {
				if shouldSetEffort {
					mergedOptions["reasoning_effort"] = reasoningEffort
				} else {
					defaultEffort := "high"
					if len(model.CatwalkCfg.ReasoningLevels) == 0 || slices.Contains(model.CatwalkCfg.ReasoningLevels, defaultEffort) {
						mergedOptions["reasoning_effort"] = defaultEffort
					}
				}
			}
		}
		useResponsesAPI := openai.ShouldUseResponsesAPI(
			model.CatwalkCfg.ID,
			providerCfg.ModelUseResponsesAPI(model.CatwalkCfg.ID),
		)
		if useResponsesAPI {
			if thinkingDisabled {
				// Clear Responses API reasoning params from provider config.
				delete(mergedOptions, "reasoning_summary")
				delete(mergedOptions, "include")
			} else if openai.IsResponsesReasoningModel(model.CatwalkCfg.ID) || model.CatwalkCfg.CanReason {
				_, hasSummary := mergedOptions["reasoning_summary"]
				if !hasSummary {
					mergedOptions["reasoning_summary"] = "auto"
				}
				_, hasInclude := mergedOptions["include"]
				if !hasInclude {
					mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
				}
			}
			parsed, err := openai.ParseResponsesOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			}
		} else {
			parsed, err := openai.ParseOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			}
		}
	case anthropic.Name, bedrock.Name:
		// Map reasoning effort to Anthropic parameters.
		//
		// Claude 4.6+ (claude-sonnet-4.6, claude-opus-4.6, claude-opus-4-7, etc.)
		// supports the "effort" parameter which enables adaptive thinking. The
		// fantasy SDK converts effort → thinking: {type: "adaptive"} automatically.
		//
		// Older Claude models use the legacy thinking: {type: "enabled", budget_tokens}.
		//
		// Default behavior: if the model supports reasoning (CanReason), enable thinking
		// by default. Users can override via Think=false to disable.
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			// Explicitly disabled: clear any thinking params from provider config too.
			delete(mergedOptions, "effort")
			delete(mergedOptions, "thinking")
		} else {
			_, hasEffort := mergedOptions["effort"]
			_, hasThinking := mergedOptions["thinking"]
			if !hasEffort && !hasThinking && model.CatwalkCfg.CanReason {
				isClaude46 := requiresAdaptiveThinking(model.CatwalkCfg.ID)
				switch {
				case shouldSetEffort:
					if isClaude46 {
						// Claude 4.6+: use effort parameter (adaptive thinking)
						mergedOptions["effort"] = reasoningEffort
					} else {
						// Older Claude: use budget_tokens
						budgetTokens := effortToBudgetTokens(reasoningEffort)
						mergedOptions["thinking"] = map[string]any{
							"type":          "enabled",
							"budget_tokens": budgetTokens,
						}
					}
				default:
					// Default: model supports reasoning, enable thinking with high effort.
					defaultEffort := "high"
					if isClaude46 {
						if len(model.CatwalkCfg.ReasoningLevels) == 0 || slices.Contains(model.CatwalkCfg.ReasoningLevels, defaultEffort) {
							mergedOptions["effort"] = defaultEffort
						}
					} else {
						mergedOptions["thinking"] = map[string]any{
							"type":          "enabled",
							"budget_tokens": effortToBudgetTokens(defaultEffort),
						}
					}
				}
			}
		}
		parsed, err := anthropic.ParseOptions(mergedOptions)
		if err == nil {
			options[anthropic.Name] = parsed
		}

	case openrouter.Name:
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			delete(mergedOptions, "reasoning")
		} else {
			_, hasReasoning := mergedOptions["reasoning"]
			if !hasReasoning && model.CatwalkCfg.CanReason {
				if shouldSetEffort {
					mergedOptions["reasoning"] = map[string]any{
						"enabled": true,
						"effort":  reasoningEffort,
					}
				} else {
					defaultEffort := "high"
					if len(model.CatwalkCfg.ReasoningLevels) == 0 || slices.Contains(model.CatwalkCfg.ReasoningLevels, defaultEffort) {
						mergedOptions["reasoning"] = map[string]any{
							"enabled": true,
							"effort":  defaultEffort,
						}
					}
				}
			}
		}
		parsed, err := openrouter.ParseOptions(mergedOptions)
		if err == nil {
			options[openrouter.Name] = parsed
		}
	case vercel.Name:
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			delete(mergedOptions, "reasoning")
		} else {
			_, hasReasoning := mergedOptions["reasoning"]
			if !hasReasoning && model.CatwalkCfg.CanReason {
				if shouldSetEffort {
					mergedOptions["reasoning"] = map[string]any{
						"enabled": true,
						"effort":  reasoningEffort,
					}
				} else {
					defaultEffort := "high"
					if len(model.CatwalkCfg.ReasoningLevels) == 0 || slices.Contains(model.CatwalkCfg.ReasoningLevels, defaultEffort) {
						mergedOptions["reasoning"] = map[string]any{
							"enabled": true,
							"effort":  defaultEffort,
						}
					}
				}
			}
		}
		parsed, err := vercel.ParseOptions(mergedOptions)
		if err == nil {
			options[vercel.Name] = parsed
		}
	case google.Name:
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			delete(mergedOptions, "thinking_config")
		} else {
			_, hasThinkingConfig := mergedOptions["thinking_config"]
			if !hasThinkingConfig && model.CatwalkCfg.CanReason {
				if shouldSetEffort {
					mergedOptions["thinking_config"] = map[string]any{
						"thinking_level":   reasoningEffort,
						"include_thoughts": true,
					}
				} else {
					defaultLevel := "high"
					if len(model.CatwalkCfg.ReasoningLevels) == 0 || slices.Contains(model.CatwalkCfg.ReasoningLevels, defaultLevel) {
						mergedOptions["thinking_config"] = map[string]any{
							"thinking_level":   defaultLevel,
							"include_thoughts": true,
						}
					}
				}
			}
		}
		parsed, err := google.ParseOptions(mergedOptions)
		if err == nil {
			options[google.Name] = parsed
		}
	case openaicompat.Name, hyper.Name:
		extraBody := make(map[string]any)

		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			delete(mergedOptions, "reasoning_effort")
			switch providerCfg.ID {
			case string(catwalk.InferenceProviderIoNet):
				extraBody["reasoning"] = map[string]string{"effort": "none"}
			case hyper.Name:
				extraBody["thinking"] = false
			case string(catwalk.InferenceProviderZAI), string(catwalk.InferenceProviderDeepSeek):
				extraBody["thinking"] = map[string]any{
					"type": "disabled",
				}
			}
		} else {
			_, hasReasoningEffort := mergedOptions["reasoning_effort"]
			if !hasReasoningEffort && model.CatwalkCfg.CanReason {
				if shouldSetEffort {
					switch providerCfg.ID {
					case string(catwalk.InferenceProviderIoNet):
						extraBody["reasoning"] = map[string]string{"effort": reasoningEffort}
					default:
						mergedOptions["reasoning_effort"] = reasoningEffort
					}
				} else {
					defaultEffort := "high"
					if len(model.CatwalkCfg.ReasoningLevels) == 0 || slices.Contains(model.CatwalkCfg.ReasoningLevels, defaultEffort) {
						switch providerCfg.ID {
						case string(catwalk.InferenceProviderIoNet):
							extraBody["reasoning"] = map[string]string{"effort": defaultEffort}
						default:
							mergedOptions["reasoning_effort"] = defaultEffort
						}
					}
				}
			}

			thinkEnabled := model.ModelCfg.Think != nil && *model.ModelCfg.Think

			// "reasoning effort" is a standard OpenAI field, but "thinking" is not.
			// Setting it in the right way for each provider.
			// TODO: Abstract this in Fantasy somehow?
			// TODO: Allow custom providers to specify how to set this?
			switch providerCfg.ID {
			case hyper.Name:
				extraBody["thinking"] = thinkEnabled
			case string(catwalk.InferenceProviderIoNet):
				if _, ok := extraBody["reasoning"]; !ok && model.CatwalkCfg.CanReason {
					if thinkEnabled {
						extraBody["reasoning"] = map[string]string{"effort": "medium"}
					} else {
						extraBody["reasoning"] = map[string]string{"effort": "none"}
					}
				}
			case string(catwalk.InferenceProviderZAI), string(catwalk.InferenceProviderDeepSeek):
				if thinkEnabled {
					extraBody["thinking"] = map[string]any{
						"type": "enabled",
					}
				} else {
					extraBody["thinking"] = map[string]any{
						"type": "disabled",
					}
				}
			}
		}
		parsed, err := openaicompat.ParseOptions(mergedOptions)
		if err == nil {
			if len(extraBody) > 0 {
				parsed.ExtraBody = extraBody
			}
			options[openaicompat.Name] = parsed
		}
	}

	return options
}

func effortToBudgetTokens(effort string) int {
	// Budget tokens chosen to produce the correct reasoning_effort when translated by Copilot API
	budgetMap := map[string]int{
		"low":    2048,  // Will map to "low" in Copilot (1024 <= budget < 8192)
		"medium": 12288, // Will map to "medium" in Copilot (8192 <= budget < 24576)
		"high":   28672, // Will map to "high" in Copilot (24576 <= budget < 32768)
		"max":    49152, // Will map to "xhigh" in Copilot (>= 32768)
	}

	budget, ok := budgetMap[effort]
	if !ok {
		budget = 12288 // default to medium
	}

	return budget
}

func requiresAdaptiveThinking(modelID string) bool {
	id := strings.ToLower(modelID)

	// For provider-prefixed model IDs (e.g., "anthropic/claude-sonnet-4.6"),
	// extract the base model ID by finding the last occurrence of "claude-"
	baseID := id
	if idx := strings.LastIndex(id, "claude-"); idx != -1 && idx > 0 {
		baseID = id[idx:]
	}

	// Match patterns like claude-{variant}-4.N or claude-{variant}-4-N where N >= 6
	for _, variant := range []string{"sonnet", "opus", "haiku"} {
		prefix := "claude-" + variant + "-4"
		// Check for prefix match (e.g., claude-sonnet-4.6)
		if strings.HasPrefix(baseID, prefix+".") {
			minor := baseID[len(prefix)+1:]
			if n, err := parseLeadingInt(minor); err == nil && n >= 6 {
				return true
			}
		}
		// Check for prefix match with dash (e.g., claude-sonnet-4-6)
		if strings.HasPrefix(baseID, prefix+"-") {
			minor := baseID[len(prefix)+1:]
			if n, err := parseLeadingInt(minor); err == nil && n >= 6 {
				return true
			}
		}
	}
	return false
}

func parseLeadingInt(s string) (int, error) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("no digits")
	}
	n := 0
	for i := 0; i < end; i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

func mergeCallOptions(model Model, cfg config.ProviderConfig) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64) {
	modelOptions := getProviderOptions(model, cfg)
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatwalkCfg.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatwalkCfg.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatwalkCfg.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatwalkCfg.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty
}

func effectiveMaxOutputTokens(model Model) (int64, bool) {
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens == 0 {
		return maxTokens, false
	}
	if model.CatwalkCfg.DefaultMaxTokens > 0 && model.ModelCfg.MaxTokens > model.CatwalkCfg.DefaultMaxTokens*2 {
		return model.CatwalkCfg.DefaultMaxTokens, true
	}
	return model.ModelCfg.MaxTokens, false
}

func (c *coordinator) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID string, useCopilotClient, isSubAgent bool) (fantasy.Provider, error) {
	var opts []anthropic.Option

	anthropicEnvMu.Lock()
	defer anthropicEnvMu.Unlock()
	oldKey, hasOldKey := os.LookupEnv("ANTHROPIC_API_KEY")

	switch {
	case strings.HasPrefix(apiKey, "Bearer "):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = apiKey
	case providerID == string(catwalk.InferenceProviderMiniMax) || providerID == string(catwalk.InferenceProviderMiniMaxChina):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = "Bearer " + apiKey
	case apiKey != "":
		// X-Api-Key header
		opts = append(opts, anthropic.WithAPIKey(apiKey))
	}

	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}

	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	if useCopilotClient {
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug)
	} else if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	// Always wrap so that requests without an explicit `thinking` field have
	// `thinking:{type:"disabled"}` injected. This is a no-op for upstream
	// Anthropic (where omitting thinking already means disabled) but is
	// required for Anthropic-compatible proxies (DeepSeek's /anthropic
	// endpoint, etc.) whose default is ON. It also makes the in-flight
	// "retry without thinking" path actually disable thinking on the wire
	// when the SDK has nilled the typed Thinking option.
	wrapped := httpext.WrapAnthropicDisableThinkingHTTPClient(httpClient)
	opts = append(opts, anthropic.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(wrapped)))

	provider, err := anthropic.New(opts...)
	if hasOldKey {
		os.Setenv("ANTHROPIC_API_KEY", oldKey)
	} else {
		os.Unsetenv("ANTHROPIC_API_KEY")
	}
	return provider, err
}

func (c *coordinator) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string, modelID string, useResponsesAPI, copilotService, useCopilotClient, isSubAgent, responsesWebSocket bool) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	if useResponsesAPI {
		opts = append(opts, openai.WithForceResponsesModel(modelID))
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	if useCopilotClient {
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug)
	} else if copilotService {
		// Use billing client for Copilot service.
		httpClient = copilot.NewBillingClient(copilotService, c.cfg.Config().Options.Debug)
	} else if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, openai.WithHTTPClient(wrapOpenAIStreamingHTTPClient(httpClient, responsesWebSocket)))

	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func wrapOpenAIStreamingHTTPClient(httpClient *http.Client, responsesWebSocket bool) *http.Client {
	if responsesWebSocket {
		httpClient = httpext.WrapOpenAIResponsesWebSocketHTTPClient(httpClient)
	}
	return httpext.WrapActivityTrackingHTTPClient(httpClient)
}

func (c *coordinator) buildOpenrouterProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, openrouter.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (c *coordinator) buildVercelProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, vercel.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

func (c *coordinator) buildOpenaiCompatProvider(
	baseURL, apiKey string,
	headers map[string]string,
	extraBody map[string]any,
	providerID string,
	modelID string,
	useResponsesAPI bool,
	useCopilotClient bool,
	isSubAgent bool,
	copilotService bool,
	responsesWebSocket bool,
) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	if providerID == string(catwalk.InferenceProviderCopilot) || useCopilotClient {
		// Copilot client already applies reasoning field normalization internally.
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug)
	} else if copilotService {
		// Use billing client for Copilot-compatible providers, wrapped with
		// reasoning field normalization.
		billingClient := copilot.NewBillingClient(copilotService, c.cfg.Config().Options.Debug)
		httpClient = &http.Client{
			Transport: copilot.NewReasoningNormalizingTransport(billingClient.Transport),
		}
	} else {
		// For all other openai-compat providers, apply reasoning field
		// normalization so that models returning "reasoning" or
		// "reasoning_text" are transparently mapped to "reasoning_content".
		var inner http.RoundTripper
		if c.cfg.Config().Options.Debug {
			inner = log.NewHTTPClient().Transport
		}
		httpClient = &http.Client{
			Transport: copilot.NewReasoningNormalizingTransport(inner),
		}
	}
	if providerID == string(catwalk.InferenceProviderCopilot) || useResponsesAPI {
		opts = append(opts, openaicompat.WithUseResponsesAPI())
	}
	if useResponsesAPI {
		opts = append(opts, openaicompat.WithForceResponsesModel(modelID))
	}
	opts = append(opts, openaicompat.WithHTTPClient(wrapOpenAIStreamingHTTPClient(httpClient, responsesWebSocket)))

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

func (c *coordinator) buildAzureProvider(baseURL, apiKey string, headers map[string]string, modelID string, useResponsesAPI bool, options map[string]string) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	if useResponsesAPI {
		opts = append(opts, azure.WithForceResponsesModel(modelID))
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, azure.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if options == nil {
		options = make(map[string]string)
	}
	if apiVersion, ok := options["apiVersion"]; ok {
		opts = append(opts, azure.WithAPIVersion(apiVersion))
	}
	if len(headers) > 0 {
		opts = append(opts, azure.WithHeaders(headers))
	}

	return azure.New(opts...)
}

func (c *coordinator) buildBedrockProvider(apiKey string, headers map[string]string) (fantasy.Provider, error) {
	var opts []bedrock.Option
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, bedrock.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}
	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
	case os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
	default:
		// Skip, let the SDK do authentication.
	}
	return bedrock.New(opts...)
}

func (c *coordinator) buildGoogleProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, google.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (c *coordinator) buildGoogleVertexProvider(headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, google.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}

	project := options["project"]
	location := options["location"]

	opts = append(opts, google.WithVertex(project, location))

	return google.New(opts...)
}

func (c *coordinator) buildHyperProvider(baseURL, apiKey string) (fantasy.Provider, error) {
	opts := []hyper.Option{
		hyper.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, hyper.WithBaseURL(baseURL))
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, hyper.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	return hyper.New(opts...)
}

func isAnthropicThinking(model catwalk.Model) bool {
	// When model.CanReason is true, thinking is enabled by default unless the
	// user explicitly disables it (Think=false). Callers that need to respect the
	// explicit-disable case must also check thinkingDisabled separately.
	if model.CanReason {
		return true
	}

	opts, err := anthropic.ParseOptions(model.Options.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

func (c *coordinator) buildProvider(providerCfg config.ProviderConfig, model catwalk.Model, isSubAgent bool, thinkingDisabled bool) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if providerCfg.Type == anthropic.Name && isAnthropicThinking(model) && !thinkingDisabled {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	apiKey, err := c.cfg.Resolve(providerCfg.APIKey)
	if err != nil {
		slog.Warn("Failed to resolve API key template", "provider", providerCfg.ID, "error", err)
	}
	apiKey = config.DecryptAPIKeyIfNeeded(apiKey)
	baseURL, err := c.cfg.Resolve(providerCfg.BaseURL)
	if err != nil {
		slog.Warn("Failed to resolve Base URL template", "provider", providerCfg.ID, "error", err)
	}

	useResponsesAPI := providerCfg.ModelUseResponsesAPI(model.ID)

	switch providerCfg.Type {
	case openai.Name:
		return c.buildOpenaiProvider(baseURL, apiKey, headers, model.ID, useResponsesAPI, providerCfg.CopilotService, providerCfg.UseCopilotClient, isSubAgent, providerCfg.ResponsesWebSocket)
	case anthropic.Name:
		return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, providerCfg.UseCopilotClient, isSubAgent)
	case openrouter.Name:
		return c.buildOpenrouterProvider(baseURL, apiKey, headers)
	case vercel.Name:
		return c.buildVercelProvider(baseURL, apiKey, headers)
	case azure.Name:
		return c.buildAzureProvider(baseURL, apiKey, headers, model.ID, useResponsesAPI, providerCfg.ExtraParams)
	case bedrock.Name:
		return c.buildBedrockProvider(apiKey, headers)
	case google.Name:
		return c.buildGoogleProvider(baseURL, apiKey, headers)
	case "google-vertex":
		return c.buildGoogleVertexProvider(headers, providerCfg.ExtraParams)
	case openaicompat.Name:
		if providerCfg.ID == string(catwalk.InferenceProviderZAI) {
			if providerCfg.ExtraBody == nil {
				providerCfg.ExtraBody = map[string]any{}
			}
			providerCfg.ExtraBody["tool_stream"] = true
		}
		return c.buildOpenaiCompatProvider(
			baseURL,
			apiKey,
			headers,
			providerCfg.ExtraBody,
			providerCfg.ID,
			model.ID,
			useResponsesAPI,
			providerCfg.UseCopilotClient,
			isSubAgent,
			providerCfg.CopilotService,
			providerCfg.ResponsesWebSocket,
		)
	case hyper.Name:
		return c.buildHyperProvider(baseURL, apiKey)
	default:
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}

func isExactoSupported(modelID string) bool {
	supportedModels := []string{
		"moonshotai/kimi-k2-0905",
		"deepseek/deepseek-v3.1-terminus",
		"z-ai/glm-4.6",
		"openai/gpt-oss-120b",
		"qwen/qwen3-coder",
	}
	return slices.Contains(supportedModels, modelID)
}
