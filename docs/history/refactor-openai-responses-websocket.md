# OpenAI Responses API WebSocket 重构方案

> 参考实现：`D:\code\copilot-refs\codex`（Rust，`codex-api` + `core`）
> 目标仓库：Crush（Go，`fantasy` + `internal/agent` + `internal/httpext`）
> 状态：规划文档（未实施）

## 1. 背景与目标

OpenAI Responses API 支持两种流式传输：

| 传输 | 典型路径 | 特点 |
|------|----------|------|
| HTTP SSE | `POST /v1/responses` + `stream: true` | 每请求独立连接，实现简单 |
| WebSocket | `wss://…/v1/responses` + `response.create` 帧 | 可复用连接、增量上下文、`previous_response_id`、更低首包延迟 |

Crush 已具备 **实验性** WebSocket 开关（`providers.*.responses_websocket`），但实现方式与 Codex 差距较大，**对 WS 的“支持不全”** 主要体现在：无连接复用、无 v2 协议能力、无 turn 级路由状态、无 HTTP 回退策略、无 prewarm/增量请求优化。

本方案目标：

1. 对齐 Codex 的 **传输分层** 与 **生命周期**（session / turn / stream）。
2. 在保持 `fantasy` 与现有 `openai-go` Responses 流解析的前提下，逐步替换 `httpext` 的“每请求伪装 SSE”方案。
3. 默认行为不变：`responses_websocket: false` 时仍走 HTTP SSE。

## 2. 现状对照

### 2.1 Crush 当前架构

```
coordinator.buildOpenaiProvider()
  → wrapOpenAIStreamingHTTPClient(httpClient, responsesWebSocket)
       → httpext.openAIResponsesWebSocketTransport.RoundTrip()
            若 POST */responses 且 body.stream==true:
              Dial WS(同 URL 改 scheme)
              发送 {"type":"response.create", ...}（去掉 stream 字段）
              将 WS Text 消息包装为 text/event-stream（event: <type>\ndata: …）
            否则：透传底层 HTTP
  → fantasy openai provider
       → responses_language_model.StreamText()
            → client.Responses.NewStreaming()  // 仍认为自己在读 SSE
```

关键文件：

- `internal/config/config.go` — `ProviderConfig.ResponsesWebSocket`
- `internal/agent/providers.go` — `wrapOpenAIStreamingHTTPClient`
- `internal/httpext/openai_responses_ws.go` — 传输 shim
- `fantasy/providers/openai/responses_language_model.go` — Responses 语义与事件映射

已有测试：`internal/httpext/openai_responses_ws_test.go`（单请求 created/completed 两条事件）。

### 2.2 Codex 参考架构

```
ModelClient (session 生命周期)
  ├─ disable_websockets: AtomicBool   // 本会话 WS 失败后永久走 HTTP
  └─ cached_websocket_session

ModelClientSession (每个 turn 新建)
  ├─ WebsocketSession.connection      // 懒连接、复用
  ├─ turn_state: OnceLock<String>     // x-codex-turn-state 粘滞路由
  ├─ last_request / last_response     // 增量 WS 请求与 previous_response_id
  └─ stream_responses_websocket()
        → ResponsesWebsocketConnection.stream_request(ResponsesWsRequest)
        → process_responses_event() → ResponseEvent 统一枚举
        失败 → FallbackToHttp (ResponsesClient SSE)
```

关键文件：

- `codex-rs/codex-api/src/endpoint/responses_websocket.rs` — WS 连接、泵、空闲超时、错误时拆连接
- `codex-rs/codex-api/src/common.rs` — `ResponsesWsRequest`、`ResponseCreateWsRequest`、`client_metadata`
- `codex-rs/core/src/client.rs` — 选型、复用、prewarm、`responses_websockets=2026-02-06` beta
- `codex-rs/websocket-client/` — 代理/TLS 拨号
- `codex-rs/core/tests/suite/client_websockets.rs` — 行为契约测试

### 2.3 能力差距矩阵

| 能力 | Codex | Crush 现状 | 影响 |
|------|-------|------------|------|
| WS 作为一等传输 | 是 | 仅 HTTP RoundTrip 内 shim | 难以做连接级策略 |
| 连接复用（session/turn） | 是 | 每 stream 新 Dial | 延迟、连接上限（60min） |
| `responses_websockets` v2 beta | 是 | 仅 `responses-api=v1`（部分 host） | 官方 WS 特性可能不可用 |
| `x-codex-turn-state` | 是 | 无 | 多步 turn 路由可能不稳定 |
| `previous_response_id` / 增量 input | 是 | 无（全量 body 每次） | token/带宽浪费 |
| prewarm (`generate=false`) | 是 | 无 | 首包慢 |
| WS 失败 → HTTP 会话级回退 | 是 | 无（Dial 失败即失败） | 可用性差 |
| handshake probe / doctor | 是 | 无 | 排障困难 |
| `client_metadata`（trace/session/turn） | 是 | 无 | 可观测性弱 |
| 代理 / 自定义 CA | 专用 dialer | `http.ProxyFromEnvironment` | 企业环境可能失败 |
| 非流式 `/responses` | HTTP | HTTP 透传 | 一致 |
| Copilot / 自定义 base URL | 有专门 header 合并 | 有 Copilot client，WS header 较少 | 需验证 |

**结论**：Crush “能 WS”仅限于 **把单次 HTTP 流式请求翻译成一次 WS 会话**；尚未实现 Codex 级别的 **Responses WebSocket 产品能力**。

## 3. 设计原则

1. **Fantasy 边界**：事件语义（`response.*` → `fantasy` stream parts）继续落在 `fantasy/providers/openai`；传输选择在 Crush `internal` 或 `fantasy` 的 provider 构造层。
2. **双传输共存**：WS 与 HTTP SSE 必须产出 **相同的** `fantasy` 流抽象，避免改 agent 主循环。
3. **渐进迁移**：先增强 `httpext` 行为与测试，再引入连接池，最后考虑把传输下沉到 `fantasy` 可选 `Transport` 接口。
4. **失败安全**：任何 WS 错误可回退 HTTP；回退粒度建议 **provider 实例 / session**（与 Codex 一致），可配置为 **per-request** 用于调试。
5. **配置向后兼容**：保留 `responses_websocket`；新增细项用可选字段（见 §6）。

## 4. 目标架构（Crush）

### 4.1 分层

```
┌─────────────────────────────────────────────────────────┐
│ internal/agent/coordinator + providers.go               │
│   Provider 构建、Copilot header、responses_websocket 开关   │
└───────────────────────────┬─────────────────────────────┘
                            │
┌───────────────────────────▼─────────────────────────────┐
│ internal/responses/transport (新包，建议)                  │
│   Transport interface: Stream(ctx, Request) (Events, err)│
│   ├─ HTTPTransport      (现有 openai-go NewStreaming)      │
│   └─ WebSocketTransport (Codex 对齐)                     │
│       ├─ Dialer (proxy, TLS, headers)                    │
│       ├─ ConnectionPool / TurnSession                    │
│       └─ EventDecoder → 与 SSE 相同的事件类型              │
└───────────────────────────┬─────────────────────────────┘
                            │
┌───────────────────────────▼─────────────────────────────┐
│ fantasy/providers/openai/responses_language_model.go     │
│   仅消费统一 StreamReader，不关心底层传输                    │
└─────────────────────────────────────────────────────────┘
```

### 4.2 生命周期（对齐 Codex）

| 作用域 | Go 建议挂载点 | 职责 |
|--------|---------------|------|
| Session | `coordinator` 或 per-provider `ResponsesTransportSession` | 缓存 WS connection；`disableWebSocket` 标志 |
| Turn | `agent.SessionAgent` 单次用户消息→assistant 完成 | `turnState` token；`lastResponseID`；prewarm |
| Stream | 单次 `StreamText` / tool loop 内一次 model 调用 | 独占 WS 写锁；idle timeout |

Crush 的 agent 步进在 `fantasy/agent.go`；turn 边界应对齐 **一次用户提交到 assistant 结束**（含 tool 循环），与 Codex `ModelClientSession` 一致。

### 4.3 请求形态

WebSocket 出站 JSON（与 Codex `ResponsesWsRequest` 对齐）：

```json
{
  "type": "response.create",
  "model": "...",
  "input": [...],
  "stream": true,
  "previous_response_id": "resp_...",
  "generate": false,
  "client_metadata": { "session_id": "...", "turn_id": "..." }
}
```

入站：每条 WS Text 为 JSON，`type` 字段与 HTTP SSE event 名一致（如 `response.output_text.delta`），由 **同一套** `processResponsesEvent` 解析（可从现有 SSE 路径抽取）。

## 5. 分阶段实施计划

### Phase 0 — 基线与契约（1–2 天）

- [ ] 在 `docs/` 保留本文；在 `AGENTS.md` 增加一行指向本文（可选）。
- [ ] 盘点 Crush 使用的 Responses 字段：`previous_response_id`、`store`、`include`、reasoning、Copilot headers。
- [ ] 从 Codex 移植/对照测试向量：`scripts/mock_responses_websocket_server.py` → Go `httptest` + `gorilla/websocket`（可放在 `internal/responses/transport/testdata`）。

**验收**：文档评审通过；现有 `go test ./internal/httpext/...` 仍绿。

### Phase 1 — 修正 shim 协议与 header（小改动，高收益）

在 `internal/httpext/openai_responses_ws.go`：

1. **Beta header 策略**
   - 若配置 `responses_websocket_v2: true`（新字段，默认 false）：设置 `OpenAI-Beta: responses_websockets=2026-02-06`（与 Codex 一致）。
   - 保留现有 `responses-api=v1` 作为 v1 模式或 host 默认。
2. **请求体**
   - 保留 `stream` 在 WS `response.create` 内（Codex 保留）；当前 Crush 删除 `stream` 需用集成测试验证官方/代理是否要求。
   - 支持从 HTTP 请求体透传 `previous_response_id`（若 fantasy 已生成）。
3. **错误分类**
   - 识别 `websocket_connection_limit_reached` close reason，错误信息提示“需新建连接”（Codex 常量已有）。

**验收**：扩展 `openai_responses_ws_test.go`；对 mock server 覆盖 v2 header 与 `previous_response_id`。

### Phase 2 — 连接复用与回退（核心）

新增 `internal/responses/transport`：

1. `WebSocketConnection`：`SendResponseCreate` + `ReadEvents` 循环。
2. `SessionStore`：`map[providerKey]*sessionState` 或使用 `sync.Map`。
3. 在 `wrapOpenAIStreamingHTTPClient` **或** provider 构建时注入 **共享** connection（按 `baseURL+apiKey` 键控）。
4. **回退**：WS dial/stream 失败 → 标记 session `preferHTTP=true` → 后续请求走原生 HTTP（不再尝试 WS）。

**验收**：

- 测试：同一 session 两次 stream 仅一次 TCP/TLS 握手（可用计数 hook）。
- 测试：第一次 WS 失败后第二次走 HTTP。

### Phase 3 — Turn 状态与增量（对齐 Codex v2）

1. 在 `internal/agent` 向 transport 传入：
   - `session_id` / `turn_id`（可用 Crush session UUID + message ID）。
   - 从响应头或首帧读取 `x-codex-turn-state`（若代理返回）并在同 turn 重放。
2. 维护 `lastResponseID`：在 `response.completed` 后写入 turn 状态；下次 WS 请求带 `previous_response_id`，input 仅追加增量（需与 `fantasy` 消息转 Responses `input` 对齐——**高风险**，单独设计见 §7）。
3. **Prewarm**（可选）：turn 开始前发 `generate=false` 的 `response.create`，等待 completed 再发真正的 stream。

**验收**：对照 Codex `client_websockets.rs` 中 reuse / prewarm 测试用例移植。

### Phase 4 — Fantasy 解耦（可选，中长期）

- 为 `openai.Provider` 增加 `ResponsesTransport` 依赖注入，默认 HTTP。
- `responses_language_model` 从 `NewStreaming` 改为 `transport.Stream(ctx, ResponsesCall)`。
- 删除 `httpext` shim 或仅保留给未迁移的 `openaicompat` 路径。

**验收**：`go test ./fantasy/providers/openai/...`；agent 集成测试无回归。

### Phase 5 — 可观测性与运维

- 结构化日志：`transport=responses_websocket|http_sse`，`connection_reused`，`fallback`。
- Crush `crush` 工具或 doctor 子命令：WS handshake probe（模仿 `ResponsesWebsocketClient.probe_handshake`）。
- 文档：在 `skill://crush-config` 或 `docs` 说明 `responses_websocket` / v2 / 回退行为。

## 6. 配置提案

在 `ProviderConfig` 扩展（均可选，默认保持现状）：

```json
{
  "providers": {
    "openai": {
      "responses_websocket": true,
      "responses_websocket_v2": true,
      "responses_websocket_fallback": "session",
      "responses_websocket_prewarm": false
    }
  }
}
```

| 字段 | 默认 | 含义 |
|------|------|------|
| `responses_websocket` | `false` | 启用 WS 传输（已有） |
| `responses_websocket_v2` | `false` | 使用 `responses_websockets=2026-02-06` beta |
| `responses_websocket_fallback` | `"session"` | `session` \| `request` \| `off` |
| `responses_websocket_prewarm` | `false` | turn 级 prewarm |

同步更新 `schema.json` 与 `internal/config/load_test.go`。

## 7. 风险与未决问题

### 7.1 `previous_response_id` 与 Crush 消息模型

Crush 通过 fantasy 维护完整对话并每次发送完整 `input` 的可能性高。Codex 在 WS 复用时会 **裁剪增量 input**。需要：

- 审计 `responses_language_model.go` 构建 `input` 的逻辑；
- 若无法安全增量，Phase 3 可仅复用连接 **不带** `previous_response_id`（仍有性能收益，但弱于 Codex）。

### 7.2 Copilot / Azure / 自定义网关

- Copilot Responses 端点是否支持 WS 需实测；shim 当前对非 `api.openai.com` 不强制 beta header。
- 建议在 Phase 1 增加 **provider 能力探测** 或文档声明“仅验证 openai.com 与兼容代理”。

### 7.3 与 `fantasy` 双消息状态的关系

修改流事件处理时须遵守 `docs/pitfalls/fantasy-dual-message-state.md`：WS 与 SSE 路径必须产生一致的 message/part 更新顺序。

### 7.4 并发

同一 connection 上 Codex 用 mutex 串行化 stream。Crush 若多 subagent 共享 provider，需 **每连接互斥** 或 **每 subagent 独立连接**（配置项）。

## 8. 测试策略

| 层级 | 内容 |
|------|------|
| 单元 | WS 帧 ↔ SSE 事件解析一致性；header 合并；URL scheme 转换 |
| 集成 | `httptest` mock WS server（多事件、function_call、reasoning delta） |
| 回归 | `fantasy/providers/openai/openai_test.go` Responses 用例 |
| 手动 | 真实 `api.openai.com` + `responses_websocket: true`；Copilot provider 对比 |

建议新增：`internal/agent/coordinator_test.go` 中 WS wrapper 测试扩展为 transport 包测试。

## 9. 里程碑与时间粗估

| 阶段 | 工作量 | 依赖 |
|------|--------|------|
| Phase 0–1 | 2–4 人日 | 无 |
| Phase 2 | 5–8 人日 | Phase 1 |
| Phase 3 | 8–15 人日 | Phase 2 + fantasy input 审计 |
| Phase 4–5 | 5–10 人日 | Phase 2+ |

**推荐最小可用（MVP）**：Phase 1 + Phase 2（连接复用 + session 回退 + v2 header），不强制 incremental input。

## 10. 参考索引（Codex）

| 主题 | 路径 |
|------|------|
| WS 端点实现 | `codex-rs/codex-api/src/endpoint/responses_websocket.rs` |
| WS 请求类型 | `codex-rs/codex-api/src/common.rs` (`ResponsesWsRequest`) |
| Session/Turn 客户端 | `codex-rs/core/src/client.rs` |
| 集成测试 | `codex-rs/core/tests/suite/client_websockets.rs` |
| Mock server | `scripts/mock_responses_websocket_server.py` |
| Feature flags | `codex-rs/features/src/lib.rs` (`responses_websockets`, `_v2`) |
| 拨号/代理 | `codex-rs/websocket-client/` |

## 11. 参考索引（Crush）

| 主题 | 路径 |
|------|------|
| 配置开关 | `internal/config/config.go` (`ResponsesWebSocket`) |
| Provider 接线 | `internal/agent/providers.go` |
| WS shim | `internal/httpext/openai_responses_ws.go` |
| Responses LM | `fantasy/providers/openai/responses_language_model.go` |
| 双消息状态陷阱 | `docs/pitfalls/fantasy-dual-message-state.md` |

---

*文档版本：2026-03-12*