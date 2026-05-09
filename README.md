# Larker

让 AI 编程助手的权限确认「飞」到手机上。

Larker 是一个 macOS 本地服务，将 Claude Code、Kimi CLI、Trae CLI 等 AI 编程助手的交互式提示桥接到**飞书/Lark**交互卡片。当 AI 助手请求执行命令、修改文件或等待输入时，你会在手机上收到一条飞书消息，点击按钮即可远程确认，无需守在电脑前。

> **核心设计："远程键盘"而非"审批门"**
> 
> Larker 不阻塞 AI 助手的 TUI 流程，而是将飞书变成一个远程键盘。用户点击"允许"后，对应的按键序列直接注入终端，AI 助手完全感知不到中间层的存在。

---

## 功能特性

- 🔐 **远程权限确认** — Claude Code 请求执行 `Bash`/`Edit`/`Write` 等工具时，飞书推送交互卡片，手机上点击即可确认
- 💬 **远程输入回复** — AI 助手等待用户输入时，直接在飞书输入框打字回复，文字注入终端
- 📋 **选项选择支持** — 支持下拉菜单/编号选项，提供「上移/下移/确认/取消」或编号按钮
- 🔓 **全局允许** — 点击「全局允许」后，该终端设备后续所有权限请求自动通过
- 📊 **执行结果同步** — 工具执行完成后，结果摘要自动回复到对应飞书话题中
- 🧵 **话题模式** — 每个会话的消息以话题形式组织，结构清晰不混乱
- 🤖 **多 AI 助手兼容** — 同时支持 Claude Code、Kimi CLI、Trae CLI，自动识别无需手动切换
- 🌙 **防休眠** — AI 助手运行期间自动阻止 macOS 进入睡眠
- 🔒 **零公网依赖** — 使用飞书 WebSocket 长连接接收回调，无需 ngrok 或内网穿透

---

## 演示

```
🚀 新会话启动 | Claude | my-project          ← SessionStart（话题根）
  ├─ 🚀 my-project | 帮我修复bug              ← PromptSubmit
  │    [OnIt reaction]                        ← 工作中
  ├─ 🔐 权限确认 | my-project                 ← Notification (permission_prompt)
  │    ⚡ Bash: rm -rf /node_modules
  │    [允许一次] [会话允许] [拒绝] [全局允许]   ← 操作按钮
  ├─ ✅ Bash 执行完成                         ← PostToolUse 回填
  ├─ 💬 等待输入 | my-project                 ← Notification (idle_prompt)
  └─ ✅ 本轮对话已结束                         ← Stop
       执行结果汇总：
       - ✅ Bash (completed)
```

---

## 系统要求

- **macOS**（终端注入和防休眠依赖 macOS 特性）
- **Go 1.23+**
- **tmux**（终端注入必须通过 tmux，`brew install tmux`）
- **飞书或 Lark 自建应用**（企业自建应用，非个人应用）

---

## 快速开始

### 1. 克隆并安装

```bash
git clone https://github.com/mnhkahn/larker.git
cd larker
bash install.sh
```

`install.sh` 会完成以下工作：

1. 编译二进制到 `/usr/local/bin/larker`
2. 交互式生成 `~/.larker/config.yaml`
3. 创建 `launchd` 服务实现开机自启
4. 自动配置 Claude Code、Kimi CLI、Trae CLI 的 Hook

### 2. 配置飞书/Lark 应用

1. 前往 [飞书开放平台](https://open.feishu.cn/app) 或 [Lark 开放平台](https://open.larksuite.com/app)
2. 创建「企业自建应用」
3. 在「凭证与基础信息」获取 **App ID** 和 **App Secret**
4. 在「权限管理」申请以下权限：
   - `im:message:send_as_bot`
   - `im:message.group_msg`（发送到群聊）
   - `im:message:p2p_msg`（发送到个人）
   - `im:chat:readonly`（读取群信息）
5. 在「事件订阅」→「连接方式」选择 **长连接（WebSocket）**
6. 订阅以下事件：
   - `im.message.receive_v1`
   - `card.action.trigger`
7. 发布应用版本（必须发布后才能正常使用）

### 3. 将机器人加入目标群聊（如使用群聊模式）

在群设置页面底部找到「群 ID」，或者直接在群里 @机器人发消息。

### 4. 启动服务

```bash
# 如果 install.sh 已完成，服务应该已自动启动
launchctl list | grep com.larker

# 手动启动
larker server

# 查看日志
tail -f ~/.larker/larker.log
```

### 5. 在 tmux 中运行 Claude Code

**关键要求：AI 助手必须在 tmux 会话中运行，否则终端注入无法工作。**

```bash
tmux new -s cc
claude  # 或 kimi、trae
```

---

## 配置说明

配置文件位于 `~/.larker/config.yaml`：

```yaml
server:
  port: 8765
  host: "127.0.0.1"

lark:
  # 国内飞书: https://open.feishu.cn (默认)
  # 国际版 Lark: https://open.larksuite.com
  base_url: "https://open.feishu.cn"
  app_id: "cli_xxx"
  app_secret: "xxx"
  receiver_type: "chat_id"   # chat_id 群聊 / open_id 个人
  receiver_id: "oc_xxx"
  working_emoji: "COMPUTER"  # 收到消息后添加的 reaction emoji

tmux:
  session_prefix: "larker"
  keep_sessions: false       # true 则完成后不结束会话

timeout:
  confirmation: 3600         # 会话超时清理时间（秒）

log:
  file: "~/.larker/larker.log"
  level: "info"              # debug, info, warn, error
  max_size_mb: 10
```

---

## 架构原理

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

### 双进程模型

**Larker Server**（长期运行）
- 监听 `ws://127.0.0.1:8765/ws`，接收 Hook 客户端的事件上报
- 维护与飞书平台的 WebSocket 长连接，实时接收卡片点击回调和 IM 消息
- 管理会话状态：`CCSession` → `Turn` → `Loop` 三级状态树
- 执行终端按键注入

**Hook Client**（短生命进程）
- 由 AI 助手 fork 执行，生命周期仅几百毫秒
- 从 `stdin` 读取 AI 助手传入的 JSON 事件
- 自动识别服务类型（Claude Code / Kimi / Trae）
- 通过 WebSocket 将事件发送到 Larker Server 后立即退出（不阻塞 AI 助手）

### 为什么使用 Notification Hook 而非 PreToolUse？

| 方案 | 机制 | 对 TUI 的影响 | 用户体验 |
|------|------|-------------|---------|
| PreToolUse | Hook 阻塞等待用户决策 | TUI 冻结，必须等待返回 | 差，终端卡死感 |
| **Notification** | **Hook 发送事件后立即退出** | **TUI 持续运行，可并行操作** | **好，终端无感知** |

---

## 支持的 AI 助手

| AI 助手 | 检测方式 | 支持的事件 |
|---------|---------|-----------|
| Claude Code | `tool_use_id`、`transcript_path` 字段 | SessionStart, PromptSubmit, Notification, PostToolUse, Stop, StopFailure |
| Kimi CLI | `tool_call_id`、`sink`、`body` 字段 | SessionStart, PromptSubmit, Notification, PostToolUse, Stop, StopFailure |
| Trae CLI | 类似 CC 的字段结构 | Notification, PostToolUse, Stop |

---

## 安全设计

- **本地 Only**：HTTP/WebSocket 仅监听 `127.0.0.1`，外部无法直接访问
- **无敏感数据落盘**：飞书 Token 仅在内存中缓存，按过期时间自动刷新
- **终端隔离**：通过 PID 树精准定位目标终端，不会误注入其他进程
- **配置权限**：`config.yaml` 生成时自动设置 `chmod 600`

---

## 常用命令

```bash
# 查看服务状态
launchctl list | grep com.larker

# 重启服务
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.larker.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.larker.plist

# 查看日志
tail -f ~/.larker/larker.log

# 手动运行 Hook 测试
echo '{"notification_type":"permission_prompt","message":"test","session_id":"xxx","cwd":"/tmp"}' | larker hook --phase=notification

# 卸载
bash uninstall.sh
```

---

## 项目结构

```
larker/
├── cmd/larker/
│   └── main.go              # 入口：server / hook 子命令
├── internal/
│   ├── server/
│   │   └── server.go        # HTTP/WebSocket 服务、会话状态管理
│   ├── lark/
│   │   ├── client.go        # 飞书 OpenAPI 客户端（发消息/卡片/回复）
│   │   ├── webhook.go       # 飞书事件结构定义
│   │   └── wsclient.go      # 飞书 WebSocket 长连接客户端
│   ├── terminal/
│   │   └── terminal.go      # TTY/tmux 目标解析与按键注入
│   ├── hook/
│   │   └── hook.go          # Hook 客户端逻辑（多 CLI 兼容层）
│   ├── config/
│   │   └── config.go        # 配置加载
│   └── sleep/
│       └── sleep.go         # macOS 睡眠管理（caffeinate）
├── scripts/
│   ├── get-ids/             # 通过 HTTP Webhook 获取 open_id/chat_id（旧方式）
│   ├── get-openid-api/      # 通过 OpenAPI 查询 open_id 和群聊列表
│   └── ws-client.go         # 本地 WebSocket 测试客户端
├── install.sh               # 一键安装脚本
├── uninstall.sh             # 卸载脚本
├── config.yaml.example      # 配置示例
└── README.md
```

---

## 已知限制

- **必须运行在 tmux 中**：终端注入依赖 `tmux send-keys`，裸终端无法注入
- **仅支持 macOS**：防休眠和终端解析使用 macOS 特定机制
- **单实例服务**：同一时间只能运行一个 `larker server` 进程
- **飞书卡片 Schema**：使用 1.0 卡片 Schema，部分新组件不支持

---

## License

MIT
