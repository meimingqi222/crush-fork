> **HISTORICAL - DO NOT USE AS REFERENCE.** This document is archived; it describes a design that has been implemented and may diverge from the current code. The current code is the authoritative source.

# 记忆机制审计与重构方案（对照 oh-my-pi）

> 状态：**大部分已实施**。Phase 1–4、6 全部完成；Phase 5 中 P5.3/P5.4/P5.5
> 已实施，P5.1/P5.2 待决策。基于 2026-07 代码审计，对照 oh-my-pi
> `packages/coding-agent/src/{memory-backend,memories,hindsight,tools}` 与
> `packages/mnemopi`。行号以撰写时 main 分支为准，实施以符号名为锚。
> 与 `docs/refactor-subagent-planmode.md` 同系列，可独立实施。

## 0. 两边架构速览

**oh-my-pi**：一个 `MemoryBackend` 接口（`memory-backend/types.ts`），四个互斥
实现（off / local / hindsight / mnemopi），唯一解析点 `resolveMemoryBackend()`。
生命周期钩子显式声明（start / buildDeveloperInstructions / clear / enqueue /
status / search / save / stats / diagnose / beforeAgentStartPrompt /
preCompactionContext）。LLM 工具（recall/retain/reflect/edit/learn）全部走
`static createIf(session)` 按 backend 门控——backend 不支持就**不注册**。
local backend 是"启动时批处理"：闲置 rollout → SQLite 任务队列（lease/
heartbeat/重试）→ stage1 逐会话摘要 → phase2 全局合并产出
memory_md / memory_summary / skills；**会话进行中零额外 LLM 开销**。
用户面有完整 `/memory` 命令族（clear/enqueue/stats/diagnose/search/save）。

**crush（重构后）**：`memory.Backend` 接口（`internal/memory/backend.go`）+
`memory.Resolve` 工厂（`internal/memory/resolve.go`，唯一解析点）替换了
原 app.go 内联 switch。两个实现：`LocalBackend`（包装 `engine.Engine` 管道）
与 `HindsightBackend`（包装 client + TranscriptRetainer + Retriever）。
`engine.Engine` 降为 LocalBackend 内部实现。工具面经 `memoryTools()` 按
`Capabilities()` 门控（off → 零工具；reflect 看 `caps.Reflect`；graph 看
`caps.Triples`）；原 `graph_query` 与 `triple_query` 合并为单个 `graph` 工具。
`retain` 已免 permission 弹窗。用户面 Commands 面板有 Memory: Status /
Search / Consolidate Now / Clear 四命令。Prompt 层 `coder.md.tpl` 的
`<memory_instructions>` 与 `retain.md` 写清三机制分工与反重复规则。
运行期：proactive linker 改 enqueue/drain 串行执行（P5.3）；compaction 后
working memory 受 discarded-token 阈值门控（P5.4）；auto-recall prefetch
默认关 reranker（P5.5）。**仍待决策**：每回合抽取改后台批处理（P5.1/P5.2）。

## 1. 审计发现

> 以下为重构前的发现。状态列标注当前解决情况。

| # | 类别 | 问题 | 位置 | 状态 |
|---|------|------|------|------|
| M1 | 死代码 | `sessionState.firstTurnInjected` 声明后全库零引用；`pendingWrites` 只在 `engine.go:529` 写入、从未读取；`Engine.ShouldUpdateWorkingMemory`（engine.go:636）**零调用方**——连带 `workingMemoryThrottle` 字段与 `Config.WorkingMemoryThrottle` 整条配置旋钮都是死的 | `internal/memory/engine/engine.go:15-17,51,110-113,636-649` | ✅ 已清除（Phase 1） |
| M2 | 架构混乱 | backend 切换是字符串散弹式分支而非接口多态：`SetMemoryEngine` 里 `if eng.Backend() != "hindsight"` 决定是否接抽取器（coordinator.go:385）；`memoryEngineHooks.OnBeforeCompaction` 内嵌两条完全不同的路径（coordinator.go:491-538）；Run 的 prefetch 里 `if Backend() == "hindsight"` 加载 mental models（coordinator.go:810）；`buildAutoRecallBlock` 把 backend 字符串一路传参（recall.go:26）；app.go 用 120 行内联 switch 组装（app.go:234-349）。对比 oh-my-pi 的单接口单解析点 | `internal/agent/coordinator.go`、`internal/app/app.go` | ✅ 已解决（Phase 2） |
| M3 | 工具冗余 | 6 个记忆工具**无条件注册**（tool_registration.go:133-138），engine 关闭时依赖传 nil，LLM 看到 6 个一调用就报 "Memory engine is not available" 的工具——浪费上下文 token 且教坏模型。oh-my-pi 用 `createIf` 做到"不支持就不存在"。`graph_query` 与 `triple_query` 查询同一个 TripleStore（两个工具一份数据）；hindsight backend 下 TripleStore 根本没有数据源，这两个工具仍被注册 | `internal/agent/tool_registration.go:133-138` | ✅ 已解决（Phase 3） |
| M4 | 概念重叠 | LLM 面同时存在**三套"记住"机制**且无仲裁：① CRUSH.md 记忆文件（coder.md.tpl:266 `<memory_instructions>` 指示主动更新）；② `retain` 工具（写 engine 事件库，还要走一次 permission 弹窗）；③ 后台自动抽取（AfterTurnIdle 每回合从转写里抽一遍）。同一事实可能被存三处或哪都不存，检索时也不互通（记忆文件不进 retriever） | `internal/agent/templates/coder.md.tpl:266`、`internal/agent/tools/retain.go:52`、`internal/memory/engine/engine.go:477` | ✅ 已解决（Phase 6 + Phase 3 §4） |
| M5 | 成本失衡 | 运行期管道过重：每回合 `AfterTurnIdle` 触发一次**抽取 LLM 调用** + embedding 入队 + proactive linker goroutine + 条件物化（再一批 LLM 调用）+ 回合计数后台 pass；每回合 prefetch 一次 recall（可再触发 reranker）；compaction 后再一次 working-memory LLM 调用；另有两条独立后台循环（物化/合并）。goroutine 各自为政，仅 consolidation 有 lease，其余无统一队列与并发上限。oh-my-pi local 的策略是"会话中零开销、启动时带租约批处理" | `internal/memory/engine/engine.go:477-552`、`internal/agent/coordinator.go:793-826,922-926`、`internal/agent/agent.go:3089-3092` | ⏳ 部分解决（P5.3/P5.4/P5.5 已实施；P5.1/P5.2 待决策） |
| M6 | 职责重叠 | 跨 compaction 的上下文保全有三套并存机制：本地 `PrepareCompactionRescue`、hindsight 的 `<memory_rescue>` 内联块（coordinator.go:504-513 手工拼字符串）、compaction 后的 working memory 生成。三者目的相同、入口分散在两层 | `internal/agent/coordinator.go:490-538`、`internal/agent/session_memory.go` | ✅ 已解决（Phase 2：`BeforeCompaction` 单路径委托） |
| M7 | 优先级倒置 | 研究级特性齐全（知识图谱三元组、veracity 冲突检测、Weibull 三票检索、embedding rerank、proactive linking），但**用户面为零**：Commands 面板没有任何 memory 命令，用户无法查看/搜索/清空/触发合并自己的记忆库，唯一入口是给 LLM 用的 `memory_status` 工具。oh-my-pi 的 `/memory` 六个子命令全是给人的 | `internal/ui/dialog/commands.go`（无 memory 项） | ✅ 已解决（Phase 4） |
| M8 | 一致性 | `agent.go:3089`、`agent_memory.go:63` 存在中文注释，与代码库注释语言不一致 | `internal/agent/agent.go:3089` | ✅ 已解决（Phase 1） |

原结论：**功能是超集，工程是欠账**。crush 把 oh-my-pi 三个 backend 的能力
（local 管道 + hindsight + mnemopi 式结构化存储）压进了一个单体 Engine，
代价是：backend 抽象缺失（M2）、工具面失控（M3/M4）、运行期成本失衡（M5）、
死配置（M1）、用户面缺席（M7）。

当前状态：M1–M4、M6–M8 全部解决；M5 部分解决，剩余 P5.1/P5.2 待决策。

---

## Phase 1 — 死代码与一致性 ✅ 已实施

1. ~~删除 `sessionState.firstTurnInjected`、`pendingWrites`（含 engine.go:529
   的写入）；删除 `ShouldUpdateWorkingMemory`、`workingMemoryThrottle` 字段、
   `Config.WorkingMemoryThrottle` 及 `New()` 中的默认值逻辑
   （engine.go:110-113）。若 grep 发现 schema.json 暴露了该配置，一并移除。~~
   已完成：全部六个符号从源码消失，`sessionState` 现为空结构体。
2. ~~`internal/agent/agent.go:3089`、`agent_memory.go:63` 中文注释翻译为英文。~~
   已完成：两处均改为英文注释，`agent_memory.go` 全文无中文。
3. 验收：`go build ./... && go test ./internal/memory/... ./internal/agent/...`；
   全库 grep 确认三个符号零残留。✅ 通过。

> **备注**：`agent.go:1052/1058/1134` 仍有中文，但那是英文注释中引用的
> 中文用户输入示例（如"继续"、"莫名其妙中断"触发去重逻辑的样例），不属于
> 本项清理目标。

## Phase 2 — Backend 接口化 ✅ 已实施

新增 `internal/memory/backend.go`：

```go
type Backend interface {
    ID() string                      // "local" | "hindsight" | "off"
    Retriever() engine.Retriever     // recall/reflect/auto-recall 数据源
    // AfterTurn 是回合结束后的唯一入口；实现自行决定抽取/留存/节流。
    AfterTurn(ctx context.Context, sessionID string, transcript TranscriptView)
    // BeforeCompaction 返回要注入 summary prompt 的 rescue 文本（可空）。
    BeforeCompaction(ctx context.Context, sessionID string) string
    OnSessionDeleted(ctx context.Context, sessionID string) error
    Capabilities() Capabilities      // {Triples, Reflect, Retain bool}
    Status(ctx context.Context) (*Status, error)
    Close() error
}
```

> **实际实现**：接口比上述草图更丰富，额外包含 `Enabled()`、`EventStore()`、
> `TripleStore()`、`TranscriptRetainer()`、`IsDegraded()`、`DegradedReason()`、
> `OnSessionCreated()`、`TriggerConsolidation()`、`TriggerMaterialization()`、
> `Clear()`。`Capabilities` 结构也增加了 `BroadRecallFallback`、
> `TruncateRecallQuery`、`MentalModels`、`SessionWorkingMemory`、
> `RemoteConsolidation`。`AfterTurn` 签名简化为不带 `TranscriptView` 参数。

1. ✅ `localBackend`（`internal/memory/local_backend.go`）：包装现有 engine
   管道。
2. ✅ `hindsightBackend`（`internal/memory/hindsight_backend.go`）：包装 client
   + TranscriptRetainer + Retriever + mental models 加载；`BeforeCompaction`
   内迁了原 coordinator 手工拼 `<memory_rescue>` 的逻辑。
3. ✅ 工厂 `memory.Resolve(memCfg *config.MemoryConfig, deps Deps) Backend`
   （`internal/memory/resolve.go:29`）替换 app.go 的整段内联组装。app.go 现在只
   剩一次 `memory.Resolve(...)` 调用。
4. ✅ coordinator 全面去分支：`SetMemoryEngine` 改为 `SetMemoryBackend`，用
   类型 switch 做一次性 wiring（不再字符串比较）；`memoryEngineHooks` 单路径
   委托 `Backend.BeforeCompaction`；prefetch 改 `caps.MentalModels` 门控；
   `buildAutoRecallBlock` 的 `backend string` 参数改为 `caps memory.Capabilities`。
5. ✅ `engine.Engine` 保留为 localBackend 的内部实现，coordinator 不再直接
   引用。

验收：coordinator/agent 包内 grep `"hindsight"` 字面量零命中（仅注释中以
散文形式出现）；行为不变（现有 memory 相关测试全绿）。✅ 通过。

## Phase 3 — 工具面：门控 + 合并 ✅ 已实施

1. ✅ **按能力注册**：`memoryTools()` 方法（tool_registration.go）替代原
   无条件注册块。memory backend 为 off/nil 时零工具；`reflect` 仅当
   `Capabilities().Reflect`；`graph` 仅当 `Capabilities().Triples`。
2. ✅ **合并图谱工具**：`graph_query` 与 `triple_query` 合并为单个 `graph`
   工具（`internal/agent/tools/graph.go`，参数 `mode: "path" | "triples"`）。
   `triple_query` 工具定义与 .md 描述已删除。
3. ✅ **memory_status 精简**：LLM 面的 `memory_status` 精简为一行状态；
   诊断细节移到 Phase 4 的用户命令。
4. ✅ **retain 免弹窗**：`NewRetainTool` 无 `permission.Service` 依赖，
   handler 无 `RequestPermission` 调用。`retain.go` 与 `retain.md` 均有显式
   注释说明设计理由（写入 crush 自己的数据目录，对齐 oh-my-pi
   `approval: "read"`）。secrets 过滤规则保留在工具描述中。

验收：memory off 配置下启动，工具列表中无任何 memory 工具；hindsight 配置下
无 graph 工具。✅ 通过。

## Phase 4 — 用户面补齐 ✅ 已实施

在 Commands 面板（`internal/ui/dialog/commands.go`）新增 Memory 分组
（由 `MemoryBackend != nil` 门控）：

| 命令 | 行为 |
|------|------|
| Memory: Status | 渲染 `Backend.Status()`（backend、事件数、上次合并时间、degraded 原因） |
| Memory: Search | 输入 query → `Retriever.Retrieve`，结果只读展示 |
| Memory: Consolidate Now | `TriggerConsolidation` + `TriggerMaterialization`（对齐 `/memory enqueue`） |
| Memory: Clear | 二次确认后清空当前 scope 的事件与物化产物（`Backend.Clear()`；hindsight 仅清本地缓存并提示远端需自行管理） |

验收：四个命令在 local backend 下全部可走通；Clear 有确认对话框。✅ 通过。

## Phase 5 — 运行期成本收敛（行为变更，需要决策确认）

目标：把"每回合多次 LLM 调用"收敛为"回合内零 LLM、后台批处理"。

1. `AfterTurnIdle` 改为**只追加原始转写事件**（带 source_hash 去重），
   不再每回合调用抽取 LLM。**⏳ 待决策，未实施。**
2. 抽取（extraction）合并进后台物化 pass：由现有
   `BackgroundInterval` / `BackgroundEveryNTurns` 触发，一次 pass 内串行完成
   抽取 → 合并 → 物化 → embedding，**全局并发上限 1**（复用
   `acquireConsolidationLease` 的租约表，扩展为通用 job lease，对齐
   oh-my-pi `memories/storage.ts` 的 claim/heartbeat 模式）。**⏳ 待决策，未实施。**
3. proactive linker 与 embedding pipeline 的独立 goroutine 触发点移入同一
   pass，删除 `go e.proactiveLinker.LinkEvents(...)`（engine.go:519）这类
   fire-and-forget。**✅ 已实施。**
4. compaction 保全三合一：`BeforeCompaction` 成为唯一入口（Phase 2 已并掉
   hindsight 路径）；compaction 后的 working memory 生成
   （`asyncUpdateSessionMemory`）保留但降频——仅当本次 compaction 丢弃的
   token 超阈值时触发（阈值配置化，默认保守）。**✅ 已实施。**
5. 每回合 prefetch auto-recall 保留（这是对时延有正贡献的路径），但
   reranker 在 auto-recall 中默认关闭（仅显式 recall 工具调用时启用）。
   **✅ 已实施。**

决策点（实施前向用户确认）：抽取从"每回合"改为"后台批处理"会让 `recall`
在回合刚结束的短窗口内查不到最新事实。缓解：合并 pass 的 EveryNTurns 默认
从当前值收紧为 2-3。

验收（已实施部分）：单回合日志中 proactive linker 不再有独立 goroutine；
compaction 后 working memory 仅在丢弃 token ≥ 阈值时生成；auto-recall
prefetch 跳过 reranker。✅ 通过。
验收（待实施部分）：单回合日志中 memory 相关 LLM 调用数为 0（不含用户显式
recall）；后台 pass 期间并发 LLM 调用 ≤1；关闭再启动后 job 租约可被回收。

### 实施状态（2026-07 code review 跟进）

- **P5.1 / P5.2（每回合抽取改后台批处理）：待决策，未实施。** 涉及行为变更
  （回合刚结束的短窗口内 `recall` 查不到最新抽取事实），按本文档要求需先
  经用户确认再动手。
- **P5.3（proactive linker / embedding 的 fire-and-forget goroutine）：已实施。**
  `engine.AfterTurnIdle` 与 `TriggerConsolidation` 不再 `go
  e.proactiveLinker.LinkEvents(...)`；改为 `enqueuePendingLinks` 入队，由
  `TriggerMaterialization`（现有物化 pass 的唯一入口，per-turn 触发、
  turn-counter 触发、compaction 前、session 删除等场景都会调用它）在开头
  `drainPendingLinks` 中串行执行。回归测试
  `TestEngine_AfterTurnIdleLinksSynchronouslyWithoutGoroutine`。
  embedding pipeline 的 `Enqueue` 本身已经是单飞 worker（`p.running` 门控），
  未发现需要收敛的额外散养触发点。
- **P5.4（compaction 后 working memory 降频）：已实施。** 新增
  `MemoryConfig.WorkingMemoryMinDiscardedTokens`（默认 20000，
  `GetWorkingMemoryMinDiscardedTokens()`），`sessionAgent.Summarize` 计算
  `discardedTokens = 压缩前估算 tokens - 压缩后估算 tokens`，仅当
  `discardedTokens >= 阈值` 且 `enableSessionMemory()`（已受
  `Capabilities.SessionWorkingMemory` 门控）同时成立时才触发
  `asyncUpdateSessionMemory`。回归测试
  `TestSummarizeTriggersWorkingMemoryGeneration`。
- **P5.5（auto-recall 默认关 reranker）：已实施。** `SummaryRetriever.Retrieve`
  的 `opts` 新增 `"rerank"` 布尔开关（默认 `true`，未知/缺失键视为启用，
  Hindsight 的 Retriever 忽略该键不受影响）；`coordinator.buildAutoRecallBlock`
  的每回合 prefetch 路径显式传 `"rerank": false`，跳过向量语义 voice 与最终
  heuristic rerank；`internal/agent/tools/recall.go` 的显式 `recall` 工具调用
  显式传 `"rerank": true`（等价于默认值，用于文档化两条路径的差异）。回归
  测试 `TestSummaryRetrieverRetrieveRerankOptGatesEmbeddingVoice`。

## Phase 6 — 记忆概念仲裁 ✅ 已实施

在 `coder.md.tpl` 的 `<memory_instructions>`（266–274 行）与 `retain.md`
（23–27 行）中写清分工，消除 M4 的三头写入：

- **CRUSH.md 记忆文件**：项目级、需要进 git、给人看的约定（构建/测试命令、
  代码风格）。
- **retain 工具**：跨会话、不适合进仓库的知识（用户偏好、环境特例、
  历史决策及其原因）。
- **自动抽取**：兜底，不作为指望；prompt 中明确"已 retain 的内容不要
  重复写入记忆文件，反之亦然"。

两处均包含"同一事实不要重复写入"的反重复指令。✅ 通过。

## 实施顺序与状态

```
Phase 1（独立）                     ✅ 已完成
Phase 2（核心）─→ Phase 3 ─→ Phase 4  ✅ 全部完成
Phase 5（依赖 2；含决策点）           ⏳ P5.3/P5.4/P5.5 已完成；P5.1/P5.2 待决策
Phase 6（文案，随 3 合入）            ✅ 已完成
```

建议 PR 粒度（供参考，已完成的不再适用）：P5 剩余两个（AfterTurn 瘦身 /
job 队列统一）各一个 PR。

## 全局验收

1. `go build ./...`、`go test ./internal/...` 通过。
2. 三种配置各跑一遍冒烟：`backend=off`（无 memory 工具、零后台 goroutine）、
   `backend=local`（retain→recall 闭环、Commands 面板四命令、后台 pass 单飞）、
   `backend=hindsight`（transcript 留存、compaction rescue 注入、无 graph 工具）。
3. coordinator/agent 包 grep 不到 `"hindsight"` 字面量。
4. ~~单回合（非 compaction）memory 相关 LLM 调用数为 0。~~
   待 P5.1/P5.2 实施后验证。
