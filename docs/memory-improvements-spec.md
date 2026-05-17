# Memory Engine Improvements — Technical Spec

本文档是 `docs/memory-improvements-prd.md` 的技术对应件，给出落地所需的
代码改动、新增类型、数据迁移、配置开关与测试要点。所有变更扩展现有
`internal/memory/engine/` 包，不引入新顶层包，不破坏 `docs/memory-engine-design.md`
的架构边界。

> 命名约定：在不引起歧义时，本文档将 `internal/memory/engine` 包内类型
> 直接以无前缀的形式书写（例如 `Engine`、`SummaryRetriever`）。

## 1. 总体改动地图

```
internal/memory/engine/
├── engine.go                       // (M) 新增 background materializer goroutine + OnBeforeCompaction 返回 rescue
├── materializer.go                 // (M) Materializer 接口新增 Name()，统一 watermark 入口
├── materializer_mental_models.go   // (A) 新增 MentalModelsMaterializer
├── materializer_rollout.go         // (A) 新增 RolloutSummaryMaterializer
├── retriever.go                    // (M) SummaryRetriever：Recall 分层拼接、Retrieve 增加 reranker hook
├── reranker.go                     // (A) Reranker 接口 + heuristic + llm 实现
├── mental_models_seeds.go          // (A) 内置 seed 集合（不暴露用户配置）
├── compaction.go                   // (A) CompactionRescue 构造逻辑
└── ...
internal/memory/hindsight/
├── retriever.go                    // (M) Recall 拼接 mental model 段
├── tagged_replicator.go            // (A) 将本地 mental model 合并产物 replicate 到 hindsight
└── ...
internal/agent/
├── coordinator.go                  // (M) 接 OnBeforeCompaction 的 rescue payload，注入 compaction prompt
└── tools/memory_status.go          // (M) 显示 mental models / background / reranker 状态
internal/config/
└── memory.go                       // (M) 增加 mental_models / reranker / background_materialize 配置字段
```

字母 `(A)` = 新增文件，`(M)` = 修改文件。

## 2. 数据模型

### 2.1 复用 MemoryEvent

不新增表。Mental Model 物化结果不写回 EventStore（它们是派生产物）。
仅扩展 `MaterializedView` 表中的 `view` 命名空间：

| view 名 | 用途 |
|---|---|
| `mental_models.user_preferences` | 用户偏好物化 |
| `mental_models.project_conventions` | 项目约定 |
| `mental_models.decisions` | 决策 |
| `rollout.<session_id>` | 单会话追溯 |

`MaterializedView` 已有 `Watermark`、`UpdatedAt` 字段，可直接复用。

### 2.2 新增类型（Go）

```go
// internal/memory/engine/mental_models_seeds.go
type MentalModelSeed struct {
    Name        string        // 文件 basename，如 "user_preferences"
    Title       string        // markdown 标题
    Description string        // 给 LLM materializer 的指引（可选）
    Filter      MemoryFilter  // 拉取 source 事件的过滤条件
    Budget      int           // 单文件字符上限（默认 4096）
    MaxItems    int           // 最大条目数（默认 20）
}

func DefaultMentalModelSeeds() []MentalModelSeed { /* 内置 */ }
```

```go
// internal/memory/engine/reranker.go
type Reranker interface {
    Rerank(ctx context.Context, query string, candidates []MemoryEvent) ([]MemoryEvent, error)
}

type HeuristicReranker struct{ ... }  // BM25 + importance + 时间衰减
type LLMReranker struct{ Model model.Model; MaxCandidates int }
```

```go
// internal/memory/engine/compaction.go
type CompactionRescue struct {
    Header   string         // 例如 "<compaction-rescue>"
    Items    []MemoryEvent  // 召回到的事件
    Rendered string         // 已渲染为 markdown 的最终 payload
    Bytes    int
}
```

### 2.3 配置（`internal/config`）

```go
type MentalModelsConfig struct {
    Enabled        bool   `json:"enabled"`         // default true
    MaxBytesShare  float64 `json:"max_bytes_share"` // default 0.5
}

type RerankerConfig struct {
    Enabled       bool   `json:"enabled"`         // default false
    Model         string `json:"model"`           // small | large | role name
    MaxCandidates int    `json:"max_candidates"`  // default 30
}

type BackgroundMaterializeConfig struct {
    Enabled        bool          `json:"enabled"`        // default true
    Interval       time.Duration `json:"interval"`       // default 5m
    EveryNTurns    int           `json:"every_n_turns"`  // default 10
}

type CompactionRecallConfig struct {
    Enabled    bool `json:"enabled"`     // default true
    TopK       int  `json:"top_k"`       // default 5
    MaxBytes   int  `json:"max_bytes"`   // default 2048
    UseRerank  bool `json:"use_rerank"`  // default false
}

type MemoryConfig struct {
    // ... existing fields ...
    MentalModels        MentalModelsConfig          `json:"mental_models"`
    Reranker            RerankerConfig              `json:"reranker"`
    BackgroundMaterial  BackgroundMaterializeConfig `json:"background_materialize"`
    CompactionRecall    CompactionRecallConfig      `json:"compaction_recall"`
}
```

## 3. 组件实现要点

### 3.1 MentalModelsMaterializer (F1)

文件：`internal/memory/engine/materializer_mental_models.go`

```go
type MentalModelsMaterializer struct {
    Store        EventStore
    Writer       ArtifactWriter      // 复用 SummaryMaterializer 的 writer
    Seeds        []MentalModelSeed
    Model        model.Model         // 可选：用于精炼条目；nil 走模板渲染
    Now          func() time.Time
}

func (m *MentalModelsMaterializer) Name() string { return "mental_models" }

func (m *MentalModelsMaterializer) Build(ctx context.Context) error {
    for _, seed := range m.Seeds {
        viewName := "mental_models." + seed.Name
        watermark, _ := m.Store.LoadWatermark(ctx, viewName)
        events, newWatermark, _ := m.Store.QuerySince(ctx, seed.Filter, watermark, /*limit*/ 200)
        if len(events) == 0 { continue }

        // 排序：importance desc, CreatedAt desc, 截 MaxItems
        sortAndTrim(events, seed.MaxItems)

        rendered := m.render(seed, events) // 模板或可选 LLM 精炼
        if len(rendered) > seed.Budget {
            rendered = truncate(rendered, seed.Budget)
        }
        path := filepath.Join("mental_models", seed.Name+".md")
        if err := m.Writer.WriteIfChanged(path, rendered); err != nil { return err }
        m.Store.UpdateWatermark(ctx, viewName, newWatermark, m.Now())
    }
    return nil
}
```

Source 事件过滤：默认只接受 `tags` 含 `consolidated_output` 且
`importance ≥ 0.4` 的事件（与 `MemoryMDMaterializer` 一致）。

### 3.2 默认 seeds

文件：`internal/memory/engine/mental_models_seeds.go`

```go
func DefaultMentalModelSeeds() []MentalModelSeed {
    return []MentalModelSeed{
        {
            Name: "user_preferences",
            Title: "User Preferences",
            Filter: MemoryFilter{Scope: ScopeUser, Kinds: []MemoryKind{MemoryKindPreference}},
            Budget: 4096, MaxItems: 20,
        },
        {
            Name: "project_conventions",
            Title: "Project Conventions",
            Filter: MemoryFilter{Scope: ScopeProject, Kinds: []MemoryKind{MemoryKindPreference, MemoryKindProcedure}},
            Budget: 4096, MaxItems: 25,
        },
        {
            Name: "decisions",
            Title: "Decisions",
            Filter: MemoryFilter{Scope: ScopeProject, Kinds: []MemoryKind{MemoryKindDecision}},
            Budget: 4096, MaxItems: 25,
        },
        {
            Name: "pitfalls",
            Title: "Known Pitfalls",
            Filter: MemoryFilter{Scope: ScopeProject, Kinds: []MemoryKind{MemoryKindPitfall}},
            Budget: 3072, MaxItems: 20,
        },
    }
}
```

### 3.3 RolloutSummaryMaterializer (F3)

文件：`internal/memory/engine/materializer_rollout.go`

```go
type RolloutSummaryMaterializer struct {
    Store    EventStore
    Writer   ArtifactWriter
    Now      func() time.Time
    MaxKeep  int            // default 200
    MinEvents int           // default 3
}

func (m *RolloutSummaryMaterializer) Build(ctx context.Context) error {
    // 1. 列出最近被 touch 过的 sessionID（通过 EventStore 新接口 RecentSessions）
    // 2. 对每个 sessionID 拉取 episodic + 关联的 consolidated 事件
    // 3. 渲染为 rollouts/<session_id>_summary.md
    // 4. 按 mtime 老化超过 MaxKeep 的文件
}
```

需要在 `EventStore` 上新增方法：

```go
type EventStore interface {
    // existing...
    RecentSessions(ctx context.Context, since time.Time, limit int) ([]string, error)
}
```

实现上是 `SELECT DISTINCT session_id FROM memory_events
WHERE created_at > ? ORDER BY created_at DESC LIMIT ?`。

### 3.4 SummaryRetriever 分层（F2）

文件：`internal/memory/engine/retriever.go`

```go
func (r *SummaryRetriever) Recall(ctx context.Context, q RecallQuery) (*RecallResult, error) {
    budget := q.MaxBytes
    if budget == 0 { budget = defaultRecallBudget }

    mmBudget := int(float64(budget) * r.cfg.MentalModels.MaxBytesShare)
    if !r.cfg.MentalModels.Enabled { mmBudget = 0 }

    var parts []string
    if mmBudget > 0 {
        if mm, err := r.readMentalModels(mmBudget); err == nil && mm != "" {
            parts = append(parts, mm)
        }
    }

    remaining := budget - sumLen(parts)
    if summary := r.readSummary(remaining); summary != "" {
        parts = append(parts, summary)
        remaining -= len(summary)
    }
    if wm := r.readWorkingMemory(remaining); wm != "" {
        parts = append(parts, wm)
    }

    return &RecallResult{Content: strings.Join(parts, "\n\n---\n\n")}, nil
}
```

`readMentalModels` 顺序按 `DefaultMentalModelSeeds` 的声明顺序读取，遇到
单个文件超 budget 切片，整段超 mmBudget 时按 seed 顺序截断。

### 3.5 Reranker (F5)

```go
// 默认 heuristic：BM25 score (来自 FTS5) + importance + 时间衰减
type HeuristicReranker struct {
    NowFn func() time.Time
}
func (h *HeuristicReranker) Rerank(ctx context.Context, query string, c []MemoryEvent) ([]MemoryEvent, error) {
    // score = bm25 * 0.5 + importance * 0.3 + recencyDecay * 0.2
}

type LLMReranker struct {
    Model         model.Model
    MaxCandidates int
}
func (l *LLMReranker) Rerank(ctx context.Context, query string, c []MemoryEvent) ([]MemoryEvent, error) {
    // 构造 prompt：列出候选 (id + summary)，要求 LLM 返回排序后的 id 列表
    // 解析失败时返回原序，由调用方决定 fallback
}
```

接线：

```go
func (r *SummaryRetriever) Retrieve(ctx context.Context, q RecallQuery) ([]MemoryEvent, error) {
    candidates := r.fts5Search(ctx, q.Query, q.Limit * 3)
    if r.reranker != nil && len(candidates) > 1 {
        if reranked, err := r.reranker.Rerank(ctx, q.Query, candidates); err == nil {
            candidates = reranked
        }
    }
    return trim(candidates, q.Limit), nil
}
```

### 3.6 Background Materializer (F6)

`Engine` 改动：

```go
type Engine struct {
    // existing...
    bgInterval   time.Duration
    bgTurns      int
    turnCounter  atomic.Int32
    bgStop       chan struct{}
    bgDone       chan struct{}
}

func (e *Engine) Start(ctx context.Context) error {
    // existing...
    if e.cfg.BackgroundMaterial.Enabled && e.bgInterval > 0 {
        e.bgStop = make(chan struct{})
        e.bgDone = make(chan struct{})
        go e.backgroundLoop(ctx)
    }
    return nil
}

func (e *Engine) backgroundLoop(ctx context.Context) {
    defer close(e.bgDone)
    t := time.NewTicker(e.bgInterval)
    defer t.Stop()
    for {
        select {
        case <-e.bgStop: return
        case <-ctx.Done(): return
        case <-t.C:
            if !e.IsDegraded() {
                _ = e.TriggerMaterialization(ctx)
            }
        }
    }
}

func (e *Engine) Close() error {
    if e.bgStop != nil {
        close(e.bgStop)
        <-e.bgDone
    }
    return nil
}
```

`AfterTurnIdle`：

```go
func (e *Engine) AfterTurnIdle(ctx context.Context) error {
    // existing extract logic ...
    if e.turnCounter.Add(1) >= int32(e.bgTurns) {
        e.turnCounter.Store(0)
        _ = e.TriggerMaterialization(ctx)
    }
    return nil
}
```

### 3.7 Compaction Recall (F4)

签名扩展：

```go
// 旧：func (e *Engine) OnBeforeCompaction(ctx context.Context) error
func (e *Engine) OnBeforeCompaction(ctx context.Context, in CompactionInput) (*CompactionRescue, error)

type CompactionInput struct {
    SessionID    string
    RecentHints  []string  // 最近 N 条消息的关键片段，由 coordinator 提供
    TokenBudget  int
}
```

实现：

```go
func (e *Engine) OnBeforeCompaction(ctx context.Context, in CompactionInput) (*CompactionRescue, error) {
    _ = e.AfterTurnIdle(ctx)
    _ = e.TriggerMaterialization(ctx)
    if !e.cfg.CompactionRecall.Enabled || e.retriever == nil { return nil, nil }

    query := buildQueryFromHints(in.RecentHints) // 关键词聚合
    items, err := e.retriever.Retrieve(ctx, RecallQuery{
        Query: query, Limit: e.cfg.CompactionRecall.TopK,
    })
    if err != nil || len(items) == 0 { return nil, err }

    rendered := renderRescue(items, e.cfg.CompactionRecall.MaxBytes)
    return &CompactionRescue{Items: items, Rendered: rendered, Bytes: len(rendered)}, nil
}
```

Coordinator 改动（`internal/agent/coordinator.go`）：

- 在调用 compaction 前收集最近 N 条 message 的关键 token（已有提取逻辑可
  复用）。
- 调用 `engine.OnBeforeCompaction(ctx, in)`，把 `Rescue.Rendered` 作为
  额外 system 块插入到压缩 prompt 中（位置：紧跟当前压缩 prompt 头部）。

### 3.8 Hindsight 跨 backend Mental Models (F7)

文件：`internal/memory/hindsight/tagged_replicator.go`

- 监听 EventStore 的合并事件（标签 `consolidated_output`、与 mental
  model seed 匹配）。
- 把这些事件以独立 `retain` 调用提交到 Hindsight，附加 tag
  `kind:mental_model` + `model:<seed-name>`。
- 维护一个独立 watermark `hindsight_mm_replicate`，避免重复 retain。

`Retriever.Recall` 改动：

```go
func (r *Retriever) Recall(ctx context.Context, q RecallQuery) (*RecallResult, error) {
    // 1. 拉取 mental models（tag 过滤）
    mm := r.fetchTagged(ctx, []string{"kind:mental_model"}, "any")
    // 2. 拉取常规 recall（必须排除 kind:mental_model tag）
    normal := r.client.Recall(ctx, withExcludeTag(q, "kind:mental_model"))
    // 3. 拼接：mental models 在前
    return assemble(mm, normal), nil
}
```

需要 oh-my-pi 风格的 Hindsight HTTP API 支持 tag exclude 过滤；如远端不
支持，则在客户端做后置过滤。

### 3.9 可观测性 (F8)

`internal/agent/tools/memory_status.go`：

- 增加 section "Mental Models"，列出每个 view 的 last_refreshed_at、
  size、item_count。
- 增加 section "Background"，显示上次运行时间、间隔、turn counter。
- 增加 section "Reranker"，显示 enabled、type、调用次数计数。

`/memory rebuild --view mental_models` 实现：直接调用对应 Materializer
的 `Build`，并把 watermark 重置为 0。

## 4. 数据迁移

无需破坏性迁移：

- 新 view 名（`mental_models.*`、`rollout.*`）首次写入时由
  `MaterializedView` 表自动 upsert。
- `OnBeforeCompaction` 签名更改需要更新调用方；保留旧无参方法作为内联
  shim 调用新签名（`CompactionInput{}` 空输入），便于分阶段推进。

## 5. 测试要点

T1. `materializer_mental_models_test.go`：
   - 给定 seed + 不同事件集，验证文件内容、watermark 推进、budget 截断。
   - 无新事件时跳过写。
   - 单个 seed 错误不影响其他 seed。

T2. `retriever_layered_test.go`：
   - mental models 段在前。
   - MentalModels.Enabled=false 时回退到旧行为。
   - 总字节预算严格不超。

T3. `reranker_test.go`：
   - HeuristicReranker 与已知输入的稳定排序。
   - LLMReranker 解析失败时返回原序，不丢候选。

T4. `engine_background_test.go`：
   - Close 能在 50ms 内停止 goroutine。
   - degraded 期间不触发物化。
   - turn counter 到阈值触发。

T5. `engine_compaction_test.go`：
   - 空 retriever / disabled 时返回 nil rescue。
   - 超 token budget 时截断。
   - rescue 中事件 ID 可被后续 `recall` 检索到。

T6. `hindsight_mental_models_test.go`：
   - 合并事件被 replicator 以正确 tag retain。
   - Recall 拼接顺序与本地一致。

T7. Coordinator integration test：
   - 长会话注入路径中包含 mental models 段。
   - compaction prompt 中包含 `<compaction-rescue>` 块。

## 6. 配置示例

```json
{
  "options": {
    "memory": {
      "backend": "local",
      "mental_models": { "enabled": true, "max_bytes_share": 0.5 },
      "background_materialize": { "enabled": true, "interval": "5m", "every_n_turns": 10 },
      "compaction_recall": { "enabled": true, "top_k": 5, "max_bytes": 2048 },
      "reranker": { "enabled": false }
    }
  }
}
```

Hindsight 模式：

```json
{
  "options": {
    "memory": {
      "backend": "hindsight",
      "remote": "http://localhost:8888",
      "remote_bank_id": "crush",
      "remote_scoping": "per-project-tagged",
      "mental_models": { "enabled": true }
    }
  }
}
```

## 7. 兼容性

- 默认开关：mental_models = on、background_materialize = on、
  compaction_recall = on、reranker = off。这意味着用户升级后能立即享受
  分层注入和及时物化，不会引入额外模型调用成本。
- 关闭所有新开关时，行为与本 SPEC 之前的代码 1:1 一致。
- `memory_summary.md`、`MEMORY.md`、`skills/` 保持现状，未被替换。

## 8. 实施顺序与里程碑

按 PRD 第 12 节给出的 S1..S7 顺序提交：每步独立 PR，独立验收。

M1 (S1+S2)：Compaction Recall + Background Materializer。
M2 (S3)：Mental Models Materializer + 分层注入。
M3 (S4+S5)：Rollout Summary + Reranker 接口（默认关闭）。
M4 (S6+S7)：Hindsight tagged replicator + memory_status 可观测性扩展。

## 9. 未覆盖的开放问题（来自 PRD）

- mental model seed 类目是否需要扩展（pitfalls、workflows）。本 SPEC
  已包含 `pitfalls`；`workflows` 待 M3 评估。
- Reranker 模型角色：M3 落地时确认是否复用 `small` 还是单独 role。
- Compaction rescue 注入位置：M1 与 compaction owner 在 PR review 中
  对齐，本 SPEC 暂定 system 块紧跟压缩 prompt 头部。
- 周期物化默认值：默认 5m / 10 turns，M1 后根据真实数据微调。
- mental model 文件丢失检测：startup 时扫描 + 写缺失 view 标记为 stale，
  下次 Build 触发重建。Recall 路径不检查文件存在性以避免延迟。
