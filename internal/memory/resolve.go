package memory

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/memory/hindsight"
)

// Deps holds the external dependencies needed to resolve a memory Backend.
type Deps struct {
	// DB is the SQLite connection used by the local engine.
	DB *sql.DB
	// DataDirectory is the base directory for materialized memory artifacts.
	DataDirectory string
	// WorkingDir is the project working directory, used for scoping.
	WorkingDir string
}

// Resolve is the single point of memory backend construction. It replaces
// the former inline switch in app.go, mirroring oh-my-pi's resolve.ts
// "single resolution point" pattern. It returns nil when memory is disabled.
func Resolve(memCfg *config.MemoryConfig, deps Deps) Backend {
	if memCfg == nil {
		// No config means memory is enabled with defaults (local backend).
		memCfg = &config.MemoryConfig{}
	}
	if !memCfg.IsEnabled() {
		return nil
	}

	backend := memCfg.BackendName()
	var bgInterval time.Duration
	var bgEveryNTurns int
	if backend == "local" && memCfg.BackgroundMaterialize.IsEnabled() {
		bgInterval = time.Duration(memCfg.BackgroundMaterialize.GetIntervalSeconds()) * time.Second
		bgEveryNTurns = memCfg.BackgroundMaterialize.GetEveryNTurns()
	}
	var consBgInterval time.Duration
	if backend == "local" && memCfg.BackgroundConsolidation.IsEnabled() {
		consBgInterval = time.Duration(memCfg.BackgroundConsolidation.GetIntervalSeconds()) * time.Second
	}

	eng := engine.New(deps.DB, engine.Config{
		Enabled:               true,
		Backend:               backend,
		BackgroundInterval:    bgInterval,
		BackgroundEveryNTurns: bgEveryNTurns,
		ConsolidationInterval: consBgInterval,
	})

	switch backend {
	case "hindsight":
		return resolveHindsight(eng, memCfg, deps)
	default:
		return resolveLocal(eng, memCfg, deps)
	}
}

// resolveLocal wires the local engine's materializers, retriever, reranker,
// and background loops, then wraps it in a LocalBackend.
func resolveLocal(eng *engine.Engine, memCfg *config.MemoryConfig, deps Deps) *LocalBackend {
	writer := engine.NewArtifactWriter(filepath.Join(deps.DataDirectory, "memory"))
	eng.SetMaterializer(engine.NewSummaryMaterializer(deps.DB, eng.EventStore(), writer))
	eng.SetMaterializer(engine.NewMemoryMDMaterializer(deps.DB, eng.EventStore(), writer))
	eng.SetMaterializer(engine.NewSkillsMaterializer(deps.DB, eng.EventStore(), writer))
	if memCfg.MentalModels.IsEnabled() {
		eng.SetMaterializer(engine.NewMentalModelsMaterializer(deps.DB, eng.EventStore(), writer, engine.DefaultMentalModels()))
	}
	if memCfg.Rollout.IsEnabled() {
		eng.SetMaterializer(engine.NewRolloutSummaryMaterializer(
			deps.DB, eng.EventStore(), writer,
			memCfg.Rollout.GetMaxKeep(),
			memCfg.Rollout.GetMinEvents(),
		))
	}
	summaryRetriever := engine.NewSummaryRetriever(eng.EventStore(), deps.DB, writer.OutputDir()).
		WithTripleStore(eng.TripleStore())
	if memCfg.Reranker.GetMaxCandidates() > 0 {
		summaryRetriever.WithMaxCandidates(memCfg.Reranker.GetMaxCandidates())
	}
	if rerank := buildLocalMemoryReranker(memCfg); rerank != nil {
		eng.SetReranker(rerank)
		summaryRetriever.WithReranker(rerank)
	}
	summaryRetriever.WithEmbeddingPipeline(eng.EmbeddingPipeline())
	eng.SetRetriever(summaryRetriever)

	// Compaction rescue options.
	var rescueOpts *engine.CompactionRescueOptions
	if memCfg.CompactionRecall != nil && memCfg.CompactionRecall.IsEnabled() {
		rescueOpts = &engine.CompactionRescueOptions{
			TopK:        memCfg.CompactionRecall.GetTopK(),
			MaxBytes:    memCfg.CompactionRecall.GetMaxBytes(),
			UseReranker: memCfg.CompactionRecall.GetUseRerank(),
		}
	}

	var backendOpts []LocalBackendOption
	if rescueOpts != nil {
		backendOpts = append(backendOpts, WithCompactionRecall(*rescueOpts))
	}
	b := NewLocalBackend(eng, backendOpts...)

	// Startup materialization.
	go func() {
		if err := eng.TriggerMaterialization(context.Background()); err != nil {
			slog.Warn("Startup memory materialization failed", "error", err)
		}
	}()

	eng.StartBackgroundMaterializer(context.Background())
	eng.StartBackgroundConsolidator(context.Background())

	return b
}

// resolveHindsight wires the hindsight client, transcript retainer, and
// retriever into the engine, then wraps it in a HindsightBackend.
func resolveHindsight(eng *engine.Engine, memCfg *config.MemoryConfig, deps Deps) *HindsightBackend {
	if memCfg.Remote == "" {
		eng.SetDegraded(true, "hindsight backend configured without memory.remote")
		slog.Warn("Hindsight memory backend requires memory.remote")
		return NewHindsightBackend(eng, nil, nil)
	}
	token := memCfg.RemoteToken
	if token == "" {
		token = os.Getenv("HINDSIGHT_API_TOKEN")
	}
	projectLabel := config.ProjectSlug(deps.WorkingDir)
	scope := hindsight.ResolveScope(memCfg.RemoteBankID, memCfg.RemoteScopingName(), projectLabel)
	hsClient := hindsight.NewClient(memCfg.Remote, scope.BankID, token)
	eng.SetTranscriptRetainer(hindsight.NewTranscriptRetainer(
		hsClient,
		hindsight.WithRetainTags(scope.RetainTags),
	))
	retriever := hindsight.NewRetriever(
		hsClient,
		hindsight.WithRecallTags(scope.RecallTags, scope.RecallTagsMatch),
	)
	eng.SetRetriever(retriever)

	go func() {
		if err := hsClient.EnsureBank(context.Background(), ""); err != nil {
			slog.Warn("Hindsight EnsureBank failed", "error", err)
		}
	}()
	slog.Info(
		"Hindsight remote memory enabled",
		"url", memCfg.Remote,
		"bank", hsClient.BankID(),
		"scoping", memCfg.RemoteScopingName(),
		"project", projectLabel,
	)

	// Hindsight has no local background loops; the engine's no-op when
	// intervals are 0, but we call them for parity with the local path.
	eng.StartBackgroundMaterializer(context.Background())
	eng.StartBackgroundConsolidator(context.Background())

	return NewHindsightBackend(eng, hsClient, retriever)
}

// buildLocalMemoryReranker builds a reranker from the memory config. Moved
// from app.go to co-locate with backend assembly.
func buildLocalMemoryReranker(memCfg *config.MemoryConfig) engine.Reranker {
	if memCfg == nil {
		return nil
	}
	if memCfg.Embeddings != nil && memCfg.Embeddings.IsEnabled() {
		switch memCfg.Embeddings.BackendName() {
		case "hashing", "":
			return engine.NewEmbeddingReranker(engine.NewHashingEmbedder(memCfg.Embeddings.GetDimensions()))
		case "provider":
			embedder := engine.NewProviderEmbedder(engine.ProviderEmbedderConfig{
				APIURL:     memCfg.Embeddings.ProviderAPIURL,
				APIKey:     memCfg.Embeddings.ProviderAPIKey,
				Model:      memCfg.Embeddings.ProviderModel,
				Dimensions: memCfg.Embeddings.GetDimensions(),
			})
			return engine.NewEmbeddingReranker(embedder)
		default:
			slog.Warn("Memory embedding backend not implemented, falling back to hashing",
				"backend", memCfg.Embeddings.BackendName())
			return engine.NewEmbeddingReranker(engine.NewHashingEmbedder(memCfg.Embeddings.GetDimensions()))
		}
	}
	if !memCfg.Reranker.IsEnabled() {
		return nil
	}
	switch memCfg.Reranker.GetType() {
	case "embedding", "hybrid":
		dimensions := 384
		if memCfg.Embeddings != nil {
			dimensions = memCfg.Embeddings.GetDimensions()
		}
		return engine.NewEmbeddingReranker(engine.NewHashingEmbedder(dimensions))
	case "heuristic", "":
		return engine.NewHeuristicReranker()
	default:
		slog.Warn("Memory reranker type not implemented, falling back to heuristic",
			"type", memCfg.Reranker.GetType())
		return engine.NewHeuristicReranker()
	}
}
