# Subagent 续用与生命周期统一重构设计

> 目标读者：负责实施的 agent。本文档给出现状分析（含行号锚点）、目标设计、
> 分阶段实施步骤、测试要求与代码清理清单。实施前请先通读
> `docs/subagent-runtime.md`（当前实现的权威描述）与
> `docs/pitfalls/fantasy-dual-message-state.md`。
>
> 行号基于编写本文时的工作区状态，实施时以符号名搜索为准。

## 1. 动机

对标 Claude Code 与 oh-my-pi 的 subagent 续用体验：

- **Claude Code**：子代理完成后 transcript 保留，主代理随时可用
  `SendMessage` 指名追加消息，harness 自动从 transcript 复活它继续对话，
  上下文不丢。
- **oh-my-pi**（`packages/coding-agent/src/registry/agent-lifecycle.ts`、
  `task/persisted-revive.ts`）：显式 `running → idle → parked` 状态机，
  park 只是降级不是删除；给 parked 代理发 DM 自动复活（`ensureLive`）；
  甚至支持跨进程冷复活（JSONL + 持久化 `session_init` 运行时契约）。

crush 的零件都在（SQLite 会话、lifecycle manager、命令队列），但没有接成
一个系统，并且核心复活路径是**不可达的死代码**（见 §2.3）。

## 2. 现状分析（已核实）

### 2.1 两套注册表、三个消息面

| 组件 | 文件 | 职责 | 问题 |
|---|---|---|---|
| `AgentRegistry` | `internal/agent/agent_registry.go` | 全局身份/状态表（`running/idle/completed/aborted`），IRC roster 来源 | 无 `parked` 状态；`ListVisibleTo` 只显示 running/idle |
| `backgroundAgentRegistry` | `internal/agent/background_agent.go` | 后台代理：名字→ID 解析、每代理命令队列 + `processQueuedCommands` 常驻 goroutine | 与 `AgentRegistry` 身份信息重复；纯内存 |
| `subagentLifecycleManager` | `internal/agent/subagent_lifecycle.go` | 前台子代理 5 分钟 warm-revive 窗口（`Adopt`/`Revoke`/`Park`） | `Park` 直接 `registry.Unregister` → 代理失联 |
| `agent` 工具 | `internal/agent/agent_tool.go` | 派生子代理（单个/批量/后台） | 只能新建，无续用入口 |
| `send_message` 工具 | `internal/agent/tools/send_message.go` | `agent_id` → 后台代理追加消息；`mailbox_id` → 批量兄弟通信 | 前台子代理不可寻址 |
| `irc` 工具 | `internal/agent/tools/irc.go` | 代理间即时消息 | 第 100 行只投递 `running/idle`；parked（已被 Unregister）不可达 |

### 2.2 后台代理：续用可用，但仅限内存

`backgroundAgentRegistry.Enqueue`（`background_agent.go:264`）向常驻队列
投递 follow-up，`processQueuedCommands`（`:324`）把 completed 代理重新拉起
（`markRunning` → runner）。`Complete` 不关闭 runner，所以**后台代理的
续用已经工作**。短板：

- `Stop()`（`:359`）把 runner/commands 置 nil 后**永久不可续用**；
- 注册表纯内存，进程重启全丢（SQLite 里的会话历史无从寻址）；
- 与 `AgentRegistry` 各存一份名字/状态，语义不同步。

### 2.3 前台子代理：复活路径是死代码

这是本次重构最重要的事实：

1. `subAgentParams.ExistingSessionID`（`coordinator.go:1884`）在
   `runSubAgentDirect` 中有完整的消费逻辑——非空时从 SQLite 取回子会话
   （`coordinator.go:2887-2892`）并跳过 handoff 前缀（`:3047`）——**但全
   仓库没有任何赋值点**（仅声明与读取）。
2. 批量收尾（`coordinator.go:2260-2286`）成功路径把注册表状态设为 `Idle`
   并 `lifecycle.Adopt(childSessionID, agentID, 5min)`，注释宣称
   "a follow-up agent tool call with ExistingSessionID can reuse"——但
   `AgentParams`（`agent_tool.go:33-42`）没有向 LLM 暴露任何续用参数，
   warm revive 同样不可达。
3. TTL 到期 `Park`（`subagent_lifecycle.go:99-120`）：
   `childSessionAgents.Delete` + `registry.Unregister` → 代理从 IRC
   roster 和一切寻址面**消失**，只剩 SQLite 里无人引用的会话历史。

结论：前台子代理目前是"一次性"的；warm/cold revive 基建存在但从未通电。

## 3. 目标设计

### 3.1 设计原则

- **一个身份源**：`AgentRegistry` 是身份与状态的唯一权威；
  `backgroundAgentRegistry` 退化为执行引擎（队列/派发），不再自持身份。
- **park 是状态不是删除**：新增 `AgentStatusParked`；parked 代理保留
  注册表条目（释放内存中的 `SessionAgent`），可被消息自动复活。
- **续用走消息，不走 agent 工具**：与 Claude Code/oh-my-pi 一致，
  `agent` 工具保持 spawn-only；follow-up 统一经 `send_message`（及 irc）。
  `ExistingSessionID` 仍然是内部字段，由复活路径填充。
- **复活分级**：warm（TTL 内，内存 `SessionAgent` 直接复用）→ cold
  （TTL 外，从 SQLite 重建）。跨进程复活作为可选的最后阶段。

### 3.2 状态机

```
                    agent 工具 spawn
                          │
                          ▼
                      running ──失败/取消──► aborted（保留条目，只读，不可续用）
                          │
                       完成
                          ▼
                romp    idle（warm：SessionAgent 在内存，TTL 5min）
                          │                    ▲
                       TTL 到期                │ 消息到达（warm revive）
                          ▼                    │
                       parked ──消息到达───────┘
                     （cold：仅 SQLite 历史 + 注册表条目）
                          │ 消息到达（cold revive：重建 SessionAgent → running）
                          ▼
                       running
```

- `AgentStatusCompleted` 不再作为终态使用（见清理清单 C6）；成功即
  `idle`，失败/取消为 `aborted`。
- 父会话销毁时（`RemoveForSession` 语义）parked/aborted 条目一并清除
  （阶段 3 引入持久化后改为可选保留）。

### 3.3 寻址与投递（统一入口）

`send_message` 的 `agent_id` 解析顺序改为：

1. `backgroundAgentRegistry.ResolveAddress`（兼容既有后台代理，不变）；
2. 未命中 → `AgentRegistry` 按 ID 精确匹配，再按 `DisplayName` 唯一匹配
   （重名且歧义时返回错误列出候选）；
3. 命中后按状态派发：
   - `running`：投递到子 `SessionAgent` 的既有 prompt 队列
     （`SessionAgent.QueuePrompt`，crush 已实现排队机制，直接复用）；
   - `idle`：warm revive——从 `childSessionAgents` 取出内存实例，构造
     `subAgentParams{ExistingSessionID: ref.SessionID, ...}` 走
     `runSubAgentDirect`；
   - `parked`：cold revive——同上，但 `params.Agent` 由 coordinator 按
     ref 里保存的 profile 重建（复用 spawn 路径的 agent 构造逻辑）；
   - `aborted`：返回明确错误（"failed subagents cannot be resumed;
     spawn a new one"）。

`irc` 工具同步放开：roster（`ListVisibleTo`）展示 parked 代理并标注
"message revives"；直接 DM parked 代理走同一条复活路径；广播**不**复活
parked（对齐 oh-my-pi 的防雪崩设计，`tools/irc.ts:281` 有同样的取舍）。

### 3.4 复活所需的运行时契约

cold revive 要重建 `SessionAgent`，`AgentRef` 需补充字段（内存阶段即可，
持久化到阶段 3 再做）：

```go
type AgentRef struct {
    // 现有字段...
    ProfileName     string // subagent profile（重建工具集/权限）
    ParentSessionID string // 权限派生与成本归集的父会话
    Role            string // spawn 时的 role（复活时保持人设）
    Isolation       string // 已解析的 isolation（worktree 复活需谨慎，见 §5 R3）
}
```

权限重建复用 `DeriveSubagentPermissions`（`subagent_permissions.go`），
成本归集沿用 `previousChildCost` 机制（`coordinator.go:2892` 已处理）。

## 4. 实施阶段

### 阶段 1：park 降级 + 打通 warm/cold revive（核心价值）

1. `agent_registry.go`：新增 `AgentStatusParked`；`AgentRef` 补 §3.4 字段；
   spawn 路径（批量在 `coordinator.go` 的 prepare 段，单个/后台同理）填充。
2. `subagent_lifecycle.go` `Park`：改为 `childSessionAgents.Delete` +
   `registry.SetStatus(id, AgentStatusParked)`，**删除 `Unregister` 调用**。
   `ParkAll`（shutdown 路径）语义不变。
3. coordinator 新增 `resumeSubagent(ctx, ref *AgentRef, prompt string)`：
   - `idle`：`childSessionAgents` 取实例 → `lifecycle.Revoke` →
     构造 `subAgentParams{ExistingSessionID: ref.SessionID, Agent: 实例,
     SubagentType: ref.ProfileName, ...}` → `runSubAgentDirect`；
   - `parked`：按 `ref.ProfileName` 重建 `SessionAgent`（提取 spawn 路径
     中"由 profile 构造 agent"的逻辑为可复用函数）→ 同上；
   - 完成后与 spawn 路径一致：成功 `SetStatus(Idle)` + `Adopt`，失败
     `SetStatus(Aborted)`（注意：aborted 不再 Unregister，保留条目供
     诊断，父会话销毁时清除）。
   - 结果回投：复用后台代理的结果通道（`backgroundAgentRunResult` →
     父会话收到 completion 通知），前台同步续用场景则直接把
     `ToolResponse` 返回给调用方。
4. `backgroundAgentMessenger`（`coordinator.go:4165`）加 fallback：
   后台注册表未命中 → `AgentRegistry` 解析 → `resumeSubagent`。

### 阶段 2：irc 打通 + 工具描述更新

1. `ListVisibleTo` 增加对 parked 的可见性（roster 标注状态与复活提示）。
2. `irc` 直接 DM parked 代理 → 经 responder 链路调 `resumeSubagent`；
   广播跳过 parked。
3. 更新 `tools/send_message.md`、`tools/irc.md`、`templates/agent_tool.md`
   的描述文本，向 LLM 说明"完成的子代理可以用 send_message/irc 续用，
   不要为 follow-up 重新 spawn"。同步更新 `docs/subagent-runtime.md`。

### 阶段 3（可选，独立 PR）：跨进程持久化

1. 新增 SQLite 表 `agent_refs`（id、display_name、parent_session_id、
   child_session_id、profile_name、role、isolation、status、created_at）；
   sqlc 查询放 `internal/db/sql/`，迁移放 `internal/db/migrations/`。
2. spawn/状态变更时写库；启动时扫描 parent 会话的 refs，以 `parked`
   状态注册（等价 oh-my-pi 的 persisted-revive：先校验 child session
   仍存在、工作目录未失效，否则跳过——对应 `persisted-revive.ts:53-61`
   的防御）。
3. `RemoveForSession` 改为软删除或按保留策略清理。

阶段 1+2 一起交付即可对齐 Claude Code 体验；阶段 3 单独评估。

## 5. 风险与决策点

- **R1 并发**：复活的子代理与父会话并发运行。子会话有独立的
  `activeRequests` 键，无冲突；`running` 状态下的 follow-up 走
  `QueuePrompt` 排队，不得直接 `Run`（会触发 busy 语义）。
- **R2 重名**：`DisplayName` 不保证唯一。解析歧义必须报错列候选，
  不允许"最新的赢"（与 Claude Code 不同，crush 的名字来自 LLM 起的任务
  名，歧义概率高）。
- **R3 worktree 隔离**：worktree 子代理完成后 merge-back 已发生、
  worktree 可能已清理。阶段 1 对 `Isolation == worktree` 的 parked 代理
  cold revive 时降级为 `none`（共享父工作区）并在返回文本中注明；
  不要尝试重建 worktree。
- **R4 handoff 前缀**：`ExistingSessionID` 非空时跳过 handoff
  （`coordinator.go:3047`），复活场景正确——历史已在会话里，勿重复注入。
- **R5 内存**：parked 条目常驻注册表（无 SessionAgent，占用极小）。
  父会话销毁时清除，无泄漏；但要确认 `app` 层的清理调用点覆盖
  crash/cancel 路径。

## 6. 测试要求

沿用仓库规范（testify `require`、`t.Parallel()`、mock 模式参考
`coordinator_test.go` 与 `queue_control_test.go`）：

1. lifecycle：`Park` 后注册表条目仍在且状态为 `parked`；
   `childSessionAgents` 已清空。
2. warm revive：Adopt 窗口内 `resumeSubagent` 复用同一 `SessionAgent`
   实例、`ExistingSessionID` 传入 `runSubAgentDirect`、handoff 被跳过。
3. cold revive：Park 后 `resumeSubagent` 重建实例、会话历史从 SQLite
   载回（消息数一致）、profile 权限与 spawn 时一致。
4. 寻址：send_message 对 后台代理/前台 idle/前台 parked/aborted/未知 ID
   五种目标的行为各一条。
5. 歧义：两个同名 parked 代理 → 报错并列出候选。
6. running 排队：对 running 子代理 send_message → 进入 prompt 队列，
   当前 run 结束后自动消费。
7. 回归：`go test ./internal/agent/...` 全绿。

## 7. 代码清理清单（实施后必须处理）

| # | 位置 | 清理内容 | 阶段 |
|---|---|---|---|
| C1 | `coordinator.go:2264-2266` | 注释宣称的 "follow-up agent tool call with ExistingSessionID" 从未成立；改写为描述真实的 send_message 复活链路 | 1 |
| C2 | `coordinator.go:1884` 附近 | `ExistingSessionID` 获得真实赋值点后，补注释说明唯一写入方是 `resumeSubagent`（内部字段，不暴露给 LLM） | 1 |
| C3 | `subagent_lifecycle.go` 头部注释（`:9-35`） | 描述 "removed from registry / leaving only persisted session messages" 的段落过时，改为 parked 状态语义 | 1 |
| C4 | `background_agent.go` `nameToID`/`ResolveAddress`/`LookupByName` | 阶段 2 统一寻址后，后台代理身份并入 `AgentRegistry`，此处仅保留队列派发；删除重复的名字解析（保留兼容期 deprecation 注释亦可，但不要长期双轨） | 2 |
| C5 | `background_agent.go` `Stop()`（`:359`） | runner 置 nil 造成"永久不可续用"，与新状态机矛盾；改为转 `parked`（cold revive 兜底）或明确注释为不可恢复终止并仅用于 shutdown | 2 |
| C6 | `agent_registry.go` `AgentStatusCompleted` | 新状态机中成功终态是 `idle→parked`，`completed` 不再有写入方；删除该常量及 `agent_registry_test.go` 中相关用例（先全库 grep 确认 UI/timeline 无消费） | 2 |
| C7 | `tools/send_message.go:44-65` | 三段 "Failed: ..." 长文案随统一寻址重写；错误分支收敛（unknown / ambiguous / aborted / queue-full 四类） | 2 |
| C8 | `docs/subagent-runtime.md` | "Keep-alive / lifecycle manager" 一节按新状态机重写；`docs/history/` 里的旧设计文档不动 | 2 |
| C9 | `coordinator.go` 批量收尾（`:2271-2284`） | 失败路径的 `Unregister` 改为 `SetStatus(Aborted)`（条目保留），相应调整依赖"失败即消失"的测试 | 1 |
| C10 | `subagent_id_allocator.go:17` 注释 | 同 C1，引用了不可达的 ExistingSessionID 说法 | 1 |

## 8. 非目标

- 不改 `agent` 工具的 spawn 语义与批量执行器（无 DAG 调度诉求）。
- 不做 oh-my-pi 式的 Agent Hub UI；注册表监听（`OnChange`）已够 TUI 用。
- 不在本次统一 `mailbox`（批量兄弟通信）——它服务于 batch 内广播，
  与"续用已完成代理"正交，保持现状。
