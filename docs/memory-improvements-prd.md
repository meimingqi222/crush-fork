# Memory Engine Improvements — PRD

## 1. 背景

`docs/memory-engine-design.md` 已经确立了 Crush Memory Engine 的事件源
（EventStore）+ 提取（Extractor）+ 合并（Consolidator）+ 物化
（Materializer）+ 召回（Retriever）的整体架构，并且大部分组件已经在
`internal/memory/engine/` 落地。这份 PRD 不重新设计架构，而是聚焦在
**本地 backend 的检索准确率、物化及时性、记忆分层** 三个已知缺陷，并把
Hindsight 远程 backend 中可借鉴的机制（Mental Models、压缩前召回、两阶段
流水线、按会话追溯）补齐到本地 backend，使两种 backend 在能力上对齐而
不强迫用户接受远程依赖或外部向量嵌入服务。

参考实现：
- `D:/code/copilot-refs/crush/internal/memory/engine/` — Crush 本地 backend。
- `D:/code/copilot-refs/crush/internal/memory/hindsight/` — Crush Hindsight backend。
- `D:/code/copilot-refs/oh-my-pi/packages/coding-agent/src/memories/` —
  oh-my-pi 本地两阶段流水线。
- `D:/code/copilot-refs/oh-my-pi/packages/coding-agent/src/hindsight/` —
  oh-my-pi 远程后端（Mental Models、preCompactionContext、anti-feedback wrapping）。

## 2. 当前实现基线

下列能力**已经实现**，本 PRD 不再视为待办：

| 模块 | 现状 | 关键文件 |
|---|---|---|
| EventStore (SQLite) | 已实现，含 FTS5 索引 | `engine/store.go` |
| LLMExtractor | 已实现，按 sessionID 提取 | `engine/extractor.go` |
| LLMConsolidator | 已实现，含 Supersedes 语义和 `consolidated_output` 标签 | `engine/consolidator.go` |
| SummaryMaterializer → `memory_summary.md` | 已实现 | `engine/materializer_memory_summary.go` |
| MemoryMDMaterializer → `MEMORY.md` | 已实现 | `engine/materializer_memory_md.go` |
| SkillsMaterializer → `skills/SKILL.md` | 已实现 | `engine/materializer_skills.go` |
| SummaryRetriever (FTS5 + 关键词 fallback) | 已实现 | `engine/retriever.go` |
| 自动召回注入（async prefetch + `<system-reminder>`） | 已实现 | `agent/coordinator.go:586-647`、`agent/recall.go` |
| `recall` 工具（有参 Retrieve / 无参 Recall） | 已实现 | `agent/tools/recall.go` |
| Consolidation lease | 已实现，180s TTL | `engine/engine.go:256-298` |
| Degraded mode | 已实现 | `engine/engine.go:323-342` |
| Hindsight TranscriptRetainer + Retriever | 已实现 | `memory/hindsight/*.go` |
| Hindsight Scope（global / per-project / per-project-tagged） | 已实现 | `memory/hindsight/scope.go` |

## 3. 待解决的问题

### 3.1 本地召回准确率差
`SummaryRetriever.Retrieve` 当前路径是 FTS5 MATCH → BM25 排序 → fallback
到 `strings.Contains` 关键词匹配。这套词法检索对自然语言查询召回精度有
限，相同语义不同用词（如 "压缩超时" vs "compaction timeout"）就会
miss。`memory_summary.md` 是一坨混合摘要，无法按知识类型分层注入。
用户已明确反对引入向量嵌入服务，因此改进必须在 **不引入外部嵌入依赖**
的前提下解决。

### 3.2 物化文件不及时
当前 `TriggerMaterialization` 仅在 `AfterTurnIdle`（且有可物化事件）和
`OnSessionClosed` 触发。当一个长会话不结束、且单轮无可物化事件时，本地
`memory_summary.md` 不会刷新；用户和模型都看不到最新合并结果。

### 3.3 没有 Mental Models 分层
当前所有合并结果混在 `memory_summary.md`。用户偏好和项目约定这类稳定
知识，会被一次性话题（任务状态、临时讨论）淹没。oh-my-pi 的 Mental
Models 把这些抽离成独立、低频更新的命名块，注入时作为稳定前缀，比
全文摘要更鲁棒。

### 3.4 缺少按会话追溯
当前合并是黑盒：一旦 episodic 事件被合并掉，难以追溯某条 semantic 记忆
来自哪个会话、原始提取内容长什么样。oh-my-pi 通过 `rollout_summaries`
表 + `<session_id>_summary.md` 实现追溯，每个会话有独立 stage1 输出。

### 3.5 压缩前没有主动召回
`OnBeforeCompaction` 当前只调用 `AfterTurnIdle` 做最后一次提取，并触发
物化。它**不会**主动召回历史相关记忆注入到压缩 prompt 中。Hindsight
backend 的 `preCompactionContext` 已经做了这件事，本地 backend 没做。

### 3.6 LLM 重排序未启用
即使有 FTS5 候选集，当前 Retrieve 也没有任何二次排序，BM25 之外的语义
相关性完全没有用到 LLM 能力。Reflect 接口存在但只在用户显式调用时使用。

### 3.7 Hindsight backend 与本地 backend 能力不对齐
Hindsight 模式没有 Mental Models（虽然它有原始 recall），也没有 lease
+ degraded 模式，远程服务抖动时会直接错误。

## 4. 目标

T1. 提升本地召回准确率，不引入外部嵌入服务。

T2. 让物化在会话进行中也能及时刷新，关键路径不超过 N 轮或 T 秒。

T3. 引入 Mental Models 分层，把稳定知识从 `memory_summary.md` 抽离，作为
稳定前缀注入。

T4. 增加按会话级追溯文件，便于调试和重建。

T5. 压缩前主动召回相关历史记忆，避免压缩后丢失关键背景。

T6. 把 Mental Models 同样适配到 Hindsight backend，让两个 backend 在能力
上对齐。

T7. 不破坏 `docs/memory-engine-design.md` 既定的架构边界（单一写入入口、
两 backend 互斥、不混查）。

## 5. 非目标

NG1. 不引入向量嵌入服务（本地小模型 or 远程 API）。

NG2. 不混查 local 和 hindsight。

NG3. 不再退化为旧 `dream` 或 `extractMemories` 直写长期记忆的语义。

NG4. 不删除现有 `memory_summary.md` 和 `MEMORY.md`，Mental Models 与它们
共存。

NG5. 不在第一阶段做完全的 P2P 一致性、多机协作或跨设备同步。

NG6. 本期不允许 LLM 直接生成 Mental Models 的 source query 配置，必须从
代码内置 seeds 走出来。

## 6. 用户场景

### 6.1 长会话中召回失败
用户在第 30 轮提问"我们之前为什么选 sqlite 而不是 bbolt？"。当前路径：
模型调用 `recall` 工具，FTS5 拿到一堆包含 "sqlite" 的事件，但"为什么选"
没匹配到，BM25 排序无关，返回错的事件。改进后：FTS5 取 top-30 候选，
LLM 重排序按"决策原因"语义筛选，返回 top-5；同时 Mental Models 中
`decisions.md` 已经稳定收录该决策，作为稳定前缀注入。

### 6.2 长会话物化滞后
用户进行一个 40 轮的重构会话，期间手动 `retain` 了一条偏好。当前：要等
会话关闭才合并并写入 `memory_summary.md`。改进后：周期性后台 worker（或
轮数阈值）触发物化，新偏好在数轮内就出现在 `mental_models/user_preferences.md`。

### 6.3 压缩后丢失背景
会话已经被压缩两次，用户问"我们最早怎么排查这个 bug 的"。当前：被压缩
的细节已丢失，模型只能从最近上下文猜测。改进后：压缩前 `Retrieve` 关
键事件并注入到压缩 prompt 中，压缩后的摘要保留了关键事件指针。

### 6.4 调试为何某条记忆出现
用户打开 `MEMORY.md` 看到一条不知所云的 decision。当前：无法追溯来源。
改进后：打开 `rollouts/<session_id>_summary.md` 可以直接看到该会话的
原始提取片段和提取时间。

## 7. 功能需求

### F1. Mental Models Materializer

- 引入 `MentalModelsMaterializer`，按 seed 配置物化为多个独立 Markdown
  文件：
  - `mental_models/user_preferences.md` — 用户偏好（来源：scope=user 或
    kind=preference 的合并事件）。
  - `mental_models/project_conventions.md` — 项目约定（来源：scope=project
    + kind=preference/procedure 的合并事件）。
  - `mental_models/decisions.md` — 架构决策（来源：scope=project +
    kind=decision 的合并事件）。
- seed 集合 hardcoded 在 Go 代码里（参考 oh-my-pi 的 `seeds.json`
  but compiled-in），不暴露为用户配置。
- 每个 mental model 文件有 char budget（默认 4KB）和 last-refreshed
  marker。
- Materializer 走标准 watermark 机制，事件没新增不刷新。

### F2. Layered Recall 注入

- `SummaryRetriever.Recall` 改为分层拼接：
  1. Mental Models（稳定前缀，按 seed 顺序）。
  2. `memory_summary.md`（动态摘要）。
  3. 当前会话 working memory。
- 注入顺序保证稳定前缀在前，volatile 内容在后，命中 prompt cache 的
  概率更高。
- 总字节预算继承现有 `maxSessionRecallBytes = 60 KB`，分配比例：
  Mental Models 最多占 50%，剩余分给 summary + working memory。

### F3. Rollout Summary Materializer

- 引入 `RolloutSummaryMaterializer`，按 sessionID 物化为
  `rollouts/<session_id>_summary.md`。
- 每个文件包含：会话首末时间、提取出的 episodic 事件列表（摘要 + 标签）、
  关联的 consolidated 事件 ID（如果该会话的事件已被合并）。
- 仅当某 sessionID 的事件超过阈值（如 ≥3 个 durable 事件）才生成。

### F4. Compaction Recall

- `Engine.OnBeforeCompaction` 在现有 `AfterTurnIdle` + `TriggerMaterialization`
  之后，新增主动召回：
  - 构造 query：从最近 N 条消息提取关键 token；或者用固定 query
    "decisions, pitfalls, procedures relevant to current task"。
  - 调用 `retriever.Retrieve(query, opts)` 取 top-K。
  - 把结果格式化为 `<compaction-rescue>` 块，返回给调用方（compaction 流程）
    用于注入到压缩 prompt 中。
- 这要求 `OnBeforeCompaction` 签名扩展为返回 rescue payload（或通过
  context 注入）。

### F5. LLM Reranker（可选）

- 在 `SummaryRetriever.Retrieve` 中，FTS5 取 top-K\*3 候选；若配置了
  reranker，调用 LLM 对候选打分并重排，截取 top-K。
- Reranker 只是 `func(ctx, query, candidates) ([]MemoryEvent, error)`
  接口，不强制 LLM 实现；调用方可注入任何 ranker。
- 不引入新的模型角色，复用现有 `Model`。默认 reranker 用 small model
  以控制成本。
- 默认关闭，通过 `memory.reranker.enabled=true` 启用。

### F6. 周期物化触发

- Engine 增加可选 `BackgroundMaterializeInterval`（默认 5 分钟）。
- 后台 goroutine 在 enabled 且非 degraded 时周期调用
  `TriggerMaterialization`。
- 关闭时（`Close`）正确停止 goroutine。
- 同时增加按"轮数"触发：每 N 轮（默认 10）`AfterTurnIdle` 强制物化，
  即使没有新事件。

### F7. Mental Models 跨 backend

- 在 Hindsight backend 模式下，把同样的 seed 配置复用：通过
  `hindsight.Materializer` 把 mental model 的合并产物以特殊 tag
  （`kind:mental_model`, `model:user-preferences` 等）retain 到远程。
- Hindsight `Retriever.Recall` 在拼接结果前，先做一次按 tag 的 mental
  model 拉取（`tags=["kind:mental_model"], tags_match=any`），格式化为
  与本地一致的稳定前缀。

### F8. 可观测性

- `/memory status` 增加：mental models 各 view 的 last_refreshed_at、
  reranker 状态、background materializer 状态。
- `/memory view mental` 直接 dump 当前 mental models 块。
- `/memory rebuild --view mental_models` 强制重建。

## 8. 不做的事情（具体）

- 不在本期引入 sentence transformers / ONNX 嵌入 / 远程嵌入 API。
- 不允许 reranker 写回 EventStore。
- 不让 mental models seed 通过 YAML/JSON 外部配置（避免 LLM 注入风险）。
- 不在 hindsight backend 引入本地 SQLite materialization 作为 fallback
  召回源（违反 `memory-engine-design.md` 的单 backend 召回原则）。
- 不在压缩 prompt 里追加无界长度的 rescue 块，必须有 token budget。

## 9. 验收标准

A1. 在一个含 ≥50 个合并事件的项目中，发起一次自然语言 query（如"我们
为什么选 sqlite"），分层注入 + LLM reranker 召回的 top-3 中至少有一条
是该决策事件本身。基线：纯 FTS5 通常返回不相关的事件。

A2. 在一个 30 轮以上的活跃会话中，手动 `retain` 一条用户偏好后，最多
N=10 轮（或 T=5 分钟，取先到）内，`mental_models/user_preferences.md`
反映出该偏好。

A3. 关闭 Mental Models 的开关时（`memory.mental_models.enabled=false`），
召回拼接退化为现有行为，不会出现空块或格式错误。

A4. `OnBeforeCompaction` 返回的 rescue payload 在 token budget 内（默认
≤2KB），并出现在 compaction prompt 中；压缩后能通过 `recall` 工具查询
到原始事件。

A5. 任一 mental model 文件丢失时，下次 `/memory rebuild` 能从 EventStore
完整重建，不依赖之前的中间文件。

A6. 启用 Hindsight backend 时，远程 bank 上能查到 `kind:mental_model` 的
tagged 项，且 `Retriever.Recall` 注入的内容里包含 mental model 段。

A7. 关闭 reranker 时，Retrieve 路径与启用前 1:1 兼容，不引入额外延迟。

A8. `/memory status` 显示 background materializer 的最近一次运行时间，
没有 stuck 在 `running`。

## 10. 风险与权衡

R1. **LLM reranker 成本**。每次 Retrieve 多花一次小模型调用。缓解：默认
关闭；启用时仅在用户主动 query 时使用，自动召回路径不走 reranker；提供
本地启发式 ranker 作为零成本备选（关键词权重 + importance + 时间衰减）。

R2. **mental model 漂移**。LLM 物化 mental model 时可能跑偏，把临时事件
误判为偏好。缓解：mental model materializer 只读已经被 consolidator 标
记为 `consolidated_output` 且 importance ≥ 阈值的事件，源头已经过一次过
滤。

R3. **物化抖动**。周期物化 + 轮数物化叠加可能导致频繁写盘。缓解：
ArtifactWriter 已经做内容 hash 去重；进一步在 materializer 内部跳过
watermark 未推进的情况。

R4. **Hindsight tag 污染**。把 mental models 用 tag 复制到 Hindsight 可能
让远程检索结果重复出现。缓解：用专用 tag 命名空间 `kind:mental_model`，
Retriever 在普通 recall 时显式排除该 tag。

R5. **rollout summary 数量爆炸**。会话非常多时 `rollouts/` 文件会很多。
缓解：保留最近 200 个 + 按 mtime 老化；提供 `/memory clear rollouts`。

R6. **依赖既有 LLM 模型**。reranker / mental model materializer 都需要
模型。模型不可用时整个流水线降级。缓解：复用现有 `degraded` 模式；
materializer 不依赖模型，只做模板渲染；reranker 失败时直接走 BM25。

## 11. 与 oh-my-pi / Hindsight 的对齐

下表标注每条改进的灵感来源：

| 改进 | 借鉴自 | Crush 已有部分 |
|---|---|---|
| Mental Models | oh-my-pi/hindsight + seeds.json | 无 |
| Rollout summaries | oh-my-pi memories/storage (stage1 outputs) | 无 |
| Compaction recall | oh-my-pi hindsight `preCompactionContext` | 仅 OnBeforeCompaction 触发提取 |
| 两阶段并行流水线 | oh-my-pi memories phase1/phase2 | Crush 已经是 Extractor + Consolidator 两阶段，仅缺并行度 |
| LLM reranker | 自有改进（不是直接抄过来）| FTS5 + 关键词 fallback |
| 周期物化 | oh-my-pi startup task | 仅事件驱动 |
| Mental models 跨 backend | 自有改进 | Hindsight 只有 raw recall |

## 12. 实施顺序

按 ROI 排序，每项独立可上线：

S1. **Compaction Recall** (F4) — 最快收益，改动小。

S2. **周期物化** (F6) — 低复杂度，立刻解决物化滞后。

S3. **Mental Models Materializer + 分层注入** (F1 + F2) — 中等复杂度，
显著提升注入质量。

S4. **Rollout Summary Materializer** (F3) — 低复杂度，提升可观测性。

S5. **LLM Reranker** (F5) — 默认关闭，先打通接口。

S6. **Mental Models 跨 backend** (F7) — 在 Hindsight 已有路径上加 tag。

S7. **可观测性补充** (F8) — 配合上述每步同步落地。

## 13. 开放问题

Q1. Mental Model seed 集合需要哪些类目？当前提案三类（user-preferences /
project-conventions / decisions）是否足够？是否需要 `pitfalls.md`、
`workflows.md`？需要在 SPEC 里给出最终列表。

Q2. Reranker 调用模型角色：复用 `small` 还是新引入 `memory.reranker`？

Q3. Compaction rescue payload 注入位置：作为新的 system message，还是
合并进现有压缩 prompt？需要和 compaction owner 对齐。

Q4. 周期物化的默认间隔与轮数阈值需要根据真实会话长度数据校准，建议先
默认 5 分钟 / 10 轮，留 config knob。

Q5. Mental Model 文件丢失检测：是 startup 时一次性扫描，还是每次
Recall 之前检查？后者代价更稳但更慢。
