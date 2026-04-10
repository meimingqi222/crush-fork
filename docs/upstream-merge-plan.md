# 上游仓库合并迁移计划

## 概述

本文档详细记录了将上游 `charmbracelet/crush` 仓库的新架构合并到本地 fork 的完整迁移计划。

### 背景

- **上游仓库**: `charmbracelet/crush`
- **本地 Fork**: `meimingqi222/crush`
- **分叉基准点**: `451faa71` (2026-03-13)
- **本地特色功能**: Memory System, Background Agent, Auto Mode, Plugin System, Checkpoint, Timeline, ToolRuntime, ACP

### 目标

1. 合并上游的 **Workspace/Server/Client** 架构
2. 保留本地所有特色功能
3. 支持未来持续同步上游更新
4. 保持代码可维护性和可扩展性

### 设计决策

| 决策点 | 选择 | 说明 |
|--------|------|------|
| 扩展加载方式 | 编译时加载 | 简单可靠 |
| Client/Server 模式扩展支持 | 支持远程调用 | 完整功能 |
| 迁移策略 | 先完成架构，再迁移功能 | 稳健可控 |
| 本地功能保留 | 全部保留 | Memory, Background, Auto, Plugin, Checkpoint, Timeline, ToolRuntime, ACP |

---

## 架构设计

### 三层架构

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          三层架构设计                                     │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  ┌───────────────────────────────────────────────────────────────────┐  │
│  │  Layer 1: Upstream Core (上游核心层)                               │  │
│  │  - workspace/, server/, client/, backend/, proto/                 │  │
│  │  - 简化的 agent/, coordinator/                                     │  │
│  │  - 保持与上游同步，最小化本地修改                                    │  │
│  └───────────────────────────────────────────────────────────────────┘  │
│                              ↓ 接口扩展                                   │
│  ┌───────────────────────────────────────────────────────────────────┐  │
│  │  Layer 2: Extension Interfaces (扩展接口层)                        │  │
│  │  - internal/ext/interfaces/                                        │  │
│  │  - 定义本地服务的接口                                              │  │
│  │  - 定义 Plugin Hook 接口                                           │  │
│  └───────────────────────────────────────────────────────────────────┘  │
│                              ↓ 具体实现                                   │
│  ┌───────────────────────────────────────────────────────────────────┐  │
│  │  Layer 3: Local Extensions (本地扩展层)                            │  │
│  │  - internal/ext/memory/                                            │  │
│  │  - internal/ext/background/                                        │  │
│  │  - internal/ext/auto/                                              │  │
│  │  - internal/ext/plugin/                                            │  │
│  │  - internal/ext/checkpoint/                                        │  │
│  │  - internal/ext/timeline/                                          │  │
│  │  - internal/ext/toolruntime/                                       │  │
│  │  - internal/ext/acp/                                               │  │
│  └───────────────────────────────────────────────────────────────────┘  │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘
```

### 完整架构图

```
Frontend (TUI/CLI)
┌─────────────────────────────────────────────────────────────────────┐
│  Workspace Interface (扩展版)                                        │
│  ├── 基础方法 (上游)                                                 │
│  │   CreateSession, ListMessages, AgentRun, LSPStart, ...          │
│  │                                                                   │
│  └── 扩展方法 (本地)                                                 │
│      Memory(), BackgroundAgent(), AutoMode(), Plugin(),             │
│      Checkpoint(), Timeline(), ToolRuntime()                         │
└─────────────────────────────────────────────────────────────────────┘
                             │
         ┌───────────────────┴───────────────────┐
         ▼                                       ▼
┌───────────────────────┐          ┌───────────────────────────┐
│   AppWorkspace        │          │   ClientWorkspace         │
│   (本地模式)           │          │   (远程模式)               │
│                       │          │                           │
│  直接调用本地服务       │          │  通过 RPC 调用远程服务     │
│  ├── MemoryService    │          │  ├── MemoryClient         │
│  ├── BackgroundAgent  │          │  ├── BackgroundClient     │
│  ├── AutoModeService  │          │  ├── AutoModeClient       │
│  └── PluginService    │          │  └── PluginClient         │
└───────────────────────┘          └───────────────────────────┘
         │                                       │
         │                                       │
         ▼                                       ▼
┌───────────────────────┐          ┌───────────────────────────┐
│   Local Extensions    │          │   Crush Server            │
│   (internal/ext/...)  │          │                           │
│                       │          │   ┌─────────────────────┐ │
│  ├── memory/          │          │   │ Backend             │ │
│  ├── background/      │◄─────────┼───┤ - 扩展方法路由      │ │
│  ├── auto/            │   RPC    │   │ - 调用本地扩展      │ │
│  ├── plugin/          │          │   └─────────────────────┘ │
│  ├── checkpoint/      │          │                           │
│  ├── timeline/        │          │   Local Extensions        │
│  └── acp/             │          │   (同左侧)                 │
└───────────────────────┘          └───────────────────────────┘
```

---

## 目录结构

### 最终结构

```
internal/
├────────────────────────────────────────────────────────────────────
│ Layer 1: Upstream Core (上游核心层 - 与上游保持同步)
├────────────────────────────────────────────────────────────────────
│   workspace/
│   │   workspace.go              # 上游接口定义
│   │   app_workspace.go          # 上游实现
│   │   client_workspace.go       # 上游实现
│   │   extended.go               # 【本地新增】ExtendedWorkspace wrapper
│   │
│   server/
│   │   server.go                 # 上游 RPC 服务器
│   │   proto.go                  # 上游协议处理
│   │   extensions.go             # 【本地新增】扩展方法处理
│   │
│   client/
│   │   client.go                 # 上游 RPC 客户端
│   │   extensions.go             # 【本地新增】扩展方法客户端
│   │
│   backend/
│   │   backend.go                # 上游后端逻辑
│   │   extensions.go             # 【本地新增】扩展路由
│   │
│   proto/
│   │   proto.go                  # 上游基础协议
│   │   extensions.go             # 【本地新增】扩展协议定义
│   │   memory.go                 # 【本地新增】Memory RPC 类型
│   │   background.go             # 【本地新增】Background RPC 类型
│   │   auto.go                   # 【本地新增】Auto Mode RPC 类型
│   │   plugin.go                 # 【本地新增】Plugin RPC 类型
│   │
│   agent/
│   │   agent.go                  # 上游简化核心 + 【本地】扩展点调用
│   │   coordinator.go            # 上游简化核心 + 【本地】扩展点调用
│   │   extension_points.go       # 【本地新增】扩展点定义
│   │
│   app/
│   │   app.go                    # 上游简化结构 + 【本地】扩展服务字段
│   │   extensions.go             # 【本地新增】扩展服务注册
│   │
│   skills/                       # 上游新增
│
├────────────────────────────────────────────────────────────────────
│ Layer 2: Extension Interfaces (扩展接口层 - 稳定接口定义)
├────────────────────────────────────────────────────────────────────
│   ext/
│   │   registry.go               # 扩展注册中心
│   │   context.go                # ExtensionContext
│   │   interfaces.go             # 基础接口
│   │
│   ├── interfaces/
│   │   ├── memory.go             # MemoryService 接口
│   │   ├── background.go         # BackgroundAgentService 接口
│   │   ├── auto.go               # AutoModeService 接口
│   │   ├── plugin.go             # PluginService 接口
│   │   ├── checkpoint.go         # CheckpointService 接口
│   │   ├── timeline.go           # TimelineService 接口
│   │   ├── toolruntime.go        # ToolRuntimeService 接口
│   │   └── acp.go                # ACPService 接口
│   │
│   └── hooks/
│       ├── agent_hooks.go        # Agent 生命周期钩子
│       ├── tool_hooks.go         # Tool 执行钩子
│       ├── chat_hooks.go         # Chat 转换钩子
│       └── permission_hooks.go   # Permission 钩子
│
├────────────────────────────────────────────────────────────────────
│ Layer 3: Extension Implementations (扩展实现层 - 本地功能)
├────────────────────────────────────────────────────────────────────
│   ext/
│   ├── memory/
│   │   ├── service.go            # 从 internal/memory 迁移
│   │   ├── dream.go              # 从 internal/agent/memory_dream.go 迁移
│   │   ├── recall.go             # 从 internal/agent/memory_recall.go 迁移
│   │   └── service_test.go
│   │
│   ├── background/
│   │   ├── registry.go           # 从 internal/agent/background_agent.go 提取
│   │   ├── runner.go
│   │   ├── escalation.go         # 从 internal/agent/escalation_ui.go 迁移
│   │   └── mailbox.go            # 从 internal/agent/mailbox/ 迁移
│   │
│   ├── auto/
│   │   ├── classifier.go         # 从 internal/agent/auto_classifier.go 迁移
│   │   ├── guard.go              # 从 internal/agent/auto_guard.go 迁移
│   │   ├── reminder.go           # 从 internal/agent/auto_mode_reminder.go 迁移
│   │   └── autopermission.go     # 从 internal/autopermission/ 迁移
│   │
│   ├── plugin/
│   │   ├── manager.go            # 从 internal/plugin/ 迁移
│   │   ├── hooks.go
│   │   ├── chat_transform.go     # 从 internal/agent/chat_transform.go 迁移
│   │   └── compact_builtin.go    # 从 internal/agent/compact_builtin.go 迁移
│   │
│   ├── checkpoint/
│   │   └── service.go            # 从 internal/checkpoint/ 迁移
│   │
│   ├── timeline/
│   │   └── service.go            # 从 internal/timeline/ 迁移
│   │
│   ├── toolruntime/
│   │   └── service.go            # 从 internal/toolruntime/ 迁移
│   │
│   └── acp/
│       ├── server.go             # 从 internal/acp/ 迁移
│       ├── client.go
│       ├── handler.go
│       └── types.go
```

---

## 核心接口设计

### 1. Extension Registry (扩展注册中心)

```go
// internal/ext/registry.go

package ext

import (
    "context"
    "github.com/charmbracelet/crush/internal/config"
)

// ExtensionContext provides context for extension initialization.
type ExtensionContext struct {
    Config     *config.ConfigStore
    WorkingDir string
    // 基础服务（上游提供）
    Sessions   session.Service
    Messages   message.Service
    History    history.Service
}

// Extension is the base interface for all local extensions.
type Extension interface {
    // Name returns the extension's unique identifier.
    Name() string
    
    // Initialize is called during app startup.
    Initialize(ctx context.Context, extCtx ExtensionContext) error
    
    // Shutdown is called during app shutdown.
    Shutdown(ctx context.Context) error
}

// Registry manages all registered extensions.
type Registry struct {
    extensions map[string]Extension
    hooks      *Hooks
}

func NewRegistry() *Registry {
    return &Registry{
        extensions: make(map[string]Extension),
        hooks:      NewHooks(),
    }
}

func (r *Registry) Register(ext Extension) error {
    if _, exists := r.extensions[ext.Name()]; exists {
        return fmt.Errorf("extension %s already registered", ext.Name())
    }
    r.extensions[ext.Name()] = ext
    return nil
}
```

### 2. Workspace Extension Interface

```go
// internal/ext/interfaces/workspace_extension.go

package interfaces

import (
    "context"
    "github.com/charmbracelet/crush/internal/workspace"
)

// WorkspaceExtension allows extensions to add methods to Workspace.
type WorkspaceExtension interface {
    Extension
    
    // ExtendWorkspace adds extension methods to a workspace.
    ExtendWorkspace(ws workspace.Workspace) workspace.Workspace
}
```

### 3. Memory Extension Interface

```go
// internal/ext/interfaces/memory.go

package interfaces

import (
    "context"
    "time"
)

// MemoryService defines the long-term memory interface.
type MemoryService interface {
    // Core operations
    Store(ctx context.Context, params StoreParams) error
    Get(ctx context.Context, key string) (Entry, error)
    Delete(ctx context.Context, key string) error
    Search(ctx context.Context, params SearchParams) ([]Entry, error)
    List(ctx context.Context, params ListParams) ([]Entry, error)
    
    // Dream consolidation (advanced feature)
    DreamConsolidate(ctx context.Context, sessionID string) error
    GetLastConsolidatedAt() (time.Time, error)
    
    // Memory recall for agent context
    RecallRelevant(ctx context.Context, query string, limit int) ([]Entry, error)
}
```

### 4. Background Agent Extension Interface

```go
// internal/ext/interfaces/background.go

package interfaces

import (
    "context"
)

// BackgroundAgentService manages background agent execution.
type BackgroundAgentService interface {
    // Spawn creates a new background agent.
    Spawn(ctx context.Context, params SpawnParams) (AgentInfo, error)
    
    // Get retrieves agent info by ID or name.
    Get(agentID string) (AgentInfo, error)
    GetByName(name string) (AgentInfo, error)
    
    // List returns all background agents.
    List() []AgentInfo
    
    // Cancel stops a running agent.
    Cancel(agentID string) error
    
    // WaitForCompletion blocks until agent completes.
    WaitForCompletion(ctx context.Context, agentID string) (AgentResult, error)
    
    // Escalate promotes background agent to foreground.
    Escalate(ctx context.Context, agentID string) error
}
```

### 5. Auto Mode Extension Interface

```go
// internal/ext/interfaces/auto.go

package interfaces

import (
    "context"
    "github.com/charmbracelet/crush/internal/permission"
)

// AutoModeService provides intelligent permission classification.
type AutoModeService interface {
    // ClassifyPermission determines if a permission request can be auto-approved.
    ClassifyPermission(ctx context.Context, req permission.PermissionRequest) (AutoClassification, error)
    
    // ShouldAutoApprove returns true if the request should be auto-approved.
    ShouldAutoApprove(ctx context.Context, req permission.PermissionRequest) bool
    
    // QuickGuard performs fast path approval checks.
    QuickGuard(ctx context.Context, req permission.PermissionRequest) (GuardDecision, error)
}
```

### 6. Agent Hooks Interface

```go
// internal/ext/hooks/agent_hooks.go

package hooks

import (
    "context"
    "charm.land/fantasy"
    "github.com/charmbracelet/crush/internal/message"
)

// AgentHooks provides lifecycle hooks for agent execution.
type AgentHooks interface {
    // BeforeRun is called before agent starts execution.
    BeforeRun(ctx context.Context, input AgentRunInput) error
    
    // AfterRun is called after agent completes execution.
    AfterRun(ctx context.Context, output AgentRunOutput) error
    
    // BeforeToolExecute is called before a tool executes.
    BeforeToolExecute(ctx context.Context, input ToolExecuteInput) (*ToolExecuteDecision, error)
    
    // AfterToolExecute is called after a tool executes.
    AfterToolExecute(ctx context.Context, output ToolExecuteOutput) (*ToolExecuteResult, error)
    
    // TransformMessages allows modifying messages before sending to LLM.
    TransformMessages(ctx context.Context, input TransformMessagesInput) ([]message.Message, error)
    
    // OnContextWindowError is called when context window is exceeded.
    OnContextWindowError(ctx context.Context, input ContextWindowErrorInput) (*ContextWindowRecovery, error)
}
```

---

## 详细实施计划

### 第 1 周：基础架构搭建 (Phase 0-1)

#### Day 1-2: 创建扩展框架

**任务清单:**

- [ ] 创建目录结构
  ```bash
  mkdir -p internal/ext/{interfaces,hooks}
  mkdir -p internal/ext/{memory,background,auto,plugin,checkpoint,timeline,toolruntime,acp}
  ```

- [ ] 创建扩展注册中心
  - 文件: `internal/ext/registry.go`
  - 内容: Registry 结构体, Register(), Initialize(), Shutdown() 方法

- [ ] 创建扩展上下文
  - 文件: `internal/ext/context.go`
  - 内容: ExtensionContext 结构体

- [ ] 创建基础接口
  - 文件: `internal/ext/interfaces.go`
  - 内容: Extension 接口, ExtensionWithHooks 接口

**验证:**
```bash
go build ./internal/ext/...
go test ./internal/ext/...
```

---

#### Day 3-4: 定义服务接口

**任务清单:**

- [ ] Memory 接口
  - 文件: `internal/ext/interfaces/memory.go`
  - 内容: MemoryService 接口定义, StoreParams, SearchParams, Entry 等类型

- [ ] Background Agent 接口
  - 文件: `internal/ext/interfaces/background.go`
  - 内容: BackgroundAgentService 接口定义, SpawnParams, AgentInfo, AgentResult 类型

- [ ] Auto Mode 接口
  - 文件: `internal/ext/interfaces/auto.go`
  - 内容: AutoModeService 接口定义, AutoClassification, GuardDecision 类型

- [ ] Plugin 接口
  - 文件: `internal/ext/interfaces/plugin.go`
  - 内容: PluginService 接口定义, Hooks 结构体

- [ ] Checkpoint 接口
  - 文件: `internal/ext/interfaces/checkpoint.go`

- [ ] Timeline 接口
  - 文件: `internal/ext/interfaces/timeline.go`

- [ ] ToolRuntime 接口
  - 文件: `internal/ext/interfaces/toolruntime.go`

- [ ] ACP 接口
  - 文件: `internal/ext/interfaces/acp.go`

**验证:**
```bash
go build ./internal/ext/interfaces/...
```

---

#### Day 5: 实现 Hook 系统

**任务清单:**

- [ ] Agent Hooks
  - 文件: `internal/ext/hooks/agent_hooks.go`
  - 内容: AgentHooks 接口, BeforeRun, AfterRun, TransformMessages 等方法

- [ ] Tool Hooks
  - 文件: `internal/ext/hooks/tool_hooks.go`
  - 内容: ToolHooks 接口, BeforeExecute, AfterExecute 方法

- [ ] Chat Hooks
  - 文件: `internal/ext/hooks/chat_hooks.go`
  - 内容: ChatHooks 接口, TransformMessages, TransformSystem 方法

- [ ] Permission Hooks
  - 文件: `internal/ext/hooks/permission_hooks.go`
  - 内容: PermissionHooks 接口, Ask, Classify 方法

**验证:**
```bash
go build ./internal/ext/hooks/...
# 编写 hook 测试
```

---

### 第 2 周：导入上游架构 (Phase 1)

#### Day 6-7: 合并上游核心包

**任务清单:**

- [ ] 合并 workspace 包
  ```bash
  git merge upstream/main --no-commit
  # 解决 internal/workspace/ 冲突
  ```

- [ ] 合并 server 包
  - 解决 `internal/server/` 冲突

- [ ] 合并 client 包
  - 解决 `internal/client/` 冲突

- [ ] 合并 backend 包
  - 解决 `internal/backend/` 冲突

- [ ] 合并 proto 包
  - 解决 `internal/proto/` 冲突

- [ ] 合并 skills 包
  - 解决 `internal/skills/` 冲突

**验证:**
```bash
go build ./internal/workspace/...
go build ./internal/server/...
go build ./internal/client/...
go build ./internal/backend/...
go build ./internal/proto/...
```

---

#### Day 8-9: 扩展 Workspace 接口

**任务清单:**

- [ ] 扩展 Workspace 接口
  - 文件: `internal/workspace/workspace.go`
  - 添加扩展方法:
    - `Memory() ext.MemoryService`
    - `BackgroundAgent() ext.BackgroundAgentService`
    - `AutoMode() ext.AutoModeService`
    - `Plugin() ext.PluginService`
    - `Checkpoint() ext.CheckpointService`
    - `Timeline() ext.TimelineService`
    - `ToolRuntime() ext.ToolRuntimeService`
    - `HasExtension(name string) bool`

- [ ] 创建 ExtendedWorkspace
  - 文件: `internal/workspace/extended.go`
  - 内容: ExtendedWorkspace 结构体, 包装 base Workspace + extensions

- [ ] 扩展 AppWorkspace
  - 文件: `internal/workspace/app_workspace.go`
  - 实现扩展方法 (调用本地服务)

**验证:**
```bash
go build ./internal/workspace/...
# 编写 Workspace 测试
```

---

#### Day 10: 定义扩展协议

**任务清单:**

- [ ] 定义扩展协议类型
  - 文件: `internal/proto/extensions.go`
  - 内容: ExtensionMethodRequest/Response 基础类型

- [ ] 定义 Memory RPC 类型
  - 文件: `internal/proto/memory.go`
  - 内容: MemoryStoreRequest, MemorySearchRequest, MemoryEntry, MemoryListResponse 等

- [ ] 定义 Background Agent RPC 类型
  - 文件: `internal/proto/background.go`
  - 内容: BackgroundSpawnRequest, BackgroundAgentInfo 等

- [ ] 定义 Auto Mode RPC 类型
  - 文件: `internal/proto/auto.go`
  - 内容: AutoClassifyRequest, AutoClassificationResponse 等

- [ ] 定义 Plugin RPC 类型
  - 文件: `internal/proto/plugin.go`

- [ ] 定义其他扩展 RPC 类型
  - 文件: `internal/proto/checkpoint.go`
  - 文件: `internal/proto/timeline.go`
  - 文件: `internal/proto/toolruntime.go`

**验证:**
```bash
go build ./internal/proto/...
```

---

### 第 3 周：迁移本地扩展 (Phase 2 - Part 1)

#### Day 11-13: Memory Service 迁移

**任务清单:**

- [ ] 迁移基础服务
  ```bash
  cp internal/memory/service.go internal/ext/memory/service.go
  # 修改实现 MemoryService 接口
  ```

- [ ] 迁移 Memory Dream
  - 提取 `internal/agent/memory_dream.go`
  - 目标: `internal/ext/memory/dream.go`

- [ ] 迁移 Memory Recall
  - 提取 `internal/agent/memory_recall.go`
  - 目标: `internal/ext/memory/recall.go`

- [ ] 实现 Extension 接口
  - 文件: `internal/ext/memory/service.go`
  - 添加 Name(), Initialize(), Shutdown() 方法

- [ ] 集成到 AppWorkspace
  - 文件: `internal/workspace/app_workspace.go`
  - 在 AppWorkspace 中添加 memory 字段
  - 实现 Memory() 方法

- [ ] 实现 ClientWorkspace RPC 调用
  - 文件: `internal/client/extensions.go`
  - 创建 MemoryClient
  - 实现 Memory() 方法

- [ ] 实现 Server 扩展处理
  - 文件: `internal/server/extensions.go`
  - 添加 Memory 方法路由

**验证:**
```bash
go test ./internal/ext/memory/...
go test ./internal/workspace/...
# 端到端测试 Memory 功能
```

---

#### Day 14-16: Background Agent 迁移

**任务清单:**

- [ ] 提取核心逻辑
  - 提取 `internal/agent/background_agent.go`
  - 目标:
    - `internal/ext/background/registry.go`
    - `internal/ext/background/runner.go`

- [ ] 迁移 Escalation UI
  - 提取 `internal/agent/escalation_ui.go`
  - 目标: `internal/ext/background/escalation.go`

- [ ] 迁移 Mailbox
  - 提取 `internal/agent/mailbox/`
  - 目标: `internal/ext/background/mailbox/`

- [ ] 实现 BackgroundAgentService 接口
  - 文件: `internal/ext/background/registry.go`
  - 实现 Spawn(), Get(), List(), Cancel() 等

- [ ] 实现 Extension 接口
  - 添加 Initialize(), Shutdown()

- [ ] 集成到 Workspace
  - 文件: `internal/workspace/app_workspace.go`
  - 文件: `internal/workspace/client_workspace.go`
  - 文件: `internal/client/extensions.go`
  - 文件: `internal/server/extensions.go`

**验证:**
```bash
go test ./internal/ext/background/...
# 端到端测试 Background Agent
```

---

#### Day 17: Checkpoint & Timeline & ToolRuntime 迁移

**任务清单:**

- [ ] Checkpoint 迁移
  ```bash
  cp internal/checkpoint/service.go internal/ext/checkpoint/service.go
  # 实现 CheckpointService 接口
  # 集成到 Workspace
  ```

- [ ] Timeline 迁移
  ```bash
  cp internal/timeline/service.go internal/ext/timeline/service.go
  # 实现 TimelineService 接口
  # 集成到 Workspace
  ```

- [ ] ToolRuntime 迁移
  ```bash
  cp internal/toolruntime/service.go internal/ext/toolruntime/service.go
  # 实现 ToolRuntimeService 接口
  # 集成到 Workspace
  ```

**验证:**
```bash
go test ./internal/ext/checkpoint/...
go test ./internal/ext/timeline/...
go test ./internal/ext/toolruntime/...
```

---

### 第 4 周：迁移本地扩展 (Phase 2 - Part 2)

#### Day 18-20: Auto Mode 迁移

**任务清单:**

- [ ] 迁移 Auto Classifier
  - 提取 `internal/agent/auto_classifier.go`
  - 目标: `internal/ext/auto/classifier.go`

- [ ] 迁移 Auto Guard
  - 提取 `internal/agent/auto_guard.go`
  - 目标: `internal/ext/auto/guard.go`

- [ ] 迁移 Auto Mode Reminder
  - 提取 `internal/agent/auto_mode_reminder.go`
  - 目标: `internal/ext/auto/reminder.go`

- [ ] 迁移 Auto Permission
  - 提取 `internal/autopermission/service.go`
  - 目标: `internal/ext/auto/autopermission.go`

- [ ] 实现 AutoModeService 接口
  - 文件: `internal/ext/auto/service.go`
  - 实现 ClassifyPermission(), ShouldAutoApprove() 等

- [ ] 实现 PermissionHooks
  - 文件: `internal/ext/auto/hooks.go`
  - 实现 PermissionAsk hook

- [ ] 集成到 Coordinator
  - 文件: `internal/agent/coordinator.go`
  - 在 coordinator 中添加 autoMode 字段
  - 在权限检查时调用 auto classifier

**验证:**
```bash
go test ./internal/ext/auto/...
# 端到端测试 Auto Mode
```

---

#### Day 21-23: Plugin System 迁移

**任务清单:**

- [ ] 迁移 Plugin Manager
  ```bash
  cp internal/plugin/manager.go internal/ext/plugin/manager.go
  cp internal/plugin/plugin.go internal/ext/plugin/hooks.go
  ```

- [ ] 迁移 Chat Transform
  - 提取 `internal/agent/chat_transform.go`
  - 目标: `internal/ext/plugin/chat_transform.go`

- [ ] 迁移 Compact Builtin
  - 提取 `internal/agent/compact_builtin.go`
  - 目标: `internal/ext/plugin/compact_builtin.go`

- [ ] 迁移 Local Tools
  ```bash
  cp internal/plugin/local_tools.go internal/ext/plugin/local_tools.go
  ```

- [ ] 实现 PluginService 接口
  - 文件: `internal/ext/plugin/service.go`
  - 实现 RegisterHooks(), ExecuteHook() 等

- [ ] 集成 Chat Hooks
  - 将 plugin.Hooks 适配到 ext/hooks/ 接口

- [ ] 集成到 Agent
  - 文件: `internal/agent/agent.go`
  - 在 sessionAgent 中添加 pluginHooks 字段
  - 在关键位置调用 hooks

**验证:**
```bash
go test ./internal/ext/plugin/...
# 端到端测试 Plugin 功能
```

---

#### Day 24: ACP 迁移

**任务清单:**

- [ ] 迁移 ACP 包
  ```bash
  mv internal/acp/* internal/ext/acp/
  ```

- [ ] 实现 ACPService 接口
  - 文件: `internal/ext/acp/service.go`

- [ ] 集成到 Workspace
  - 文件: `internal/workspace/app_workspace.go`
  - 实现 ACP() 方法

**验证:**
```bash
go test ./internal/ext/acp/...
```

---

### 第 5 周：合并核心与测试 (Phase 3-5)

#### Day 25-27: 合并上游 Agent 核心

**任务清单:**

- [ ] 分析上游变更
  ```bash
  git diff upstream/main HEAD -- internal/agent/agent.go
  git diff upstream/main HEAD -- internal/agent/coordinator.go
  # 确定哪些逻辑需要保留
  ```

- [ ] 合并 agent.go
  - 接受上游简化版本
  - 添加扩展点调用:
    - 在 Run() 开头调用 BeforeRun hook
    - 在 Run() 结尾调用 AfterRun hook
    - 在 preparePrompt() 中调用 TransformMessages hook
    - 在工具执行时调用 ToolHooks

- [ ] 合并 coordinator.go
  - 接受上游简化版本
  - 添加扩展点:
    - 添加 `extensions *ext.Registry` 字段
    - 在 buildAgent() 中注入 hooks
    - 在权限处理时调用 AutoMode hook

- [ ] 创建 extension_points.go
  - 文件: `internal/agent/extension_points.go`
  - 定义扩展点调用逻辑

- [ ] 迁移其他高级功能
  - `context_window.go` → `internal/ext/context/` (或保留)
  - `delegation.go` → `internal/ext/delegation/`

**验证:**
```bash
go build ./internal/agent/...
go test ./internal/agent/...
```

---

#### Day 28-29: UI 层迁移

**任务清单:**

- [ ] 合并 UI 重构
  ```bash
  git merge upstream/main -- internal/ui/
  # 解决冲突
  ```

- [ ] 适配 Workspace 接口
  - 修改 UI 代码通过 Workspace 接口访问服务
  - 不再直接依赖 app.App

- [ ] 保留本地 UI 组件
  - Timeline View: 通过 `Workspace.Timeline()` 访问
  - Memory Dream UI: 通过 `Workspace.Memory()` 访问
  - Background Agent UI: 通过 `Workspace.BackgroundAgent()` 访问

**验证:**
```bash
go build ./internal/ui/...
go test ./internal/ui/...
```

---

#### Day 30-31: 集成测试

**任务清单:**

- [ ] 功能测试
  - Memory 系统端到端测试
  - Background Agent 端到端测试
  - Auto Mode 端到端测试
  - Plugin 系统端到端测试
  - Client/Server 模式测试

- [ ] 性能测试
  - 内存使用测试
  - 响应时间测试
  - 并发测试

- [ ] 边界测试
  - 大规模数据测试
  - 错误处理测试
  - 超时测试

**验证:**
```bash
go test ./... -timeout 10m
# 手动功能验证
```

---

#### Day 32-33: 文档与清理

**任务清单:**

- [ ] 更新 AGENTS.md
  - 新架构说明
  - 扩展开发指南

- [ ] 创建扩展开发文档
  - 文件: `docs/extension-development.md`
  - 内容:
    - 如何创建新扩展
    - 如何实现接口
    - 如何注册扩展

- [ ] 清理废弃代码
  ```bash
  # 删除已迁移的目录
  rm -rf internal/memory/
  rm -rf internal/autopermission/
  rm -rf internal/checkpoint/
  rm -rf internal/timeline/
  rm -rf internal/toolruntime/
  rm -rf internal/acp/
  # 清理 internal/agent/ 中已迁移的文件
  ```

- [ ] 代码审查
  - 检查代码风格
  - 检查注释完整性
  - 检查测试覆盖率

**验证:**
```bash
go build ./...
go test ./...
go vet ./...
```

---

## 工作量总结

| 周 | 任务 | 工作量 | 风险 | 产出 |
|----|------|--------|------|------|
| 第 1 周 | 基础架构搭建 | 5 天 | 🟢 低 | 扩展框架就绪 |
| 第 2 周 | 导入上游架构 | 5 天 | 🟡 中 | Workspace 架构就绪 + Proto 定义 |
| 第 3 周 | 迁移扩展 (Part 1) | 5 天 | 🟡 中 | Memory, Background, Checkpoint 等迁移 |
| 第 4 周 | 迁移扩展 (Part 2) | 5 天 | 🟡 中 | Auto, Plugin, ACP 迁移 |
| 第 5 周 | 合并核心与测试 | 6 天 | 🔴 高 | Agent 核心 + UI 更新 + 测试 |
| **总计** | | **26 天** | | |

---

## 关键里程碑

```
Week 1 End: ✅ 扩展框架就绪
           ✅ 所有接口定义完成
           ✅ Hook 系统就绪

Week 2 End: ✅ 上游架构合并完成
           ✅ Workspace 接口扩展完成
           ✅ Proto 定义完成

Week 3 End: ✅ Memory 迁移完成
           ✅ Background Agent 迁移完成
           ✅ Checkpoint/Timeline/ToolRuntime 迁移完成

Week 4 End: ✅ Auto Mode 迁移完成
           ✅ Plugin 系统迁移完成
           ✅ ACP 迁移完成

Week 5 End: ✅ Agent 核心合并完成
           ✅ UI 层迁移完成
           ✅ 全面测试通过
           ✅ 文档更新完成
```

---

## 未来合并上游更新的流程

### 定期同步流程

```bash
# 1. 获取上游更新
git fetch upstream

# 2. 查看变更
git log HEAD..upstream/main --oneline
git diff HEAD upstream/main --stat

# 3. 评估冲突
# Layer 1 (核心层): 可能需要手动合并
# Layer 2 (接口层): 通常无冲突
# Layer 3 (扩展层): 通常无冲突

# 4. 执行合并
git merge upstream/main

# 5. 解决冲突 (主要在 Layer 1)
# - agent.go: 保留扩展点调用
# - coordinator.go: 保留扩展点调用
# - workspace/: 可能需要扩展接口
# - proto/: 可能需要添加新类型

# 6. 测试验证
go test ./...

# 7. 提交
git commit
```

### 冲突解决策略

| 文件 | 冲突可能性 | 解决策略 |
|------|-----------|----------|
| `agent/agent.go` | 🔴 高 | 保留扩展点调用代码 |
| `agent/coordinator.go` | 🔴 高 | 保留扩展点调用代码 |
| `workspace/workspace.go` | 🟡 中 | 保留扩展方法定义 |
| `server/server.go` | 🟡 中 | 保留扩展路由代码 |
| `client/client.go` | 🟡 中 | 保留扩展客户端代码 |
| `proto/*.go` | 🟡 中 | 保留扩展协议定义 |
| `app/app.go` | 🟡 中 | 保留扩展服务字段 |
| `ext/**` | 🟢 低 | 无冲突 |

---

## 风险与缓解

| 风险 | 可能性 | 影响 | 缓解措施 |
|------|--------|------|----------|
| 上游大幅重构 Agent | 中 | 高 | 保持扩展点设计灵活 |
| 接口定义需要调整 | 低 | 中 | 使用接口版本化 |
| 性能退化 | 低 | 中 | 每阶段进行性能测试 |
| Client/Server 扩展 RPC 复杂 | 中 | 中 | 优先实现本地模式，RPC 作为增强 |

---

## 附录

### A. 迁移前后对比

#### 迁移前 (当前 Fork)

```
internal/
├── memory/          # 独立服务
├── autopermission/  # 独立服务
├── checkpoint/      # 独立服务
├── timeline/        # 独立服务
├── toolruntime/     # 独立服务
├── acp/             # 独立协议
├── plugin/          # 独立系统
└── agent/
    ├── memory_dream.go      # 混合在 agent 中
    ├── memory_recall.go     # 混合在 agent 中
    ├── background_agent.go  # 混合在 agent 中
    ├── auto_classifier.go   # 混合在 agent 中
    ├── auto_guard.go        # 混合在 agent 中
    └── ...
```

#### 迁移后 (新架构)

```
internal/
├── workspace/       # 上游新架构
├── server/          # 上游新架构
├── client/          # 上游新架构
├── backend/         # 上游新架构
├── proto/           # 上游新架构 + 扩展协议
├── skills/          # 上游新增
│
├── agent/           # 上游简化核心 + 扩展点
│   ├── agent.go
│   ├── coordinator.go
│   └── extension_points.go  # 本地扩展点
│
├── app/             # 上游简化 + 扩展服务注册
│   ├── app.go
│   └── extensions.go
│
└── ext/             # 本地扩展层 (清晰分层)
    ├── interfaces/  # 稳定接口定义
    ├── hooks/       # 生命周期钩子
    ├── memory/      # Memory 服务
    ├── background/  # Background Agent
    ├── auto/        # Auto Mode
    ├── plugin/      # Plugin System
    ├── checkpoint/  # Checkpoint
    ├── timeline/    # Timeline
    ├── toolruntime/ # ToolRuntime
    └── acp/         # ACP Protocol
```

### B. 扩展开发示例

参见: `docs/extension-development.md` (待创建)

### C. 参考资料

- 上游仓库: https://github.com/charmbracelet/crush
- 本地 Fork: https://github.com/meimingqi222/crush
- 分叉基准: commit `451faa71` (2026-03-13)

---

**文档版本**: v1.0  
**创建日期**: 2026-04-10  
**最后更新**: 2026-04-10  
**维护者**: Crush Fork Team
