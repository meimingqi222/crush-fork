> **HISTORICAL - DO NOT USE AS REFERENCE.** This document is archived; it describes a design that has been implemented and may diverge from the current code. The current code is the authoritative source.

# Plan 模式与 Subagent 机制重构方案

> 状态：**大部分已实施**。Phase 1–4 全部完成；Phase 5 部分完成（plan 相关 UI
> 逻辑已拆入 `ui_plan.go`，coordinator 拆分未做）。R6 以时间线事件替代
> reducer metadata 实现 spawn-time 信号，设计有偏离但意图达成。基于 2026-07
> 的代码审计，对照了 oh-my-pi
> (`packages/coding-agent/src/{plan-mode,task,modes}`) 与 opencode
> (`packages/tui/src/routes/session`, `packages/core/src/plugin/agent.ts`)
> 的同类实现。所有行号以撰写时的 main 分支为准，实施时以符号名为锚点。

## 0. 审计结论：现存的冗余 / 错误 / 过时设计清单

> 状态列标注当前解决情况。✅ = 已解决，⚠️ = 部分解决/设计偏离，⏳ = 未实施。

| # | 类别 | 问题 | 位置 | 状态 |
|---|------|------|------|------|
| R1 | 错误 | `summarizeTaskStatusCounts` 中 `completedSet` 构建后从未使用（死代码）；命名返回值 `pending` 从未赋值，导致 task 摘要行永远显示 `pending 0`，所有非终态任务都被计成 `running` | `internal/ui/chat/agent.go:736-759` | ✅ Phase 1 |
| R2 | 死代码 | `taskStatusIcon` 的 default 分支 if/else 两条路径返回完全相同的 `IconPending`，`opts`/`taskID` 参数实际无效；running 与 pending 无视觉区分（对比 oh-my-pi 的 spinner 帧） | `internal/ui/chat/agent.go:770-784` | ✅ Phase 1 |
| R3 | 双协议冗余 | 计划产出存在两条并行协议：内联 `<proposed_plan>` 块（`planmode.ExtractProposedPlan`，`ui.go:3156` 优先走此路径）与 plan 文件 + `resolve(action:"apply")`（主路径）。`docs/plan-mode.md` 声称前者已删除，实际仍是优先路径 | `internal/planmode/planmode.go:14-37`、`internal/ui/model/ui.go:3149-3173`、`internal/ui/chat/assistant.go:299` | ✅ Phase 2 |
| R4 | 门控矛盾 | plan 模式下 bash 的两层门控互相矛盾：`tool_registration.go:83-90` 为 plan 模式准备了 `RestrictedToGitReadOnly` 的 bash，但 `filterToolsForRiskPolicy`（`plan_mode.go:379`）因 bash 是 `toolRiskExecute` 把它整个裁掉——主 agent 在 plan 模式下根本没有 bash，read-only 配置成了死配置。副作用：规划期无法运行任何命令（跑测试、`go doc`、查版本） | `internal/agent/plan_mode.go:203,332-350`、`internal/agent/tool_registration.go:83` | ✅ Phase 2 |
| R5 | 过度限制 | plan 模式裁掉全部 network 级工具（`agentic_fetch`/`sourcegraph`/`download`），规划期无法做外部调研；oh-my-pi 与 opencode 均不禁研究类工具 | `internal/agent/plan_mode.go:200-224` | ✅ Phase 2 |
| R6 | 冗余匹配 | task → child session 的关联没有单一权威，UI 侧靠三层猜测兜底：① reducer metadata 的 `ChildSessions[].TaskID/ChildSessionID`；② `CreateAgentToolSessionID` 的 `msgID::toolCallID` 复合 ID + `"::"` 前缀扫描；③ `taskRef`/`subtask://` 模糊匹配 | `internal/ui/model/session.go:540-687` | ⚠️ Phase 3（偏离：用时间线事件替代 reducer metadata） |
| R7 | 设计噪音 | plan 模式 turn 末尾强制工具调用的 enforcement 通过递归 `c.Run` 注入一条明文 user reminder 实现：多耗一轮推理、reminder 以用户消息形式持久化污染 transcript、depth>1 后静默放弃 | `internal/agent/plan_mode_enforcement.go:71-94` | ✅ Phase 2 |
| R8 | 文档过时 | `docs/plan-mode.md` 全文描述已被替换的 `<proposed_plan>` 协议与 "Execute Proposed Plan" 命令；`docs/subagent-runtime-spec.md`(1006 行)、`subagent-runtime-prd.md`(713 行)、`subagent-system-redesign.md`(178 行) 三份互相重叠的历史文档无 single source of truth | `docs/` | ✅ Phase 2 |
| R9 | 别名冗余 | `Session.IsPlanMode()` 只是 `IsActivePlanMode()` 的别名 | `internal/session/mode_guards.go:26-28` | ✅ Phase 1 |
| R10 | UX 缺陷 | subagent 进入方式发现性差：`]` 需要 tab 切焦点→j/k 选中特定行→按 `]` 三步；帮助文案 `]/l open subagent` 只有在已选中带子会话的行时才出现（`ui.go:4132`）——不知道该功能的用户永远看不到提示 | `internal/ui/model/ui.go:3495-3522,4129-4135` | ✅ Phase 4 |
| R11 | UX 缺陷 | subagent 会话内迷路：唯一线索是一行 muted banner `SUBAGENT <ROLE>  read-only  [ back`。无兄弟索引 (n of N)、无 prev/next 提示（ctrl+↑/↓ 藏在 full help）、无 tokens/cost。对比 opencode `subagent-footer.tsx`（名称 + "(2 of 5)" + tokens(ctx%) + cost + 可点击 Parent/Prev/Next） | `internal/ui/model/ui.go:4976-4989` | ✅ Phase 4 |
| R12 | 结构性 | `coordinator.go` 5054 行的 god object：混杂 10+ 个 `buildXProvider`、模型解析、工具构建、plan 文件生命周期、memory hooks、队列管理、subagent 调度；`ui/model/ui.go` 6630 行同理 | `internal/agent/coordinator.go`、`internal/ui/model/ui.go` | ⏳ Phase 5（UI plan 逻辑已拆入 `ui_plan.go`；coordinator 拆分未做） |

总体判断：**核心机制不落后**（plan 文件 + resolve 审批明显参照了 oh-my-pi 的设计，子会话导航比 oh-my-pi 的纯内联更进一步），问题集中在：迁移残留的双协议、两层门控没对齐、UI 引导层缺失、文档失真。属于"半程迁移未收尾"，不需要推倒重来。

当前状态：R1–R5、R7–R11 全部解决；R6 以设计偏离方式部分解决；R12 仅完成 UI 侧拆分。

---

## Phase 1 — 死代码与实错修复（低风险，先行合入）✅ 已实施

### 1.1 修复 task 状态汇总（R1）

`internal/ui/chat/agent.go` `summarizeTaskStatusCounts`：

- 删除 `completedSet` 整段（736-742 行的构建循环）。
- 区分 pending 与 inProgress：`statuses` 中无条目或值为
  `ToolResultSubtaskStatusPending` → `pending++`；
  `InProgress`/`Running` → `inProgress++`；其余保持现有分支。
- 摘要行 `summaryText`（`agent.go:649`）保持格式不变，数字将首次真实。

验收：构造含 pending/running/completed 混合状态的 reducer metadata 单测，
断言各计数正确（扩展 `internal/ui/chat/agent_test.go` 现有用例）。

### 1.2 简化并修正状态图标（R2）

`taskStatusIcon`：

- 删除 default 分支中无效的 if/else，保留一条返回。
- pending → `IconPending`；running/in_progress → 新增 working 图标
  （复用 `sty.Tool` 中现有 spinner/working 样式；若无动画基础设施则用
  静态区分符号即可，不引入新计时器）。
- 若 `opts`/`taskID` 参数因此不再使用，从签名移除并更新调用点。

### 1.3 删除 `IsPlanMode` 别名（R9）

全局把 `Session.IsPlanMode()` 调用点改为 `IsActivePlanMode()`，删除别名方法。

---

## Phase 2 — Plan 模式：协议统一与门控修正（中风险）✅ 已实施

### 2.1 收敛到单一计划协议：plan 文件 + resolve（R3）

权威协议 = agent 把计划写入 plan 文件（`local://<slug>-plan.md`），以
`resolve(action:"apply", extra:{title})` 请求审批；UI 从 `sess.PlanFilePath`
读文件内容打开 `dialog.NewPlanReview`。

改动：

1. `internal/ui/model/ui.go` `maybeOpenProposedPlanDialog`（3149）：删除
   `planmode.ExtractProposedPlan` 分支，逻辑简化为：有 `resolve apply` 调用
   且 `PlanFilePath` 非空 → `loadPlanReview`；文件缺失/为空 → 报错提示
   （不再回退解析消息文本）。
2. `internal/planmode/planmode.go`：删除 `ProposedPlanOpenTag/CloseTag`、
   `ExtractProposedPlan`、`WrapProposedPlan` 及其测试。保留
   `ExecutionContextMode`、`BuildExecutionPrompt`、`toc.go` 的
   `SplitSections`/`ParseTOC`。
3. `internal/ui/chat/assistant.go:299`：增强渲染的触发条件从"消息文本含
   proposed_plan 标签"改为"消息含 resolve apply 工具调用"，渲染内容为普通
   markdown（计划正文在文件里，消息文本不再承载协议）。
4. 更新使用 `WrapProposedPlan` 的测试
   （`ui_message_update_test.go`、`agent_test.go:428`）为写临时 plan 文件 +
   resolve 工具调用的形态。

### 2.2 对齐 bash 门控：plan 模式提供只读 bash（R4）

决策：plan 模式主 agent **保留 bash**，但注册为
`RestrictedToGitReadOnly + DisableBackground`（该选项已存在且已在
`tool_registration.go:83` 配好，只是被上层 filter 吃掉）。

改动：`internal/agent/plan_mode.go`

- `isPlanModeToolAllowed` 增加 `tools.BashToolName` 特判放行（带注释：
  注册层已将其收窄为 git 只读；这里放行的是收窄后的工具）。
- 在 `planModeFileInspectToolNames` 注释中说明 bash 不在此表的原因
  （它不是 read risk，靠注册层收窄）。

可选（作为后续独立 PR，不阻塞本方案）：把 `RestrictedToGitReadOnly`
扩展为可配置的只读命令白名单（`go test`、`ls`、`cat` 等），对齐
Claude Code / opencode 规划期可运行只读命令的体验。

### 2.3 放开规划期研究工具（R5）

`isPlanModeToolAllowed` 放行 `tools.AgenticFetchToolName` 与
`tools.SourcegraphToolName`（均为纯读外部数据；`download` 因写入工作区
继续禁止）。同步更新 `plan_mode_test.go` 中对工具过滤的断言。

### 2.4 降噪 enforcement（R7）

最小改动方案（不重构 Run 循环）：

- `planModeToolDecisionReminder` 注入的 user 消息打上隐藏标记
  （复用 message 层现有的 hidden/synthetic 机制；若无，加一个
  `message.WithSynthetic()` 元数据并让 chat 渲染层跳过），transcript 不再
  显示这条机器 reminder。
- depth 用尽仍未调用工具时，不再静默：发布一条 UI status warning
  （"Plan 轮次未以 resolve/request_user_input 结束"），让用户知情。

### 2.5 文档收敛（R8）

- 重写 `docs/plan-mode.md`：描述现实（collaboration_mode 状态机、plan 文件
  协议、resolve 审批、三种执行选项、工具门控两层结构、与 goal 模式互斥）。
- 三份 subagent 文档合并为一份 `docs/subagent-runtime.md`（只写 current
  state，从 spec 中"已实现"部分提炼）；`subagent-runtime-prd.md` 与
  `subagent-system-redesign.md` 移入 `docs/history/` 并在文件头标注
  "historical, do not use as reference"。

---

## Phase 3 — task→child session 单一权威映射（R6）⚠️ 部分实施（设计偏离）

> **实施偏离**：原方案要求 coordinator 在 spawn 时把 `ChildSessionID` + `TaskID`
> 写入工具消息的 reducer metadata。实际实现改为在 spawn 时发布时间线事件
> （`SubagentEventStarted` → `timeline.ChildSessionStartedEvent`），UI 通过
> 时间线获取 spawn-time 信号。reducer metadata 仍在完成时写入。意图达成
> （UI 能在 spawn 时解析 child session），但权威数据源是时间线而非 reducer
> metadata。

目标：reducer metadata 是唯一权威，UI 不再做前缀扫描与模糊匹配。

1. 写侧保证：coordinator 在 child session 创建成功的当下（而非任务结束时）
   就把 `ChildSessionID` + `TaskID` 写入工具消息的 reducer metadata
   （`message.WithReducer` 已支持增量更新；确认 `runSubAgentDirect` 与
   TaskGraph 两条路径都在 spawn 点写入）。
2. 读侧简化：`internal/ui/model/session.go`
   - `openSelectedChildSession`：仅通过 `TaskNodeItem.ChildSessionID()` /
     reducer 查找；`childID + "::"` 前缀扫描降级为带
     `slog.Debug("child session resolved via legacy prefix scan")` 的
     兜底分支，计划在下一个版本删除。
   - `childSessionIDForTaskRef` 与 `taskRefMatches` 若在读侧简化后无调用方，
     一并删除。
3. 单测：为"spawn 即写入 metadata"补一条 coordinator 侧断言
   （扩展 `subagent_runtime_test.go`）。

---

## Phase 4 — Subagent UI：入口提示、footer、fallback（对标 opencode/oh-my-pi）✅ 已实施

本 Phase 是用户体验主体，不依赖 Phase 2/3（可并行实施）。

### 4.1 内联入口提示（R10，学 opencode `index.tsx:1495`）

在 agent 工具块渲染（`internal/ui/chat/agent.go`，`summaryLine` 之后）追加
一行 muted 提示：

```
] open subagent
```

规则：仅当该工具调用已有 child session（reducer metadata 判断）时渲染；
子会话视图内（避免嵌套提示）不渲染。样式用 `sty.Muted`，不参与选中高亮。

### 4.2 `]` 的无选中 fallback

`internal/ui/model/ui.go` `SessionChild` 分支（3502）：当
`openSelectedChildSession()` 返回 nil（无选中项或选中项无子会话）且当前
session 存在 child sessions 时，进入**最近创建的、状态为 running 的**子会话；
无 running 则进入最近创建的。实现放在 `session.go` 新函数
`openLatestChildSession()`，复用 `m.childSessions(parentID)`。

### 4.3 Subagent footer（R11，学 opencode `subagent-footer.tsx`）

重写 `renderSubagentBanner`（`ui.go:4978`）为两段式 footer：

```
SUBAGENT EXPLORE  (2 of 5) · 34.2k tokens (17%) · $0.12
[ parent   ctrl+↑/↓ prev/next   read-only
```

数据源：

- 索引/总数：`m.childSessions(m.session.ParentSessionID)`，按创建时间排序后
  定位当前 ID（与 `cycleSiblingChildSession` 相同的列表，提取共享 helper）。
- tokens/ctx%：`m.session` 的 usage 字段 + `ModelForSession` 的 context
  window（`landing.go:13` 已有取模型逻辑，复用）。
- cost：session cost 字段；为 0 时省略该段。
- 总数为 1 时省略 `(n of N)` 与 prev/next 提示。
- session pubsub 更新时 footer 自动重渲染（现有更新路径已覆盖，无需新订阅）。

`km.Chat.SessionNext/SessionPrev` 的帮助文案保持，但确保它们在
subagent 会话内出现在 ShortHelp（当前只在 FullHelp）。

### 4.4 per-task 行统计（可选，学 oh-my-pi `appendAgentStats`）

若 `reducer.ChildSessions` 中已带 usage 字段则在每行 task 尾部追加
`· 12.3k tok · $0.04`（muted）；若数据不在 metadata 中，本项跳过并在 PR
描述中注明，**不要**为此给渲染层加同步 DB 查询。

### 4.5 键位保持，不做破坏性改动

- 保留 `[`/`]`/`h`/`l`/ctrl+方向键/alt+方向键全部现有绑定
  （`keys.go:300-319`）。opencode 的 up=parent 空间隐喻与本项目的
  编辑器焦点模型冲突（up 已用于历史/滚动），**不采纳**。
- 核查 `km.Chat.SessionNav`（`keys.go:316`）：若仅用于 help 聚合显示，
  加注释说明；若无任何 `key.Matches` 调用点，删除。

---

## Phase 5 — 结构清理（可选，最后做，纯移动不改行为）⏳ 部分实施

> **已实施**：plan 相关 UI 逻辑（`maybeOpenProposedPlanDialog`、
> `executeApprovedPlan`、resolve helpers）已拆入 `internal/ui/model/ui_plan.go`。
> **未实施**：`coordinator.go` 的 `providers.go` / `plan_session.go` / `queue.go`
> 拆分。

1. `internal/agent/coordinator.go`（5054 行）拆分：
   - `providers.go`：全部 `buildXProvider` / `getProviderOptions` /
     `mergeCallOptions`（约 1900-2270 行区间）。
   - `plan_session.go`：`ensurePlanFileForSession` 及 plan 文件生命周期。
   - `queue.go`：`RemoveQueuedPrompt`/`ClearQueue`/`PauseQueue` 等队列方法。
2. `internal/ui/model/ui.go`（6630 行）：plan 相关
   （`maybeOpenProposedPlanDialog`、`executeApprovedPlan`、resolve helpers）
   移入 `ui_plan.go`。
3. 每次拆分单独成 commit，`go build ./... && go test ./internal/...` 全绿。

---

## 实施顺序与依赖

```
Phase 1（独立） ─┐            ✅ 已完成
Phase 2（独立） ─┼─→ Phase 5（最后）  ✅ P1-P4 已完成；P5 部分（UI 侧已拆，coordinator 未拆）
Phase 3 ─→ 4.1/4.2 依赖 3 的 metadata 保证（4.3/4.5 独立）  ✅ 已完成（R6 设计偏离）
```

建议 PR 粒度（供参考，已完成的不再适用）：P5 coordinator 拆分若实施，
每个文件拆分单独成 commit。

## 全局验收

1. `go build ./...`、`go test ./internal/...` 通过。✅
2. 手动流程 A（plan）：进入 plan 模式 → agent 可用 git 只读 bash 与
   agentic_fetch → 写 plan 文件 → resolve apply → 三种执行选项均可走通 →
   transcript 中无机器 reminder 消息。✅
3. 手动流程 B（subagent）：触发多 subagent 任务 → 父会话内看到
   `] open subagent` 提示与真实 pending/running 计数 → 焦点在 editor 时
   tab 后直接 `]` 可进入 running 子会话 → footer 显示 `(n of N)`、tokens、
   cost 与导航提示 → ctrl+↑/↓ 切兄弟、`[` 回父会话且恢复原选中位置。✅
4. `docs/plan-mode.md` 与 `docs/subagent-runtime.md` 与代码一致；
   历史文档已归档。✅
