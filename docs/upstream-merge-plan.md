# 上游迁移计划（Release 驱动）

## 概述

本文档记录当前分支对上游 `charmbracelet/crush` 的迁移策略。

结论已经明确：

- 不再按“整包接受 upstream 的 Workspace/Server/Client 架构”推进。
- 当前主线改为“按 release 做高价值增量移植”。
- 优先移植稳定性、兼容性、可观测性和高价值 TUI 改进。
- 长周期的大架构统一保留为 RFC，不作为当前执行计划。

## 背景

- 上游仓库：`charmbracelet/crush`
- 本地 Fork：`meimingqi222/crush-fork`
- 当前分析范围：`v0.49.0` 到 `v0.56.0`
- 本地分支保留的核心能力：
  - Memory
  - Background Agent
  - Auto Mode
  - Plugin System
  - Checkpoint
  - Timeline
  - ToolRuntime
  - ACP

## 迁移原则

### 1. 先移植单点高价值补丁

优先选择满足以下条件的变更：

- 冲突面小
- 测试容易补齐
- 对当前 fork 的收益直接
- 不要求先重构 `agent/coordinator/app/ui` 的整体结构

### 2. 不接受高风险整包重构

以下方向暂不作为近期主线：

- 一次性迁入 upstream 的 `Workspace/Server/Client`
- 为了贴近上游而重写本地 `agent/coordinator`
- 为架构一致性而大规模搬迁本地扩展层

原因：

- 当前 fork 已在 `agent/coordinator/app/ui` 上发生深度分叉。
- 整包迁移会把“功能收益”变成“长期合并成本”。
- 与其引入大面积回归，不如逐个吸收上游已验证的改进。

### 3. 每个阶段都必须可验证

每次移植至少满足以下一项：

- 有新增或更新测试
- 有明确的包级验证命令
- 可以通过 `go test ./...` 回归

## 当前判断

### 现在不该做什么

- 不该继续沿用旧文档中的“五周整包架构迁移”路线。
- 不该把 `Workspace/Server/Client` 视为当前必须完成项。
- 不该为迁移而迁移 skills/server/client 分层。

### 现在应该做什么

- 以 release 为单位梳理 `v0.49.0` 到 `v0.56.0`
- 优先移植稳定性补丁
- 补配置兼容和诊断能力
- 选择少量用户可感知的 TUI 改进

## Release 范围与优先级

### P0：必须优先移植

这些改动收益高、风险低，已经证明适合当前 fork。

- 稳定性修复
  - app shutdown context 修复
  - MCP prompt timeout 修复
  - `MaxOutputTokens` 仅在大于 0 时下发
- 配置兼容
  - `HYPER_API_KEY` 支持
- 文件系统行为
  - `ls` / ignore 相关兼容性改进
  - respect git `core.excludesfile`
- morph-compact 重复压缩修复

### P1：高价值，优先移植

- 可观测与诊断
  - `crush_info`
  - `crush_logs`
- ACP / hooks 测试稳定性修复
- 消息元数据兼容处理
- 命令相关稳定性补丁

### P1：高价值 TUI 改进

- 变量高度输入框
  - 来自 upstream 的 prompt input 高度自适应
  - 已证明对当前 fork 用户体验有直接收益
- 配套布局测试
  - editor 增高时主聊天区收缩
  - follow 模式下保持滚动到底部

### P2：可继续评估

- skills 相关增强
  - 只移植与当前 fork 架构兼容的能力
  - 不为了 skill 功能去接受整套架构调整
- 其他 TUI 细节优化
  - 前提是不破坏本地 UI 分叉
- 工具和消息层的小型兼容增强

### P3：长期 RFC

- `Workspace/Server/Client` 整包迁移
- upstream 风格的 server/client 分层统一
- 本地扩展层的整体抽象重构

这些内容保留为长期方向，但当前不排期。

## 已完成项

以下内容已经在当前分支完成并通过测试验证：

- `charm.land/bubbles/v2` 升级到 `v2.1.0`
- 变量高度输入框移植
- `crush_info` 工具与测试
- `crush_logs` 工具与测试
- app shutdown context 修复
- MCP prompt timeout 修复
- `MaxOutputTokens` 发送条件修复
- `HYPER_API_KEY` 兼容支持
- ACP 事件发送竞态修复
- hooks 脆弱测试修复
- `ls` / ignore 相关兼容增强
- morph-compact 重复压缩问题修复与验证脚本

## 仍值得继续评估的上游内容

### TUI

- skills 相关可见性与交互优化
- 输入框之外的编辑体验优化
- 低冲突的列表/状态栏/布局细节改进

原则：

- 优先移植用户直接可感知的改进
- 不接受需要大范围改 UI 架构的 patch

### Agent / Tooling

- 更稳健的命令、消息和工具行为
- 更好的可观测与诊断输出
- 与 fantasy/provider 兼容性更高的消息适配

### Config / Provider

- 更多 provider 兼容处理
- 环境变量加载与覆盖行为修正
- 更严格的测试覆盖

## 暂缓项

以下内容明确暂缓：

- 重写当前本地 `agent/coordinator`
- 把本地扩展统一挪入新的 `ext/` 架构
- 以“贴近 upstream 目录结构”为目标的大搬迁
- 为了 server/client 模式而先改协议层

## 执行方式

### 阶段 1：稳定性与兼容性

目标：

- 把最容易回归的问题先解决
- 保证当前 fork 在本地模式下稳定

执行内容：

- app / commands / config / message / fsext 补丁
- provider 兼容修复
- flaky tests 修复

验证命令：

```bash
go test ./internal/app ./internal/commands ./internal/config ./internal/fsext ./internal/message
go test ./internal/acp ./internal/hooks
go test ./...
```

### 阶段 2：诊断与可观测

目标：

- 增加对当前运行状态和日志的直接检查能力

执行内容：

- `crush_info`
- `crush_logs`
- 配套注册、白名单和测试

验证命令：

```bash
go test ./internal/agent/tools -run "TestCrushInfo|TestCrushLogs"
go test ./internal/agent/...
go test ./internal/config
go test ./...
```

### 阶段 3：高价值 TUI 改进

目标：

- 移植少量但体验明显的 UI 优化

执行内容：

- 变量高度输入框
- 布局同步和 follow 模式修正
- 对应布局测试

验证命令：

```bash
go test ./internal/ui/model -count=1
go test ./...
```

### 阶段 4：继续筛选 release 候选

目标：

- 继续从 `v0.49.0` 到 `v0.56.0` 中挑选下一批低冲突改动

筛选标准：

- patch 可以独立落地
- 不要求大规模重构
- 有明确的用户价值或维护价值

## 里程碑状态

### 已完成

- 稳定性基础补丁
- 诊断工具 MVP
- 输入框高度优化
- 全量测试恢复为通过

### 进行中

- 文档与迁移策略对齐
- 继续筛选下一批 release 候选 patch

### 暂缓

- `Workspace/Server/Client` 架构统一
- skills / server / client 相关大范围结构迁移

## 决策记录

### 决策：放弃旧的整包迁移路线

原因：

- 文档原计划与当前分支真实情况不匹配
- 风险高于收益
- 当前 fork 的分叉深度已经不适合“整体接收上游”

### 决策：转为 release 驱动增量迁移

原因：

- 更符合当前代码现实
- 便于持续交付
- 能逐步吸收上游价值而不是引入大爆炸式改动

### 决策：TUI 优化只选高价值项

原因：

- TUI 本身是高耦合区域
- 但输入框高度优化这类局部改动收益明显、风险可控

## 后续维护规则

后续更新本文件时遵循以下规则：

- 不再新增“整包迁移周计划”
- 只记录可执行、可验证的 patch 级迁移
- 每新增一类上游移植，都要补“收益 / 风险 / 验证命令”
- 若某项需要结构级重构，默认放入“长期 RFC / 暂缓”

