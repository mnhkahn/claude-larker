# Larker 设计文档

## 一、设计思路

### 1.1 背景与痛点

在使用 Claude Code、Kimi CLI、Trae CLI 等 AI 编程助手时，Agent 频繁需要用户确认：

- **工具调用确认**："是否允许执行 `Edit` 工具修改文件？"
- **等待用户输入**：Agent 执行完毕，等待下一步指令
- **询问对话框**：需要从多个选项中选择

传统方式要求用户**盯着终端**、**守在电脑旁**操作，无法利用手机等移动设备远程处理。

### 1.2 核心设计理念

**"远程键盘"而非"审批 gate"**

Larker 的关键设计决策是：**不阻塞 AI 助手的 TUI 流程**，而是将飞书/Lark 变成一个远程键盘。当用户在手机上点击"允许"或"拒绝"时，Larker 将对应的按键序列直接注入终端，AI 助手的 TUI 照常运转，完全感知不到中间层的存在。

为此，Larker 采用了 **Notification Hook（非阻塞）** 方案，而非传统的 PreToolUse（阻塞）方案：

| 方案 | 机制 | 对 TUI 的影响 | 用户体验 |
|------|------|-------------|---------|
| PreToolUse | Hook 阻塞等待用户决策 | TUI 冻结，必须等待返回 | 差，终端卡死感 |
| **Notification** | **Hook 发送事件后立即退出** | **TUI 持续运行，可并行操作** | **好，终端无感知** |

### 1.3 架构设计原则

1. **零侵入**：AI 助手无需任何改造，仅通过标准 Hook 机制对接
2. **多客户端兼容**：同时支持 Claude Code、Kimi CLI、Trae CLI，自动识别各平台的事件格式
3. **无公网依赖**：使用飞书 WebSocket 长连接接收回调，无需 ngrok 等内网穿透工具
4. **终端原生**：通过 `tmux send-keys` 注入输入，保持终端所有交互特性（颜色、光标、清屏等）

---

## 二、实现原理

### 2.1 整体架构

```
┌─────────────────┐     fork      ┌──────────────────┐
│  Claude Code    │──────────────▶│  larker hook     │  (短生命进程)
│  / Kimi / Trae  │  (JSON stdin) │  (各 phase)      │
└─────────────────┘               └────────┬─────────┘
                                           │ WebSocket
                                           ▼
                              ┌──────────────────────────┐
                              │    larker server         │
                              │  ┌──────────────────┐    │
                              │  │  HTTP/WebSocket  │    │
                              │  │  Server :8765    │    │
                              │  └──────────────────┘    │
                              │  ┌──────────────────┐    │
                              │  │  Lark WS Client  │◀───┼──▶ 飞书/ Lark 平台
                              │  │  (长连接)         │    │    (WebSocket 事件订阅)
                              │  └──────────────────┘    │
                              │  ┌──────────────────┐    │
                              │  │  Terminal        │───┼──▶ tmux send-keys
                              │  │  Injector        │    │    (按键注入终端)
                              │  └──────────────────┘    │
                              └──────────────────────────┘
```

### 2.2 双进程模型

#### 2.2.1 Larker Server（长期运行）

- 监听 `ws://127.0.0.1:8765/ws`，接收 Hook 客户端的事件上报
- 维护与飞书平台的 **WebSocket 长连接**，实时接收卡片点击回调和 IM 消息
- 管理会话状态：`CCSession` → `Turn` → `Loop` 三级状态树
- 执行终端按键注入

#### 2.2.2 Hook Client（短生命进程）

由 AI 助手 fork 执行，生命周期仅几百毫秒：

1. 从 `stdin` 读取 Claude Code / Kimi / Trae 传入的 JSON 事件
2. 自动识别服务类型（通过 JSON 字段特征，如 `tool_use_id` vs `tool_call_id`）
3. 通过 WebSocket 将事件发送到 Larker Server
4. **对于 Notification phase：发送后立即退出，不阻塞 AI 助手**
5. 对于 PreToolUse phase：等待 Server 返回 allow/deny 决策（兼容旧模式）

### 2.3 终端注入机制

这是 Larker 最核心的技术环节——**如何将飞书上的点击转化为终端的实际输入**。

#### 2.3.1 TTY 解析策略（多层级回退）

```
策略 0: 环境变量 LARKER_TMUX_TARGET 显式指定
    ↓
策略 1: 从 Hook PID 向上遍历进程树，查找持有 TTY 的进程
         ps -o tty= -p <pid>
    ↓
策略 2: 将 TTY 设备映射到 tmux pane
         tmux list-panes -a -F "#{pane_tty} #{session_name}:..."
    ↓
策略 3: 使用历史会话中缓存的 TerminalTarget
    ↓
失败: 在飞书卡片中提示终端注入失败，引导用户手动操作
```

#### 2.3.2 tmux 按键注入

通过 `tmux send-keys -t <target>` 发送按键，支持完整的 ANSI 转义序列映射：

| 原始序列 | tmux 按键名 | 含义 |
|---------|-----------|------|
| `\x1b[A` | `Up` | 光标上移 |
| `\x1b[B` | `Down` | 光标下移 |
| `\r` | `Enter` | 回车确认 |
| `\x1b` | `Escape` | 取消/退出 |
| `1`, `2` | `1`, `2` | 选项选择 |

> **关键约束**：Claude Code 必须在 tmux 会话中运行，否则无法完成终端注入。

### 2.4 飞书交互卡片

#### 2.4.1 卡片类型

根据 `notification_type` 生成不同的卡片交互形态：

- **`permission_prompt`**（权限确认）：展示工具名和参数，提供「允许一次」「会话允许」「拒绝」「全局允许」按钮
- **`idle_prompt`**（等待输入）：无按钮，仅展示消息内容 + 输入框
- **`elicitation_dialog`**（询问对话框）：解析消息中的选项列表，生成对应编号按钮；如检测到下拉选择形态，则提供「上移」「下移」「确认」「取消」+「全局允许」按钮组

#### 2.4.2 话题模式（Thread）

所有消息以话题形式组织，结构清晰：

```
🚀 新会话启动 | Claude | my-project          ← SessionStart（话题根）
  ├─ 🚀 my-project | 帮我修复bug              ← PromptSubmit
  │    [OnIt reaction]                        ← 工作 reaction
  ├─ 🔐 权限确认 | my-project                 ← Notification (permission_prompt)
  │    Edit 文件 /path/to/file                ← 工具详情卡片
  │    [允许一次] [会话允许] [拒绝] [全局允许]   ← 操作按钮
  ├─ ✅ Edit 结果                            ← PostToolUse 回填
  ├─ 💬 等待输入 | my-project                 ← Notification (idle_prompt)
  └─ ✅ 本轮对话已结束                         ← Stop
       执行结果汇总：
       - ✅ Edit (completed)
```

### 2.5 WebSocket 长连接事件订阅

与飞书/ Lark 平台通信采用 **WebSocket 长连接**（非 HTTP Webhook）：

1. 启动时调用 `/callback/ws/endpoint` 获取 WS 接入点
2. 建立 WebSocket 连接，自动维护心跳（ping/pong）
3. 订阅事件：
   - `im.message.receive_v1`：接收用户在群里的文字回复
   - `card.action.trigger`：接收卡片按钮点击回调
4. 连接断开后自动重连（10 秒间隔，无限重试）

优势：**无需公网 IP、无需 ngrok、无需配置回调 URL**，即开即用。

### 2.6 防休眠机制

macOS 在长时间等待用户确认时可能进入睡眠。Larker 通过 **CGO 调用 IOKit** 创建电源断言：

```c
IOPMAssertionCreateWithName(
    kIOPMAssertionTypeNoIdleSleep,
    kIOPMAssertionLevelOn,
    "Larker waiting for Lark confirmation",
    &assertionID
);
```

在等待飞书确认期间阻止系统休眠，确认完成后立即释放。

### 2.7 多 CLI 兼容层

Hook 模块通过 JSON 字段特征自动识别输入来自哪个 CLI：

| CLI | 检测字段 | 特殊处理 |
|-----|---------|---------|
| Claude Code | `tool_use_id`, `transcript_path` | 标准字段名 |
| Kimi CLI | `tool_call_id`, `sink`, `body` | 字段映射转换 |
| Trae CLI | 类似 CC 的字段结构 | 兼容处理 |

输出时同样区分格式：
- Claude Code: `{"permissionDecision": "allow"}`
- Kimi: `{"hookSpecificOutput": {"permissionDecision": "allow"}}`

---

## 三、功能说明

### 3.1 核心功能

| 功能 | 说明 |
|------|------|
| **远程权限确认** | Claude Code 请求工具调用时，飞书推送交互卡片，用户在手机上点击「允许/拒绝」后自动注入终端 |
| **远程输入回复** | Agent 等待用户输入时，用户可在飞书输入框直接打字回复，文字内容注入终端 |
| **选项选择** | 支持 Claude Code 的下拉菜单/编号选项，提供「上移/下移/确认/取消」或编号按钮 |
| **全局允许** | 点击「全局允许」后，该终端设备后续所有权限请求自动通过（注入 `2`，即"Allow for session"） |
| **执行结果同步** | 工具执行完成后，将结果摘要自动回复到对应飞书话题中 |
| **会话生命周期追踪** | 完整追踪 Session → Turn → Loop 三级结构，在 Stop 时汇总本轮所有操作 |
| **多 AI 助手支持** | 同时支持 Claude Code、Kimi CLI、Trae CLI，自动识别无需手动切换 |

### 3.2 消息流转

#### 3.2.1 SessionStart（会话启动）

```
AI 助手启动
  → Hook 发送 sessionstart 事件
  → Server 创建 CCSession
  → 发送 "🚀 新会话启动" 消息到飞书（作为话题根）
```

#### 3.2.2 PromptSubmit（用户提交指令）

```
用户在终端输入指令
  → Hook 发送 promptsubmit 事件
  → Server 结束上一 Turn，创建新 Turn
  → 发送 "🚀 项目名 | 指令预览" 到飞书话题
  → 添加 "OnIt" reaction 表示处理中
```

#### 3.2.3 Notification（需要用户交互）

```
AI 助手显示提示（权限/等待/询问）
  → Hook 发送 notification 事件（立即退出，不阻塞）
  → Server 创建 Loop，发送交互卡片到飞书话题
  → 用户在飞书点击按钮 / 输入文字
  → 飞书 WS 推送 card.action.trigger 事件
  → Server 查找对应 Loop，执行终端按键注入
  → 更新卡片状态（允许/拒绝/超时）
```

#### 3.2.4 PostToolUse（工具执行完成）

```
AI 助手执行完工具
  → Hook 发送 posttooluse 事件
  → Server 将结果摘要回复到飞书话题
  → 更新 Loop 状态为 completed
```

#### 3.2.5 Stop（回合正常结束）

```
AI 助手完成本轮响应
  → Hook 发送 stop 事件
  → Server 结束当前 Turn，撤回 working reaction
  → 发送 "✅ 本轮对话已结束" + 执行结果汇总
```

#### 3.2.6 StopFailure（异常退出）

```
AI 助手异常终止
  → Hook 发送 stopfailure 事件
  → Server 标记 Turn 为 failed
  → 发送 "❌ 异常退出" 到飞书话题
```

### 3.3 配置项

配置文件位于 `~/.larker/config.yaml`：

| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| `server.host` | 本地监听地址 | `127.0.0.1` |
| `server.port` | 本地监听端口 | `8765` |
| `lark.base_url` | 飞书/ Lark API 域名 | `https://open.feishu.cn` |
| `lark.app_id` | 自建应用 App ID | - |
| `lark.app_secret` | 自建应用 App Secret | - |
| `lark.receiver_type` | 接收者类型：`chat_id` 群聊 / `open_id` 个人 | `chat_id` |
| `lark.receiver_id` | 接收者 ID | - |
| `timeout.confirmation` | 会话/ Turn 超时清理时间（秒） | `3600` |
| `log.level` | 日志级别 | `info` |
| `log.file` | 日志文件路径 | `~/.larker/larker.log` |

### 3.4 命令行用法

```bash
# 启动服务端（通常由 launchd 托管）
larker server

# 手动运行 Hook（供 AI 助手调用）
echo '<json>' | larker hook --phase=notification

# 支持指定服务类型（覆盖自动检测）
larker hook --phase=post --service=kimi
```

### 3.5 安装与部署

`install.sh` 一键脚本完成以下工作：

1. 检查依赖（Go、tmux）
2. 编译二进制到 `/usr/local/bin/larker`
3. 交互式生成 `~/.larker/config.yaml`（支持飞书/ Lark 选择、群聊/个人选择、自动查询 chat_id / open_id）
4. 创建 `launchd` 服务 `com.larker.plist`，实现开机自启
5. 自动配置 Claude Code (`~/.claude/settings.local.json`)、Trae CLI (`~/.trae/traecli.yaml`)、Kimi CLI (`~/.kimi/config.toml`) 的 Hook

---

## 四、数据模型

### 4.1 CCSession

表示一个 AI 助手进程实例的生命周期：

- `SessionID`：AI 助手分配的会话 ID
- `HookPID`：最近一次 Hook 进程的 PID
- `TerminalTarget`：解析到的 tmux 目标地址
- `Turns`：该会话下的所有 Turn
- `Active`：是否仍在运行

### 4.2 Turn

表示一次 "PromptSubmit → Stop" 的完整回合：

- `TurnID`：回合唯一标识
- `ThreadMsgID`：飞书话题根消息 ID，后续所有消息以此回复
- `Loops`：回合内的所有交互 Loop
- `FirstMessage`：用户最初的 Prompt 内容（用于飞书标题展示）
- `WorkingReactionID`：处理中状态的 reaction ID，回合结束时撤回

### 4.3 Loop

表示一次需要用户交互的具体事件：

- `LoopID`：工具调用时取 `ToolUseID`，其他情况自动生成
- `Type`：`permission_prompt` / `idle_prompt` / `elicitation_dialog`
- `ToolName` / `Params`：工具调用名称和参数
- `Result` / `Summary`：PostToolUse 回填的执行结果
- `Status`：`pending` / `allowed` / `denied` / `completed`
- `CardMsgID`：飞书卡片消息 ID
- `NotificationID`：回调路由 ID
- `Responses`：按钮到按键序列的映射表

---

## 五、安全与限制

### 5.1 安全设计

- **本地-only 服务**：HTTP/WebSocket 仅监听 `127.0.0.1`，外部无法直接访问
- **无敏感数据落盘**：飞书 Token 仅在内存中缓存，按过期时间自动刷新
- **终端隔离**：通过 PID 树精准定位目标终端，不会误注入其他进程
- **配置权限**：`config.yaml` 生成时设置 `chmod 600`

### 5.2 已知限制

- **必须运行在 tmux 中**：终端注入依赖 `tmux send-keys`，裸终端无法注入
- **仅支持 macOS**：防休眠使用 IOKit CGO，终端解析使用 macOS `ps` 命令
- **飞书卡片 Schema 限制**：使用 1.0 卡片 Schema，部分新组件不支持
- **单实例服务**：同一时间只能运行一个 larker server 进程
