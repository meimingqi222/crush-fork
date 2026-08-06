# IRC 代理间通信重构方案

> 状态：阶段 0-2 已实施。
>
> 目标：把 IRC 从"同步调用对方的无上下文后台生成"改成"消息投递 + 接收方
> 真实回合回复"。
>
> **前置依赖（硬）**：`docs/refactor-subagent-continuation.md` 阶段 1
> （`AgentStatusParked` + park 降级 + warm/cold revive）。本文档的状态矩阵
> 依赖 `parked` 状态存在，且不重复设计续用语义——见 §2.3。
>
> 参考实现：`oh-my-pi/packages/coding-agent/src/irc/bus.ts`、
> `session/irc-bridge.ts`。
>
> 实施前必读：`docs/pitfalls/fantasy-dual-message-state.md`。

## 1. 结论摘要

当前 IRC 的语义错位：

```text
A 调用 irc(send, await_reply=true)
  -> 全局 IrcResponder                       (tools/irc.go:117-123)
  -> B.RespondAsBackground                   (coordinator.go:354-363)
  -> 新建一次抛弃式 fantasy.Agent            (agent.go:3817-3822)
  -> 只喂 IRC 文本，512 max output，30s 超时
  -> 同步返回一段 500 字符截断文本           (agent.go:3844-3848)
```

这条路径不读 B 的 session history、不进 B 的主 agent loop，因此不能可靠回答
"B 是否正在改某个文件"、"B 读过哪些代码"、"B 进展到哪"这类问题。

`PrepareStep` 原样透传 `options.Messages`（`agent.go:3832-3835`），确认没有
任何历史注入。

这不是模型槽位问题，是通信模型问题。重构原则：

1. **投递与生成回复分离。** 投递成功 ≠ 对方已回答。
2. **真实回复由接收方自己的 agent turn 产生**，使用接收方当前 inference
   model、会话历史和正常工具能力。
3. **不能并发重入一个 SessionAgent 的主回合。** 忙时排队、在安全边界注入。
4. **`await_reply` 等待的是回复消息**，而不是让发送调用直接触发一次无上下文
   Generate。
5. **不重建旁路回复。** 见 §6：`RespondAsBackground` 应删除而非改进。

## 2. 现状分析（已核实）

### 2.1 三个独立的现存 bug

这三条与架构无关，可以先单独修掉：

**(a) 回复的发送方归属是错的。** `coordinator.go:362`：

```go
return ref.Agent.RespondAsBackground(ctx, targetID, message)
```

签名是 `RespondAsBackground(ctx context.Context, from, message string)`
（`agent.go:3801`）。传进去的 `targetID` 是**接收方自己**。于是 prompt 变成
"You received an IRC message from agent `<你自己>`"。真正的发送方只出现在
`tools/irc.go:120` 拼进 body 的 `[IRC <selfID> → you]` 前缀里。

**(b) `await_reply=false` 对 DM 不可达。** `tools/irc.go:85-90`：

```go
awaitReply := params.AwaitReply
if to == "all" && !params.AwaitReply {
    awaitReply = false
} else if to != "all" && !params.AwaitReply {
    awaitReply = true          // DM 的 false 被强制改成 true
}
```

`AwaitReply` 是裸 `bool`，无法区分"未传"与"显式 false"。**任何保留该字段
形状的兼容方案都无法实现 `await_reply=false`。** 必须改成 `*bool`，或直接靠
§4 的 `op=send` / `op=wait` 切分。

**(c) 主 agent 的状态永远是 idle。** `coordinator.go:316-317` 把主代理注册为
`AgentStatusIdle`，全仓库没有任何地方改过它——`SetStatus` 唯一的调用点是
`coordinator.go:2407`（子代理）。所以发送方无法知道 Main 是否 busy，§4 的
"primary agent"行在当前信号下无法实现。需要把 `SessionAgent.IsBusy()`
（`agent_queue.go:179`）接到 registry。

注意这只修了**状态可见性**，主会话的投递路径本身还有两个待定问题，见 §4.1。

附带现象：完成的子代理在 `coordinator.go:2400-2410` 被设为 `idle` 并继续留在
`ListVisibleTo` 里，直到 TTL 触发 `Park`（当前 `Park` 直接 `Unregister`）。
所以 `irc list` 会把已完工的子代理列为"可达 idle 节点"，DM 过去会在一个
已完成的 SessionAgent 上跑 `RespondAsBackground`。continuation 文档阶段 1
落地后这条现象会变成正常的 parked 语义。

### 2.2 steering 队列已经是所需的注入机制

**不要新建 per-agent IRC 队列。** 仓库已有完整的"安全边界注入"设施：

| 组件 | 位置 | 职责 |
|---|---|---|
| `EnqueueSteer` | `agent_queue.go:44-55` | 入队并 `signalSteering` 打断运行中的工具 |
| `signalSteering` | `agent_queue.go:386-390` | 取消协作式信号，让工具在安全点让出 |
| `popSteeringCalls` | `agent_queue.go:341-351` | 唯一消费者，只在安全 drain 点调用 |
| drain 点 | `agent.go:1240-1266` | `PrepareStep` 内，优先于 join-active-run prompt |
| `flushStrandedSteeringMessages` | `agent_queue.go:397-409` | 把错过所有 drain 点的消息提到常规队列队首 |
| coordinator 侧入口 | `queue.go:44` | 已在使用 |

再建一条平行队列 + 独立 drain 点，正是 §7 要防的双重注入的来源。

但**不能直接复用 steering 的语义权重**。`EnqueueSteer` 做了两件事：

```go
a.enqueueSteer(sessionID, call)
a.signalSteering(sessionID)   // 取消协作信号，打断运行中的工具
```

于是 peer 消息会继承两个不该有的属性：

**(a) 注入措辞。** `formatSteeringPrompt`（`agent_queue.go:415-421`）硬编码
"The user sent a message while you were working. Treat it as the active
instruction; it supersedes earlier directions if they conflict."。用户 steer
有权改写当前指令，**peer 消息没有**。需要 source-aware formatter：区分
`InitiatorUser` 与 peer 来源，peer 版本的语义应是"一条待处理的入站消息，
不改变你当前的任务优先级"，并说明"如需回复，在正常回合调用 `irc send` 并带
`reply_to`"。

**(b) 协作式打断。** 工具通过 `GetSteeringSignalFromContext`
（`tools/tools.go:166-169`）select 在该信号上，前台 bash 等会在安全点让出。
一条 peer 消息**不应该**打断接收方正在跑的测试或构建。因此 IRC 投递默认
**只入队、不 `signalSteering`**，消息在下一个自然的 `PrepareStep` 被 drain。

唯一例外见 §5.3：接收方自己正阻塞在 `op=wait` 时必须被唤醒。

实现上建议给 `SessionAgentCall` 或投递选项加一个来源标记，让 formatter 和
signal 决策都读同一个字段，而不是在两处分别判断。

### 2.3 与 continuation 文档的分工裁决

`docs/refactor-subagent-continuation.md` §3.1 已经定下：

> `agent` 工具保持 spawn-only；follow-up 统一经 `send_message`（及 irc）。
> `ExistingSessionID` 仍然是内部字段，由复活路径填充。

**本文档采纳该结论**，不再提出独立的 `follow_up` 工具或 `agent_follow_up`
API。理由：`send` 与 `follow_up` 的区别（"只是协调"还是"追加一轮正式工作"）
是**接收方**该做的判断，不是调用方；强迫父模型在两个工具间选择，只是把
判断成本转嫁给模型。

因此本文档的范围**不含**：

- 稳定 agent identity、park/revive、cold revive、跨进程持久化
  → `docs/refactor-subagent-continuation.md`；
- 子代理结果投影、payload 断链、locator 卫生
  → `docs/refactor-subagent-result-contract.md`；
- public handle 体系 → 不做。agent ID 已经是 handle 形态
  （`"0-Main"` / `"0-Main::explore-<rand>"`），见 result-contract 文档 §2.3。

### 2.4 模型槽位的准确现状

`agent.go:3804-3812`：

```go
bgModel := a.largeModel.Get()
if a.backgroundModel != nil {
    bgModel = a.backgroundModel.model
    ...
}
```

所以"IRC 默认用 background_model"只在配置了 background model 时成立，否则
回落到 large model。无论哪种，都不是"接收方自己的推理模型 + 历史"，结论不变。

## 3. 目标架构

### 3.1 分层

```text
IRC Tool
  -> IrcBus（coordinator-owned）：解析地址、生成消息 ID、投递、等待
      -> AgentRegistry：查询 peer 和运行状态
          -> SessionAgent.EnqueueIRC → 复用 steering 队列
              -> Coordinator：唤醒 idle / revive parked
                  -> 接收方自己的 inference model + history + tools
```

`background_model` 不进入 IRC 任何路径。它继续服务 compaction、memory、
handoff。

### 3.2 消息信封

```go
type IRCMessage struct {
    ID          string
    From        string
    To          string
    Body        string
    ReplyTo     string
    ExpectReply bool
    CreatedAt   time.Time
}
```

- `From` / `To`：精确 agent ID，不用 DisplayName 作内部主键（重名歧义见
  continuation 文档 R2）；
- `ExpectReply`：仅表示发送方在等待，不触发任何自动生成；
- `CreatedAt`：排序、超时、诊断。

投递结果与回复结果分开：

```go
type IRCDeliveryOutcome string

const (
    IRCInjected IRCDeliveryOutcome = "injected"  // 注入运行中回合
    IRCWoken    IRCDeliveryOutcome = "woken"     // 唤醒 idle
    IRCRevived  IRCDeliveryOutcome = "revived"   // 复活 parked
    IRCQueued   IRCDeliveryOutcome = "queued"    // 排队待处理
    IRCFailed   IRCDeliveryOutcome = "failed"
)

type IRCDeliveryReceipt struct {
    MessageID string
    To        string
    Outcome   IRCDeliveryOutcome
    Error     string
}
```

receipt 只说明"消息是否到达可处理的接收方"，绝不携带自动生成的文本。

## 4. 接收方状态矩阵

| 接收方状态 | 消息如何进入 | 回复来源 | `await_reply` 行为 |
|---|---|---|---|
| `running`，工具/流式中 | 入 steering 队列，**不** `signalSteering`；下一个 `PrepareStep` drain | 当前回合稍后回复 | 等待真实回复 |
| `running`，且阻塞在 `op=wait` | 入队 **并** `signalSteering`（§5.3 的唯一例外） | 当前回合被唤醒后回复 | 等待真实回复 |
| `running`，来自父 agent | 同上（steering 本就是高优先级语义） | 当前回合 | 等待 |
| `idle` | coordinator 受控唤醒，启动真实回合 | 接收方 inference model | 等待真实回复 |
| `parked` | coordinator/lifecycle warm 或 cold revive | 接收方 inference model | 等待复活后的回复 |
| `aborted` | 不投递 | 无 | 立即返回明确失败 |
| primary agent，`running` | 入队，不打断（见 §4.1(a) 的计费约束） | 主 agent inference model | 等待真实回复 |
| primary agent，`idle` | 只入 pending 队列，**不自动唤醒**（§4.1(b)） | 随用户下一次输入 | 返回 `IRCQueued`，大概率超时 |

关键约束：

- 不能从 IRC 工具的执行栈里直接调 `Run` 或直接调 provider——`running` 会重入
  主循环，`idle` 会绕过 coordinator 的受控入口。
- 广播只投递当前可达 peer。**parked peer 默认不因广播复活**（对齐 oh-my-pi
  的防雪崩取舍）。

### 4.1 primary agent 的投递路径

主会话不是"另一个 peer"，它的投递有两个必须先定的问题。

**(a) busy 时不能复用 `coordinator.Steer`。** `queue.go:44-50` 硬编码
`InitiatorType: copilot.InitiatorUser`：

```go
func (c *coordinator) Steer(sessionID, prompt string, ...) bool {
    return c.currentAgent.EnqueueSteer(sessionID, SessionAgentCall{
        ...
        InitiatorType: copilot.InitiatorUser,
    })
}
```

该值经 `copilot.ContextWithInitiatorType` 落到 `X-Initiator` 请求头，而
`internal/oauth/copilot/billing.go:21-23` 定义了 `user` 计费、`agent` 免费。
IRC 投递若复用这条路，会把 agent 发起的请求记成用户发起，**计费归属错误**。

IRC 投递必须走独立入口并使用 `copilot.InitiatorAgent`
（`internal/agent/vision.go:118` 是现成先例）。这条同样适用于向子代理投递。

**(b) idle 时默认不自动唤醒。** 唤醒一个 idle 子代理没有外部可见性；在用户
可见的主会话里启动一个用户没有请求的回合是产品行为，不是实现细节——它会产生
用户没预期的 token 消耗、可能与用户正在输入的内容冲突，且没有明确的取消入口。

因此本文档的决定：**主 agent 为 `idle` 时，IRC 消息只入 pending 队列并返回
`IRCQueued`**，同时发出 UI 事件提示有待处理的 peer 消息；消息随用户下一次
输入一并进入上下文。不为 peer 消息自动启动主会话回合。

发送方看到 `IRCQueued` 而非 `IRCWoken`，因此 `await_reply=true` 在这种情况下
大概率超时——这是正确的语义：主 agent 的注意力归用户，不归 peer。若后续确实
需要自动唤醒，应作为显式配置项单独评估，默认关闭。

## 5. `send` 与 `await_reply` 语义

### 5.1 目标操作集

```text
irc op=send  -> delivery receipt，返回 message_id
irc op=wait  -> 按 message_id / from / timeout 等待 reply
irc op=list  -> 查询可见 peer（现状保留）
```

回复是另一条 `IRCMessage`：

```text
reply.ReplyTo == request.ID
reply.From    == request.To
reply.To      == request.From
```

### 5.2 兼容策略

`AwaitReply` 因 §2.1(b) 必须改成 `*bool`：

- `nil`：沿用现有默认（DM 等待，广播不等待）；
- `false`：只返回 receipt；
- `true`：内部执行 `send + wait`，但两个状态机保持分离。

超时返回"已投递但尚未收到回复"，**不能把超时误报成投递失败**。超时后仍可用
`op=wait` 收到迟到回复。

### 5.3 死锁防护：`op=wait` 必须响应 steering signal

基线约束：

- 等待超时必设；
- sender context 取消时解除 waiter；
- waiter 按 `To` + `ReplyTo` 精确匹配；
- 真实回合不因等待回复而占用不可释放的 session 锁。

**环形等待的解法不是等待图检测，而是让 `wait` 可被打断。** 推导：

1. 阻塞在 `op=wait` 的 agent 处于 busy，因此 `EnqueueSteer` 的
   `IsSessionBusy` 检查（`agent_queue.go:45-47`）会通过，入站消息成功入队；
2. 该消息**是** §2.2(b) 的唯一例外——投递方要调用 `signalSteering`；
3. `op=wait` 的实现 select 在 `GetSteeringSignalFromContext` 上，收到信号后
   返回"等待被入站消息打断"，让回合前进到 `PrepareStep`；
4. drain 点处理入站消息，接收方可以回复，环解开。

这比"waiter 注册时检查目标是否在等自己"更强：后者只覆盖 A↔B 两节点环，
A→B→C→A 就漏了；前者对任意长度的环都成立，且不需要维护等待图。

超时仍然是最后的兜底，但正常情况下环应该在一个 step 内解开，而不是靠双双
超时。

## 6. 为什么不做旁路回复

早期方案曾计划用"接收方 inference model + 历史快照"重建一条旁路回复路径，
替换 `RespondAsBackground`。**本文档放弃该方案。**

理由：

1. §2.2 落地后，唯一剩下的场景是"接收方短期内到不了 step boundary"。对这种
   情况，`IRCQueued` + "已投递，尚无回复"是**比快照猜测更真实也更有用**的
   答案。
2. snapshot builder 是整个计划里最贵、最危险的一项：要在不复用主回合可变
   fantasy message state 的前提下构造一致快照，受
   `docs/pitfalls/fantasy-dual-message-state.md` 约束。为一个兜底路径付这个
   成本不划算。
3. 保留任何形式的自动回复，都会让调用方难以区分"接收方的真实工作状态"与
   "模型基于快照的猜测"。§1 原则 1 的收益会被抵消掉。

因此：**`RespondAsBackground`（`agent.go:3801-3849`）在阶段 2 完成后删除**，
连带 `SessionAgent` 接口声明（`agent.go:221`）、`coordinator.go:354-363` 的
全局 responder 注册、`tools/irc.go:46,183-198` 的 `IrcResponder` 类型与全局
单例，以及 `coordinator_test.go:91`、`tools/irc_test.go:72-102` 的相关桩。

同时不需要引入 `irc_reply` 模型槽位。

## 7. 消息显示与持久化

| 事件 | 进入 agent context | 持久化 | 显示 |
|---|---:|---:|---:|
| `irc:incoming` | 是，在 steering drain 点 | 是 | 是 |
| `irc:reply` | 是（作为发送方收到的消息） | 是 | 是 |
| delivery receipt | 否 | 可选诊断事件 | 可选 |

必须避免双重注入：

- 已进入 agent context 的消息不能再次被 drain（复用 steering 队列的单消费者
  约束天然满足，前提是不新建平行队列）；
- 已被 waiter 消费的 reply 不能再注入下一轮；
- UI relay 不改变 agent context。

## 8. 实施阶段

### 阶段 0：修现存 bug（可独立提交）

1. 修 §2.1(a)：`coordinator.go:362` 传真正的发送方 ID。需要给 responder
   签名加 sender 参数，或在删除 responder 前先做最小修复。
2. `IrcParams.AwaitReply` 改 `*bool`（§2.1(b)），补 DM 显式 false 的测试。
3. 把主 agent 的 busy 状态接到 registry（§2.1(c)）。
4. 补 message ID、超时、投递结果的日志/事件，让现状可观测。
5. 更新 `tools/irc.md`，明确当前 IRC 回复**不是**接收方真实工作状态。

### 阶段 1：消息总线与真实投递

目标：不丢消息、不重入、不把投递当回复。

1. 新增 coordinator-owned IRC bus（建议 `internal/agent/irc_bus.go`）：
   message ID、peer lookup、waiter、delivery outcome、reply correlation、
   timeout/cancel、UI relay。**该层不选择 provider，也不构造 agent turn。**
2. 用注入的 bus 替换 `tools.SetIrcResponder` 全局单例（全局单例会跨
   coordinator、测试和并发 session 串线）。
3. `irc.send` 默认只投递并返回 receipt。
4. `running` 接收方走 steering 队列（§2.2），但**默认不 `signalSteering`**，
   并使用 peer 版本的 formatter。不得从工具调用栈直接重入 `Run`。
5. 投递路径使用 `copilot.InitiatorAgent`，不复用 `coordinator.Steer`
   （§4.1(a) 的计费约束）。
6. `idle` **子代理**由 coordinator 受控唤醒，使用自己的 inference model 和
   history；`idle` **主代理**只入队并返回 `IRCQueued`（§4.1(b)）。
7. 广播对每个目标单独返回 outcome；限制 fan-out 数、消息大小和并发唤醒数。
8. `parked` 复活复用 continuation 文档阶段 1 的 `resumeSubagent`；若该阶段
   尚未落地，返回明确的 `IRCFailed` outcome 而非静默降级。

### 阶段 2：真实回复与 `await_reply`

1. 入站 prompt 明确要求：需要回复时调用 `irc send` 并设置 `reply_to`。
2. bus 加 waiter，按 request ID 精确匹配。
3. `op=wait` 独立开放，允许先发后等。其实现必须 select 在
   `GetSteeringSignalFromContext` 上（§5.3），且投递给一个正在 `wait` 的
   接收方是 §2.2(b) 的唯一 `signalSteering` 例外。
4. 处理超时、取消、重复回复、未知 request ID、环形等待。
5. reply 由接收方正常 agent turn 产生，**可以使用工具**；副作用受接收方原有
   权限、yolo/auto 模式和 tool registration 约束。
6. 删除 §6 列出的全部旁路代码。

## 9. 模型策略

| 场景 | 模型 |
|---|---|
| 接收方正常 IRC 回合 / idle 被唤醒 / busy 的下一 step | 接收方的 inference model |
| compaction、summarize、memory、handoff | `background_model` |
| IRC 回复 | 不使用 `background_model`，也不新增槽位（§6） |

模型选择必须服从执行语义：真实回合需要保持接收方的角色、历史、工具和权限。
`background_model` 只代表"后台任务成本档位"，不代表"任意 agent 的临时意识"。

## 10. 风险与边界

### 10.1 消息状态

Fantasy 存在消息状态与 provider 状态的双重约束。放弃旁路快照（§6）后本次
不再触碰这一约束，但注入路径仍要遵守。实施前阅读：

- `docs/pitfalls/fantasy-dual-message-state.md`
- `internal/agent/session_projection.go`
- `internal/agent/agent_queue.go`

### 10.2 工具副作用

正常 IRC 回合可以调用工具，因此它不是"问答回调"。必须保留接收方的权限模型
和当前 session 的 tool registration，不能因为消息来自 peer 就提权。

### 10.3 共享工作区状态

"你是否正在修改 config.go"不等同于"文件当前是否被修改"。agent 状态、
filetracker、git 状态和文件锁是不同的事实源。IRC 回复只能陈述接收方上下文中
有证据的状态。

### 10.4 环形等待

父等子、子又等父的场景靠 §5.3 的"`wait` 响应 steering signal"解开，超时只是
兜底；不能用同步嵌套 `Generate`。

### 10.5 主会话的注意力归属

主 agent 的回合属于用户。§4.1(b) 的"idle 不自动唤醒"不是性能取舍，而是产品
边界——一旦 peer 可以自发启动主会话回合，用户会看到自己没有请求的 token 消耗
和输出。任何放宽都必须是显式配置且默认关闭。

## 11. 测试清单

### 单元测试

- message ID 生成、`ReplyTo` 关联、重复消息去重；
- delivery receipt 与 reply result 分离；
- waiter 精确匹配 `To` + request ID；
- timeout、cancel、unknown peer、aborted peer；
- `running` / `idle` / `parked` 状态矩阵各一条；
- DM 显式 `await_reply=false` 只返回 receipt（§2.1(b) 回归保护）；
- 入站 prompt 里的发送方是真正的 sender（§2.1(a) 回归保护）；
- peer 消息的注入措辞**不含** supersede 语义，用户 steer 的仍然含有
  （§2.2(a)）；
- peer 消息投递**不**触发 `signalSteering`，运行中的协作式工具不让出
  （§2.2(b)）；
- 投递路径的 `InitiatorType` 是 `InitiatorAgent`（§4.1(a) 计费回归保护）；
- 向 idle 主代理投递返回 `IRCQueued` 且不启动回合（§4.1(b)）；
- 广播不重复投递、不复活 parked peer；
- provider failure 后消息仍可诊断或重新处理。

### 并发与竞态

- IRC 投递与主 agent stream 并发；
- IRC 投递与 tool execution 并发；
- agent 完成 / park / revive 与投递同时发生；
- 多个发送方同时等待同一接收方；
- 双向互发；
- A 等 B、B 等 A 的两节点环，以及 A→B→C→A 三节点环：都应在一个 step 内解开
  （靠 `wait` 响应 steering signal），**不依赖超时**（§5.3）；
- coordinator shutdown 时投递；
- `go test -race` 覆盖 bus、queue、coordinator、agent。

### 集成测试

1. A 向 idle B 发消息，B 用自己的历史完成真实回合并回复。
2. A 向 busy B 发消息，消息在 steering drain 点注入，B 不被重入。
3. A 询问 B 是否修改某文件，B 的回复来自历史/真实工具状态，而非无上下文猜测。
4. A `await_reply=true` 超时后，仍能通过 `op=wait` 收到迟到回复。
5. parked B 被 DM 后复活，消息只处理一次。
6. B 不可复活（aborted）时，A 收到明确失败，不产生伪造回复。
7. primary agent、普通 subagent、background agent 使用统一的 peer 与消息语义。
8. 同一条消息不会在 UI、session history、agent context 中重复注入。

## 12. 验收标准

- IRC send 的成功只表示消息已投递或排队；
- 接收方的正常回复来自接收方自己的 agent turn，可关联到 request ID；
- busy agent 不会因 IRC 被并发重入；
- **不存在任何自动生成的回复路径**——`RespondAsBackground` 及全局 responder
  已删除；
- `background_model` 只负责后台任务；
- 超时、取消、迟到回复、不可达 peer 都有明确状态；
- 环形等待在一个 step 内解开，不靠超时；
- peer 消息不打断接收方运行中的工具，也不冒充用户指令；
- IRC 触发的 provider 请求计入 `InitiatorAgent`，不误记为用户发起；
- 主 agent、subagent、parked/revived agent 的寻址语义一致；投递语义按 §4.1
  在主代理上有意区别对待；
- 同一条 IRC 消息不会被重复注入。
