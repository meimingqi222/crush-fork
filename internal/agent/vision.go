package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/imageutil"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
)

// defaultVisionDescribePrompt is the instruction sent to the vision model when
// the caller does not provide a custom prompt.
const defaultVisionDescribePrompt = "Describe this image in detail. Include any text visible in the image, the overall scene, objects, people, colors, and any notable features. Be thorough but concise."

// visionCacheTTL is how long a cached description remains valid.
const visionCacheTTL = 30 * time.Minute

type visionCacheEntry struct {
	description string
	expiresAt   time.Time
}

// VisionService describes images using a separately configured vision-capable
// model. It is used as a fallback when the primary (coder) model does not
// support image inputs.
type VisionService struct {
	coordinator *coordinator
	cacheMu     sync.Mutex
	cache       map[string]visionCacheEntry
}

// NewVisionService creates a VisionService backed by the given coordinator.
// The coordinator is used to resolve the vision model and build the LLM agent.
func NewVisionService(c *coordinator) *VisionService {
	return &VisionService{
		coordinator: c,
		cache:       make(map[string]visionCacheEntry),
	}
}

// IsAvailable returns true when a vision model slot is configured with a
// provider. The vision slot is user-selected, so do not require provider model
// metadata to advertise image support: custom/private gateways can support a
// model even when it is not listed in provider.models or models.dev.
func (v *VisionService) IsAvailable() bool {
	_, _, ok := v.configuredVisionSelection()
	return ok
}

// DescribeImage sends the image data to the configured vision model and returns
// a text description. The prompt parameter customizes the instruction; pass an
// empty string to use the default. Results are cached by image hash for
// visionCacheTTL to avoid redundant API calls.
func (v *VisionService) DescribeImage(ctx context.Context, data []byte, mimeType string, prompt string) (string, error) {
	if !v.IsAvailable() {
		return "", fmt.Errorf("vision model is not configured")
	}
	if len(data) == 0 {
		return "", fmt.Errorf("no image data provided")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = defaultVisionDescribePrompt
	}

	// Compress large images before sending to the vision model.
	compressCfg := imageutil.DefaultCompressionConfig()
	compressed, compressErr := imageutil.CompressImage(data, mimeType, compressCfg)
	if compressErr == nil && compressed != nil {
		data = compressed.Data
		mimeType = compressed.MimeType
	}

	// Check cache.
	hash := imageHash(data, mimeType, prompt)
	v.cacheMu.Lock()
	if entry, ok := v.cache[hash]; ok && time.Now().Before(entry.expiresAt) {
		v.cacheMu.Unlock()
		return entry.description, nil
	}
	v.cacheMu.Unlock()

	model, providerCfg, err := v.resolveVisionModel(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to resolve vision model: %w", err)
	}

	maxOutputTokens := int64(2048)
	if model.CatwalkCfg.DefaultMaxTokens > 0 && model.CatwalkCfg.DefaultMaxTokens < maxOutputTokens {
		maxOutputTokens = model.CatwalkCfg.DefaultMaxTokens
	}

	ag := fantasy.NewAgent(
		model.Model,
		fantasy.WithMaxOutputTokens(maxOutputTokens),
		fantasy.WithUserAgent(userAgent),
	)

	userMsg := fantasy.NewUserMessage("")
	userMsg.Content = append(userMsg.Content, fantasy.FilePart{
		Data:      data,
		MediaType: mimeType,
		Filename:  "image",
	})
	userMsg.Content = append(userMsg.Content, fantasy.TextPart{Text: prompt})

	var result strings.Builder
	streamResult, err := ag.Stream(
		copilot.ContextWithInitiatorType(ctx, copilot.InitiatorAgent),
		fantasy.AgentStreamCall{
			Messages:        []fantasy.Message{userMsg},
			ProviderOptions: getProviderOptions(model, providerCfg),
			PrepareStep: func(callCtx context.Context, _ fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
				return copilot.ContextWithInitiatorType(callCtx, copilot.InitiatorAgent), fantasy.PrepareStepResult{}, nil
			},
			OnTextDelta: func(_ string, text string) error {
				result.WriteString(text)
				return nil
			},
		},
	)
	if err != nil {
		return "", fmt.Errorf("vision model request failed: %w", err)
	}

	v.recordVisionUsage(ctx, model, streamResult.TotalUsage)

	description := strings.TrimSpace(result.String())
	if description == "" {
		return "", fmt.Errorf("vision model returned an empty description")
	}

	// Cache the result.
	v.cacheMu.Lock()
	v.cache[hash] = visionCacheEntry{
		description: description,
		expiresAt:   time.Now().Add(visionCacheTTL),
	}
	// Opportunistic cleanup of expired entries.
	for k, e := range v.cache {
		if !time.Now().Before(e.expiresAt) {
			delete(v.cache, k)
		}
	}
	v.cacheMu.Unlock()

	return description, nil
}

// recordVisionUsage persists vision-helper token cost on the active session.
// Vision calls are auxiliary and must not overwrite PromptTokens, which tracks
// the latest main-conversation context length.
func (v *VisionService) recordVisionUsage(ctx context.Context, model Model, usage fantasy.Usage) {
	if v == nil || v.coordinator == nil || v.coordinator.sessions == nil {
		return
	}
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0 {
		return
	}

	sess, err := v.coordinator.sessions.Get(ctx, sessionID)
	if err != nil {
		return
	}

	modelConfig := model.CatwalkCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	sess.Cost += cost
	sess.CompletionTokens += usage.OutputTokens
	_, _ = v.coordinator.sessions.Save(ctx, sess)
}

func (v *VisionService) configuredVisionSelection() (config.SelectedModel, config.ProviderConfig, bool) {
	if v == nil || v.coordinator == nil || v.coordinator.cfg == nil {
		return config.SelectedModel{}, config.ProviderConfig{}, false
	}
	cfg := v.coordinator.cfg.Config()
	if cfg == nil {
		return config.SelectedModel{}, config.ProviderConfig{}, false
	}
	selectedModel, ok := cfg.SelectedModelForType(config.SelectedModelTypeVision)
	if !ok || strings.TrimSpace(selectedModel.Provider) == "" || strings.TrimSpace(selectedModel.Model) == "" {
		return config.SelectedModel{}, config.ProviderConfig{}, false
	}
	providerCfg, ok := cfg.Providers.Get(selectedModel.Provider)
	if !ok {
		return config.SelectedModel{}, config.ProviderConfig{}, false
	}
	return selectedModel, providerCfg, true
}

func (v *VisionService) resolveVisionModel(ctx context.Context) (Model, config.ProviderConfig, error) {
	model, providerCfg, err := v.coordinator.selectedModel(ctx, config.SelectedModelTypeVision, false)
	if err == nil {
		return model, providerCfg, nil
	}
	if !errors.Is(err, errTargetModelNotFound) {
		return Model{}, config.ProviderConfig{}, err
	}

	selectedModel, providerCfg, ok := v.configuredVisionSelection()
	if !ok {
		return Model{}, config.ProviderConfig{}, fmt.Errorf("vision model is not configured")
	}

	catwalkModel := catwalk.Model{
		ID:               selectedModel.Model,
		Name:             selectedModel.Model,
		DefaultMaxTokens: selectedModel.MaxTokens,
		ContextWindow:    selectedModel.ContextWindow,
		SupportsImages:   true,
	}
	thinkingDisabled := selectedModel.Think != nil && !*selectedModel.Think
	provider, err := v.coordinator.buildProvider(providerCfg, catwalkModel, false, thinkingDisabled)
	if err != nil {
		return Model{}, config.ProviderConfig{}, err
	}

	modelID := selectedModel.Model
	if selectedModel.Provider == "openrouter" && isExactoSupported(modelID) {
		modelID += ":exacto"
	}
	languageModel, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return Model{}, config.ProviderConfig{}, err
	}

	return Model{
		Model:      languageModel,
		CatwalkCfg: catwalkModel,
		ModelCfg:   selectedModel,
	}, providerCfg, nil
}

// imageHash computes a SHA-256 hash of the image data, MIME type, and prompt
// for use as a cache key.
func imageHash(data []byte, mimeType, prompt string) string {
	h := sha256.New()
	h.Write(data)
	h.Write([]byte(mimeType))
	h.Write([]byte(prompt))
	return hex.EncodeToString(h.Sum(nil))
}

// Ensure VisionService satisfies the tools.VisionDescriber interface.
var _ tools.VisionDescriber = (*VisionService)(nil)
