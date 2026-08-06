# Subagent 结果契约与 locator 卫生重构

> 状态：设计方案，尚未实施。
>
> 目标：修复"子代理严格按 `OutputSchema` 返回结构化结果时，父模型反而看到
> `no textual response`"这一断链；并把结果定位符（child session ID）从模型
> 可见输出中收回。
>
> 依赖：无。可独立交付，不依赖 IRC 或 subagent 续用重构。
>
> 相关文档：`docs/refactor-irc.md`（代理间通信）、
> `docs/refactor-subagent-continuation.md`（续用与生命周期）。三者共享
> §5 的 control plane / data plane 边界原则，但实施上互不阻塞。

## 1. 结论摘要

Explore 的 `OutputSchema` 把 `summary` 定义为 payload 内的必填字段
(`internal/config/config.go:1704-1735`)，但整条结果投影链路**从不消费
`Yield.Payload`**。于是一个完全合规的 Explore 子代理会让父模型看到：

```text
Subagent completed with no textual response. Open child session
<messageID>$$<toolCallID> from this Agent tool call to inspect tool
outputs and details.
```

这不是模型不会返回结果，而是运行时把结构化结果当成了"无结果"。Explore 是
最常用的子代理，且是 crush 对抗"全文件内容转发"反模式的主要架构手段——当前
状态下它越合规越不可用。

## 2. 现状分析（已核实）

### 2.1 payload-only yield 的四个断点

`yield` 允许成功结果只提供 `data` 或 `payload`
（`internal/agent/tools/yield.go:125-127`）：

```go
if (status != failed && status != blocked) && data == "" && len(payload) == 0 {
    return fantasy.NewTextErrorResponse("data or payload is required ...")
}
```

但只提供 `payload` 时，结果在四处依次丢失：

| # | 位置 | 行为 | 后果 |
|---|---|---|---|
| B1 | `tools/yield.go:184-189` | `summary := data`；`payload` 只进 metadata，不进 `ToolResponse.Content` | `Content == ""` |
| B2 | `coordinator.go:2551-2565` `withSubagentOutputMetadata` | 只取 `Content` → `Yield.Data` | `Preview == ""`，`OutputChars == 0` |
| B3 | `coordinator.go:3259-3266`（单个子代理路径） | 仅当 `Yield.Data != ""` 时取用，否则落 `subAgentNoContentText` | 父模型收到 "no textual response" + child session ID |
| B4 | `coordinator.go:3740-3786` `subagentOutputDetailsForModel`（批量路径） | 候选筛选依次试 `Content`/`Yield.Data`/`Preview`，三者皆空则整个任务被跳过 | 批量结果里该任务无输出段 |

`subAgentNoContentText`（`coordinator.go:4284-4290`）共有三个调用点：
`:2878`、`:3265`、`:3602`。

### 2.2 泄漏给模型的是内部复合键

`CreateAgentToolSessionID` 直接拼接 `messageID$$toolCallID`
（`internal/session/session.go:1279-1282`）。它的职责是数据库与恢复链路的
canonical identity，不适合作为 LLM 可见的 locator：

- 长度由父消息 ID 和 tool call ID 共同决定；
- 含 `$$` 结构分隔符，模型容易漏字符、改写或截断；
- 把内部调用拓扑暴露给模型，不提供额外语义。

`subAgentNoContentText` 把它直接写进父模型可见文本，是当前最主要的泄漏点。

### 2.3 peer ID 已经是 handle 形态，不必重新设计

需要澄清一个容易误判的点：**agent ID 本身已经不是 UUID**。

- 主代理：`"0-Main"`（`coordinator.go:303`）；
- 子代理：`fmt.Sprintf("%s::%s-%s", c.mainAgentID, uniqueName, generateAgentID())`
  （`coordinator.go:2248`），其中 `uniqueName` 由 `subagentIDAllocator` 产出
  `explore` / `explore-2` 形态（`internal/agent/subagent_id_allocator.go:31-41`）。

所以本次**不需要**引入一套全新的 handle 体系。真正的 ID 卫生问题只有两条：

1. 子代理 ID 里的 `0-Main::` 前缀与 `-<rand>` 后缀是纯噪声（随机后缀存在的
   理由是防止并发批次 registry 撞键，见 `coordinator.go:2240-2247`，不能简单
   删掉）；
2. child session ID 通过 §2.2 的路径泄漏。

### 2.4 `SubagentTaskRef` 已可用，但仅限单次调用内

`SubagentTaskRef`（`coordinator.go:2568-2577`）产出
`<toolCallPrefix>-<index>-<slug>`，比 canonical ID 适合模型，但由 tool call
前缀和 index 组成，跨轮次不稳定。它的正确定位是**本次父 tool 调用内的结果
别名**，不是长期身份。长期身份是 agent ID（§2.3）。

## 3. 目标契约

### 3.1 三类数据必须分开

| 数据 | 消费者 | 是否必须直接返回给父模型 |
|---|---|---:|
| `summary` / `data` | 父 agent 当前模型 | 是 |
| `payload` | reducer / UI 的结构化逻辑 | 可选，但不能替代 summary |
| child session ID / task ref / artifact locator | follow-up、完整输出、诊断、UI | 否 |

- **data plane**：summary、findings、file references、decisions、next actions。
  先交付这一层，父模型才能判断是否需要 follow-up。
- **control plane**：agent ID、child session ID、task ref、artifact ID。只用于
  定位和继续操作。

### 3.2 规则

1. 成功或带警告的 `yield` 必须产出父模型可读的文本；
2. `payload` 是结构化补充，不得成为唯一成功输出；
3. 模型只提交 payload 时，**运行时负责生成 deterministic summary**——不是
   要求模型重试，也不是返回"无文本响应"；
4. 父工具响应顺序：summary → 状态 → locator；
5. 只有 `data` 和 `payload` **同时**为空时才允许出现 no-content 文案；
6. 结构化 payload 投影成文本时长度受限，按总预算公平分配，防止单个任务占满
   父模型上下文（复用 `subagentOutputAggregateCharsLimit` /
   `subagentOutputPerTaskCharsLimit` 现有预算逻辑）；
7. 自动模式的 handoff 安全审查仍可对完整 raw output 做隔离，但不能把正常
   handoff 替换成空 stub。

### 3.3 payload → summary 的投影规则

投影必须是 deterministic 的纯函数，不调用模型。按 schema 形状分层处理：

1. 若 payload 顶层有 `summary` 字符串字段（Explore 的情况），它就是 summary；
2. 其余顶层字段追加为带标签的段落；数组投影成条目列表并受条目数上限约束；
3. 任何字段缺失都不构成失败——投影尽力而为；
4. 全部字段都无法产出可读文本时，退化为紧凑 JSON（仍然优于 no-content）。

**字段顺序**必须 deterministic，且不能依赖 Go 的 map 迭代顺序（随机化）。
注意 schema 的两个键在这一点上性质不同：

- `properties` 是 JSON object → 解码成 `map[string]any`，**声明顺序已丢失**；
- `required` 是 JSON array → 解码成 slice，**声明顺序保留**，且 schema 作者
  放进 `required` 的正是最重要的字段。

因此顺序规则是：`summary` → `required` 声明顺序 → 其余 `properties`
按字母序 → schema 未声明的 payload 字段按字母序。Explore 的
`required: ["summary", "files"]` 于是得到 summary → files（证据）→
architecture，而纯字母序会把 architecture 排到 files 前面。

`required` 可能是 `[]string`（`config.go` 里的 Go 字面量）或 `[]any`
（crush.json 覆盖经 `json.Unmarshal`），两种都要处理。

Explore 的期望输出：

```text
IRC reply currently bypasses the recipient session and runs a background
Generate.

Files:
- internal/agent/tools/irc.go:117-135 — 同步调用全局 responder
- internal/agent/agent.go:3801-3849 — 抛弃式 Generate，无 history

Architecture:
...
```

（`summary` 直接开头，不加 `Summary:` 标签——它是正文，不是附加字段。）

而不是：

```text
Subagent completed with no textual response. Open child session
<messageID>$$<toolCallID> ...
```

## 4. 实施步骤

### 阶段 1：修复 payload 断链（核心价值）

1. 新增 `projectYieldPayload(payload json.RawMessage, schema map[string]any) string`
   ——实现 §3.3 的 deterministic 投影。放 `internal/agent/` 而非 `tools/`，
   因为 coordinator 侧的两条路径都要用。

   **注意 import 方向**：`internal/agent` 导入 `internal/agent/tools`，反向
   不成立。所以 B1 在 `tools/yield.go` 里无法直接调用该函数。解法是依赖注入
   ——给 yield 工具加一个 `WithPayloadProjector(func(json.RawMessage, any) string)`
   选项，在 `tool_registration.go`（package `agent`）唯一的注册点用闭包注入
   `projectYieldPayload`。这样投影逻辑仍然只有一份实现，不在 `tools/` 里复制。
2. **B1**：`tools/yield.go:184-189`——`data == "" && len(payload) > 0` 时用
   投影结果填充 `Content`。`Yield.Data` 保持原样（空），以免混淆"模型给的"
   与"运行时生成的"；两者的区分靠 metadata 新增字段记录。
3. **B2**：`coordinator.go:2551-2565` `withSubagentOutputMetadata`——候选链
   补 `Yield.Payload` 投影，使 `Preview` / `OutputChars` 正确。
4. **B3**：`coordinator.go:3259-3266`——`hasYield` 且 `Data` 为空但 payload
   非空时使用投影，不落 `subAgentNoContentText`。
5. **B4**：`coordinator.go:3747-3786`——候选筛选与正文渲染的两处候选链
   （`:3749-3755`、`:3776-3782`）都补 payload 投影。**两处必须一致**，否则
   任务会被计入预算却渲染为空。
6. 把 §2.1 的四段候选链逻辑收敛成**一个**辅助函数，避免第五处遗漏。

### 阶段 2：收回 locator

1. `subAgentNoContentText`（`coordinator.go:4284-4290`）不再输出 child
   session ID。改为引用稳定 agent ID：

   ```text
   Subagent completed with no textual response. Continue or inspect it with
   send_message(agent_id="<agentID>").
   ```

   注意三个调用点（`:2878`、`:3265`、`:3602`）当前只拿到 `childSessionID`，
   需要把 agent ID 传下去。
2. `subagentSessionDetailsForModel` 是**第二个泄漏点**（批量路径的
   "Child sessions:" 段落，直接把 child session ID 写进模型可见文本）。改名
   为 `subagentLocatorsForModel`，只输出 `agent_id` 和 `task_ref`，段落标题
   改成 "Subagents:"。`result.AgentID` 在该结构里本来就有，所以去掉 session
   ID 几乎零成本。
3. reducer 的 `ToolResultReducerChildSession.SessionID`
   （`coordinator.go:2543-2547`）保持不变——它服务 UI 和 DB，不是模型可见
   文本。
4. 子代理 ID 的噪声（§2.3 第 1 条）：**本次不改**。它是 registry 撞键防护，
   改动要连带 IRC roster 和续用寻址一起评估，归
   `docs/refactor-subagent-continuation.md`。
5. `SubagentTaskRef` 保持现状，文档化为"单次父 tool 调用内的结果别名"
   （§2.4），并修正 `subagent_id_allocator.go:14-18` 中引用了不可达
   `ExistingSessionID` 说法的注释（与 continuation 文档 C10 是同一条，谁先
   落地谁做）。

## 5. 测试要求

沿用仓库规范（testify `require`、`t.Parallel()`）：

1. `projectYieldPayload` 单元测试：Explore schema 的完整 payload、缺 `files`、
   缺 `summary`、空对象、非对象 JSON、超长数组各一条；
2. payload-only yield 走**单个**子代理路径 → 父 tool response 文本包含
   payload 里的 `summary`，且**不**包含 `no textual response`；
3. payload-only yield 走**批量**路径 → 同上，且该任务出现在 `Task outputs:`
   段落里；
4. `data` 和 `payload` 都为空 → 仍然产出 no-content 文案（回归保护，现有
   `coordinator_test.go:667,711` 的断言必须继续通过）；
5. 大 payload 只截断补充字段、保留 `summary`；
6. no-content 文案不含 `$$`（locator 泄漏回归保护）；
7. `go test ./internal/agent/... ./internal/config/...` 全绿。

## 6. 验收标准

- Explore/plan/review 等带 `OutputSchema` 的子代理，成功结果在**同一个**
  agent tool response 中直接给出父模型可读的 summary；
- 父模型不需要解析 child session ID、也不需要调用查询工具就能拿到基本结论；
- locator 只用于超出摘要预算的完整输出、续用 session、查看 artifact 或诊断；
- 模型可见文本中不再出现 `messageID$$toolCallID` 形态的复合键；
- 只有真正无任何输出时才出现 no-content 文案。

## 7. 非目标

- 不引入新的 public handle 体系（§2.3：agent ID 已经是 handle）。
- 不新增 `agent_output` / `agent_follow_up` 之类的工具——续用入口的裁决属于
  `docs/refactor-subagent-continuation.md` §3.1（结论是走 `send_message`）。
- 不改 `yield` 的参数形状或 `OutputSchema` 校验/自修复逻辑。
- 不改 auto mode 的 handoff review 隔离策略。
