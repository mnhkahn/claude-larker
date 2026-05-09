# Larker 设计文档

## 概述

Larker 是一个 macOS 本地服务，用于将 Claude Code 的权限确认操作桥接到飞书（Lark）交互卡片。当 Claude Code 需要用户确认时，Larker 会创建一个 tmux 会话，向飞书发送一条交互卡片，然后阻塞等待用户在手机上确认或拒绝。

## 目标

- 通过 `PreToolUse` hook 拦截所有 Claude Code 的工具使用确认，`PostToolUse` hook 在工具执行完成后发送飞书通知
- 每次确认操作发送一个飞书交互卡片，标题包含 tmux 会话信息
- 每次请求创建一个独立的 tmux 会话，后续进展同步到同一条飞书消息会话中
- 支持双向消息同步：用户在飞书会话中回复的内容会转发到 tmux 会话（从而传回 Claude Code）
- 以 macOS 后台服务（launchd）运行，开机自动启动，崩溃自动重启

## 非目标

- Linux systemd 支持（仅 macOS）
- Web UI 或 CLI 仪表板
- 服务重启后保留消息历史
- 飞书 webhook 签名验证（V1 暂不支持）

## 架构

```
+---------------+      websocket       +-----------------+
| Claude Code   | <------------------> |   Larker 服务   |
| (settings)    |   (hook 客户端)       | (:8765)         |
+---------------+                      |                 |
                                       |  +-----------+  |
                                       |  | Lark      |  |
                                       |  | Agent     |  |
                                       |  +-----+-----+  |
                                       |        |        |
                                       |  +-----v-----+  |
                                       |  | Tmux      |  |
                                       |  | Bridge    |  |
                                       |  +-----------+  |
                                       +-----------------+
                                                |
                                         HTTP / Webhook
                                                |
                                       +--------v--------+
                                       |   飞书 (Lark)   |
                                       |  交互卡片        |
                                       +-----------------+
```

## 组件

### 1. 安装脚本 (`install.sh`)

一次性执行的 shell 脚本：
1. 检查 Go 环境，缺失时提示用户安装
2. 编译 `larker` 二进制：`go build -o /usr/local/bin/larker ./cmd/larker`
3. 写入或修改 `~/.claude/settings.json`（或 `settings.local.json`）：
   ```json
   {
     "hooks": {
       "PreToolUse": "/usr/local/bin/larker hook --phase=pre",
       "PostToolUse": "/usr/local/bin/larker hook --phase=post",
       "Stop": "/usr/local/bin/larker hook --phase=stop"
     }
   }
   ```
4. 创建 `~/Library/LaunchAgents/com.larker.plist`：
   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" ...>
   <plist version="1.0">
   <dict>
     <key>Label</key><string>com.larker</string>
     <key>ProgramArguments</key>
     <array>
       <string>/usr/local/bin/larker</string>
       <string>server</string>
     </array>
     <key>RunAtLoad</key><true/>
     <key>KeepAlive</key><true/>
     <key>StandardOutPath</key><string>/tmp/larker.out</string>
     <key>StandardErrorPath</key><string>/tmp/larker.err</string>
   </dict>
   </plist>
   ```
5. 执行 `launchctl load ~/Library/LaunchAgents/com.larker.plist`
6. 根据用户输入生成 `~/.larker/config.yaml`（飞书 app_id、app_secret、receiver_open_id）

### 2. Larker 服务 (`cmd/larker`)

单个 Go 二进制，通过子命令区分角色：
- `larker server` — 启动 HTTP + WebSocket 服务
- `larker hook` — 由 Claude Code `PreToolUse` / `PostToolUse` / `Stop` hook 调用；连接本地 WebSocket，发送确认请求、执行完成通知或回合结束通知

**服务路由：**

| 路由 | 方法 | 说明 |
|------|------|------|
| `/ws` | WebSocket | Claude Code hook 客户端连接 |
| `/webhook/lark` | POST | 飞书事件订阅回调 |

**HTTP 服务：**
- 使用 Go 标准库 `net/http`，不引入 gin/echo 等第三方框架
- WebSocket 使用 `github.com/gorilla/websocket`（社区事实标准），通过 `upgrader.Upgrade()` 从 `net/http` 升级
- 路由通过 `http.HandleFunc` 注册
- 启动：`http.ListenAndServe(config.Server.Host + ":" + config.Server.Port, nil)`

**内部状态：**
```go
type Session struct {
    ID          string    // request_id，也是 tmux 会话名的后缀
    TmuxName    string    // "larker-<id>"
    LarkMsgID   string    // 根消息 ID，用于回复会话
    ToolName    string    // Bash、Read、Edit 等
    ToolArgs    string    // 原始 JSON 参数
    Status      string    // pending、allow_once、allow_session、denied、timeout
    WSConn      *websocket.Conn
    CreatedAt   time.Time
}

var sessions = make(map[string]*Session)
var mu sync.RWMutex

// 会话级允许列表：整个 CC 会话中被标记为"会话允许"的工具类型
var allowedTools = make(map[string]bool)  // key: 工具名称

// 活跃工具调用计数器，用于睡眠管理
var activeToolCount int
var sleepAssertion IOPMAssertionID  // macOS 电源断言 ID
```

**睡眠管理：**
- 当 `activeToolCount > 0` 时（存在待确认或执行中的工具），创建 macOS `kIOPMAssertionTypeNoIdleSleep` 断言，阻止系统进入睡眠
- 当 `activeToolCount == 0` 时，释放断言，允许系统正常睡眠
- 断言通过 CGO 调用 `IOPMAssertionCreateWithName` / `IOPMAssertionRelease` 管理

### 3. Lark Agent (`internal/lark`)

**发送初始卡片：**
- 调用飞书 OpenAPI `POST https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=open_id`
- `msg_type`: `"interactive"`（注意：API 参数名是 `msg_type`，不是 `message_type`）
- `content`: **JSON 字符串**（需将卡片 JSON 序列化后作为字符串传入，不是 JSON 对象）
- 卡片使用 JSON 2.0 格式，必须包含 `"schema": "2.0"` 和 `"config": {"update_multi": true}`
- 卡片标题包含 tmux 会话名称，例如 `Larker Session: larker-abc123`
- 卡片正文展示工具名称和参数（与 Claude Code 原始展示完全一致）
- 按钮通过 `behaviors` 数组定义回调：
  ```json
  {
    "tag": "button",
    "text": {"tag": "plain_text", "content": "允许一次"},
    "behaviors": [{"type": "callback", "value": {"action": "allow_once", "session_id": "req-123"}}]
  }
  ```
- 三个按钮：
  - **允许一次** — 仅允许本次调用
  - **会话允许** — 本次 CC 会话中该工具类型后续不再询问
  - **拒绝** — 拒绝本次调用

**更新卡片（用户操作后）：**
- 调用飞书 OpenAPI `PATCH https://open.feishu.cn/open-apis/im/v1/messages/{message_id}`
- 仅支持更新 `interactive` 类型消息，14 天内有效
- 请求体只有 `content` 字段，传入新的完整卡片 JSON 字符串以替换旧内容
- 替换后按钮隐藏，替换为确认结果：
  - 允许一次：`✅ 已允许 (一次)` + 时间戳
  - 会话允许：`✅ 已允许 (会话)` + 时间戳
  - 已拒绝：`❌ 已拒绝` + 时间戳
  - 已超时：`⏱️ 已超时` + 时间戳
- 原始工具名称和参数保留可见

**接收回调：**
- 飞书事件订阅推送 `im.message.receive_v1` 和 `card.action.trigger` 到 `/webhook/lark`
- 点击卡片按钮时：
  1. 从 `event.action.value` 解析 action 值：`allow_once`、`allow_session` 或 `deny`
  2. 从 `event.context.open_message_id` 获取消息 ID
  3. 若 `allow_session`：将 `ToolName` 加入 `allowedTools` 映射（会话级缓存）
  4. 更新会话状态
  5. 调用 `UpdateCard` 刷新消息（隐藏按钮，展示结果）
  6. 将结果写入 tmux 会话
  7. 恢复 WebSocket 响应给 hook
- 在消息会话中回复文字：
  - 从 `event.message.content` 提取消息内容（JSON 字符串，需反序列化）
  - 将文字转发到 tmux 会话的 stdin

**执行完成通知：**
- 工具执行完成后，`PostToolUse` hook 触发
- 调用飞书回复 API：`POST /open-apis/im/v1/messages/{message_id}/reply`
- 请求体：
  ```json
  {"msg_type": "text", "content": "{\"text\":\"✅ 执行完成 ...\"}", "reply_in_thread": true}
  ```
- 成功：`✅ 执行完成` + 执行耗时 + 结果摘要（截断至 500 字符）
- 失败：`❌ 执行失败` + 错误信息（截断至 500 字符）

**会话线程：**
- 所有后续消息通过 `POST /messages/{message_id}/reply` 回复到同一条消息线程下
- `reply_in_thread: true` 确保以话题形式组织

### 4. 日志组件 (`internal/logger`)

使用 Go 标准库 `log` 或 `log/slog`（Go 1.21+）实现，输出到 `~/.larker/larker.log`。

**日志内容：**
- 每次 hook 请求：请求 ID、工具名称、参数摘要
- 飞书消息：发送/接收的 API 调用结果、消息 ID
- 卡片交互：按钮点击的动作、用户选择、状态变更
- tmux 会话：创建、写入、结束的时间点和结果
- 错误信息：API 失败、超时、连接断开等

**日志格式：**
```
[2024-01-15 10:23:45] [INFO] hook request received: id=req-123, tool=Bash
[2024-01-15 10:23:45] [INFO] lark card sent: msg_id=om_xxx, session=larker-req-123
[2024-01-15 10:24:12] [INFO] card action: id=req-123, action=allow_once
[2024-01-15 10:24:12] [INFO] hook response sent: id=req-123, result=allow
```

**日志配置：**
```yaml
log:
  file: "~/.larker/larker.log"
  level: "info"  # debug, info, warn, error
  max_size_mb: 10  # 单文件上限，超过则创建新文件
```

### 5. Tmux Bridge (`internal/tmux`)

**创建会话：**
```go
func CreateSession(name string) error {
    // tmux new-session -d -s "larker-<id>" "cat"
    // 'cat' 保持会话存活并接受 stdin
    cmd := exec.Command("tmux", "new-session", "-d", "-s", name, "cat")
    return cmd.Run()
}
```

**写入会话：**
```go
func WriteToSession(name, input string) error {
    cmd := exec.Command("tmux", "send-keys", "-t", name, input, "C-m")
    return cmd.Run()
}
```

**读取会话：**
```go
func CapturePane(name string) (string, error) {
    cmd := exec.Command("tmux", "capture-pane", "-p", "-t", name)
    return cmd.Output()
}
```

**结束会话：**
```go
func KillSession(name string) error {
    return exec.Command("tmux", "kill-session", "-t", name).Run()
}
```

**会话存在检查：**
```go
func SessionExists(name string) bool {
    err := exec.Command("tmux", "has-session", "-t", name).Run()
    return err == nil
}
```

## 数据流

### 确认流程（正常路径）

```
1. CC 决定调用 Bash("rm -rf /")
2. PreToolUse hook 触发：/usr/local/bin/larker hook --phase=pre
3. CC 通过 stdin 向 Hook 发送 JSON：{"tool_use_id":"toolu_xxx","tool_name":"Bash","tool_parameters":{"command":"rm -rf /"}}
4. Hook 从 stdin 读取 CC 输入，连接 ws://localhost:8765/ws
5. Hook 向服务端发送 WebSocket：{"phase":"pre","tool_use_id":"toolu_xxx","tool_name":"Bash",...}
6. 服务端 activeToolCount++，创建 macOS 睡眠断言（阻止系统睡眠）
7. 服务端创建 Session{ID: "req-123", TmuxName: "larker-req-123"}
8. 服务端调用 tmux.CreateSession("larker-req-123")
9. 服务端调用 lark.SendCard(session) → 获取 LarkMsgID
10. 服务端将 LarkMsgID 存入 session
11. Hook 阻塞，等待 WebSocket 响应
12. 用户看到飞书卡片：标题 "Larker Session: larker-req-123"，正文 "Bash: rm -rf /"
13. 用户点击 "允许一次"
14. 飞书 POST /webhook/lark，携带 card action (`allow_once`)
15. 服务端根据 ID 找到 session，设置 Status = "allow_once"
16. 服务端调用 lark.UpdateCard(session, "allow_once") → 卡片按钮隐藏，显示 `✅ 已允许 (一次)`
17. 服务端调用 tmux.WriteToSession("larker-req-123", "ALLOW_ONCE")
18. 服务端发送 WebSocket 响应：{"result":"allow"}
19. Hook stdout 打印 JSON 给 CC：`{"permissionDecision":"allow"}`
20. Hook 以 exit 0 退出，CC 继续执行工具
21. CC 工具执行完成
22. PostToolUse hook 触发：/usr/local/bin/larker hook --phase=post
23. CC 通过 stdin 向 Hook 发送工具执行结果 JSON
24. Hook 通过 WebSocket 将结果通知服务端
25. 服务端调用飞书回复 API，在同一条消息线程中回复：`✅ 执行完成` + 结果摘要
26. 服务端 activeToolCount--，若归零则释放睡眠断言
27. 服务端结束 tmux 会话（或根据配置保留）
```

### 回合结束通知流程（Stop hook）

```
1. CC 一轮对话结束（所有工具执行完毕，模型生成最终回复）
2. Stop hook 触发：/usr/local/bin/larker hook --phase=stop
3. CC 通过 stdin 向 Hook 发送 JSON：{"last_assistant_message":"...","transcript_path":"..."}
4. Hook 从 stdin 读取输入，连接 ws://localhost:8765/ws
5. Hook 向服务端发送 WebSocket：{"phase":"stop","message":"..."}
6. 服务端向飞书发送一条独立消息：
   - 标题：`🎯 CC 回合已结束`
   - 内容：本轮对话的助手回复摘要（截断至 1000 字符）
7. 服务端重置 activeToolCount = 0，释放睡眠断言
8. 服务端清空 allowedTools 缓存（会话级允许列表重置）
9. Hook 不等待响应，直接以 exit 0 退出
```

替代路径（用户点击 "会话允许"）：
```
13. 用户点击 "会话允许"
14. 服务端设置 Status = "allow_session"，将 "Bash" 加入 allowedTools
15. 服务端调用 lark.UpdateCard → 显示 `✅ 已允许 (会话)`
16. 服务端发送 WebSocket 响应：{"result":"allow"}
17. Hook stdout 打印 JSON 给 CC：`{"permissionDecision":"allow"}`
18. Hook 以 exit 0 退出，CC 继续执行
19. 后续 Bash 工具调用：服务端检查 allowedTools["Bash"] == true，直接返回 allow，不再发送飞书卡片
20. PostToolUse 触发后发送执行完成通知
21. activeToolCount--，归零后释放睡眠断言
```

### 双向消息同步流程

```
1. 用户在飞书会话中回复文字 "请小心执行"
2. 飞书 POST /webhook/lark，携带消息内容
3. 服务端提取文字，调用 tmux.WriteToSession("larker-req-123", "请小心执行")
4. tmux 会话接收到文字（用户 attach 后可见）
5. （未来）若 CC 处于交互模式，文字可转发到 CC stdin
```

## 配置

`~/.larker/config.yaml`：

```yaml
server:
  port: 8765
  host: "127.0.0.1"

lark:
  app_id: "cli_xxx"
  app_secret: "xxx"
  receiver_open_id: "ou_xxx"  # 默认接收人，后续可支持群聊 chat_id

tmux:
  session_prefix: "larker"
  keep_sessions: false  # true 则完成后不结束会话

timeout:
  confirmation: 3600s  # 默认 1 小时

log:
  file: "~/.larker/larker.log"
  level: "info"
  max_size_mb: 10
```

## Claude Code Hook 协议

Claude Code 的 hook 通过 **stdin** 向 hook 脚本传递 JSON 输入，hook 脚本通过 **stdout** 打印 JSON 输出返回结果，并通过 **exit code** 控制 CC 行为。

**PreToolUse stdin 输入（推断的 JSON schema，基于 changelog 和已知字段）：**
```json
{
  "tool_use_id": "toolu_01AbCdEfGhIjKlMnOpQrStUv",
  "tool_name": "Bash",
  "tool_parameters": {"command": "rm -rf /"},
  "file_path": "/absolute/path/file.txt"
}
```
- `tool_use_id`: 工具调用的唯一标识
- `tool_name`: 工具名称（Bash、Read、Edit、Write 等）
- `tool_parameters`: 工具的参数对象
- `file_path`: 对于 Write/Edit/Read 工具，传入绝对路径

**PreToolUse stdout 输出（返回给 CC 的 JSON）：**
```json
{
  "permissionDecision": "allow",
  "updatedInput": {},
  "additionalContext": "用户从飞书确认"
}
```
- `permissionDecision`: `"allow"` | `"deny"` | `"ask"`
- `updatedInput`: 可选，修改后的工具参数
- `additionalContext`: 可选，附加上下文信息（会注入模型上下文）

**PreToolUse exit code：**
- `0` → 允许工具执行
- `非 0` → 拒绝 / 取消工具调用
- `2` → 阻塞工具调用（当 stdout 输出 JSON 时使用）

**PostToolUse stdin 输入：**
```json
{
  "tool_use_id": "toolu_01AbCdEfGhIjKlMnOpQrStUv",
  "tool_name": "Bash",
  "tool_parameters": {"command": "rm -rf /"},
  "file_path": "/absolute/path/file.txt",
  "result": {...}
}
```

**Stop hook stdin 输入：**
```json
{
  "last_assistant_message": "我已经完成了代码重构...",
  "transcript_path": "/Users/xxx/.claude/sessions/.../transcript.jsonl"
}
```

## Hook 脚本行为

Hook 脚本 (`larker hook`) 同时处理 **stdin（来自 CC）** 和 **WebSocket（连接 larker server）**，作为两者之间的桥接器。

### PreToolUse 阶段（--phase=pre）

```go
func runPreHook() {
    // 1. 从 stdin 读取 CC 传入的 JSON
    var ccInput PreToolUseInput
    json.NewDecoder(os.Stdin).Decode(&ccInput)

    // 2. 连接 larker server WebSocket
    conn, _ := gorilla.Dial("ws://localhost:8765/ws")
    req := HookRequest{
        ID:        generateID(),
        Phase:     "pre",
        ToolUseID: ccInput.ToolUseID,
        ToolName:  ccInput.ToolName,
        Params:    ccInput.ToolParameters,
        FilePath:  ccInput.FilePath,
    }
    conn.WriteJSON(req)

    // 3. 阻塞等待服务端响应（飞书用户确认结果）
    var resp HookResponse
    conn.ReadJSON(&resp)

    // 4. stdout 打印 JSON 返回给 CC
    if resp.Result == "allow" {
        json.NewEncoder(os.Stdout).Encode(map[string]any{
            "permissionDecision": "allow",
        })
        os.Exit(0)
    } else {
        json.NewEncoder(os.Stdout).Encode(map[string]any{
            "permissionDecision": "deny",
        })
        os.Exit(1)
    }
}
```

### PostToolUse 阶段（--phase=post）

```go
func runPostHook() {
    var ccInput PostToolUseInput
    json.NewDecoder(os.Stdin).Decode(&ccInput)

    conn, _ := gorilla.Dial("ws://localhost:8765/ws")
    req := HookRequest{
        ID:        generateID(),
        Phase:     "post",
        ToolUseID: ccInput.ToolUseID,
        ToolName:  ccInput.ToolName,
        Params:    ccInput.ToolParameters,
        Result:    ccInput.Result,
    }
    conn.WriteJSON(req)
    // PostToolUse 不等待响应，发送即退出
    os.Exit(0)
}
```

### Stop 阶段（--phase=stop）

```go
func runStopHook() {
    var ccInput StopInput
    json.NewDecoder(os.Stdin).Decode(&ccInput)

    conn, _ := gorilla.Dial("ws://localhost:8765/ws")
    req := HookRequest{
        Phase:   "stop",
        Message: ccInput.LastAssistantMessage,
    }
    conn.WriteJSON(req)
    // Stop hook 不等待响应，发送即退出
    os.Exit(0)
}
```

**服务端会话允许缓存：**
- 服务端收到 hook 请求时，先检查 `allowedTools[toolName]`
- 若命中 → WebSocket 立即返回 `allow`，不创建 tmux 会话，不发送飞书卡片
- 若未命中 → 走完整确认流程（tmux + 飞书卡片）
- `allowedTools` 保存在内存中，服务重启后清空（与 CC 会话生命周期一致）

## 错误处理

| 场景 | 行为 |
|------|------|
| Larker 服务未运行 | Hook 打印警告到 stderr，stdout 返回 `{"permissionDecision":"deny"}`，exit 1，CC 回退到本地提示 |
| WebSocket 连接失败 | Hook 打印警告，返回 deny，exit 1 |
| 飞书 API 错误（发送卡片失败） | 服务端 WebSocket 返回 "deny"，Hook stdout 打印 deny JSON，exit 1 |
| 用户超时未确认 | 会话状态 = "timeout"，结束 tmux 会话，WebSocket 返回 deny，Hook 打印 deny JSON，exit 1 |
| Larker 崩溃 | launchd 自动重启；进行中会话丢失，但 tmux 会话仍在；新的 hook 连接从头开始 |
| Tmux 未安装 | `install.sh` 提前检查并失败 |
| 飞书凭证无效 | 服务端记录错误，API 返回 401，发送卡片失败，连锁导致 deny |
| Hook stdin 解析失败 | Hook 打印错误到 stderr，返回 deny，exit 1 |

## 安全说明

- 服务仅绑定 `127.0.0.1`（不对外暴露）
- WebSocket 无认证（仅限本地）
- 飞书凭证存储在 `~/.larker/config.yaml`，权限 `chmod 600`
- `PreToolUse` / `PostToolUse` hook 可被用户编辑 settings.json 绕过（设计如此）
- V1 不验证飞书 webhook 签名（后续可添加）

## 项目结构

```
larker/
├── cmd/larker/
│   └── main.go              # 子命令路由
├── internal/
│   ├── server/
│   │   └── server.go        # HTTP/WebSocket 服务、会话管理
│   ├── lark/
│   │   ├── client.go        # 飞书 API 调用
│   │   └── webhook.go       # webhook 处理器
│   ├── tmux/
│   │   └── tmux.go          # tmux 封装
│   ├── config/
│   │   └── config.go        # 配置加载
│   ├── logger/
│   │   └── logger.go        # 日志封装
│   ├── sleep/
│   │   └── sleep.go         # macOS 睡眠管理（CGO + IOPMAssertion）
│   └── hook/
│       └── hook.go          # hook 客户端逻辑
├── install.sh
├── go.mod                     # 依赖：gorilla/websocket, gopkg.in/yaml.v3
└── go.sum
```

## 附录：Claude Code 完整 Hook 列表

Claude Code 当前支持以下 hook 事件（基于 v2.1.114 版本）：

| Hook 事件 | 触发时机 | Larker 是否使用 |
|-----------|---------|----------------|
| **PreToolUse** | 工具调用前 | 是，核心确认流程 |
| **PostToolUse** | 工具调用后 | 是，发送执行完成通知 |
| **Stop** | 模型停止生成时（回合结束） | 是，发送回合结束通知，重置状态 |
| SessionStart | 新会话开始时 | 否 |
| SessionEnd | 会话结束时 | 否 |
| UserPromptSubmit | 用户提交消息时 | 否 |
| SubagentStop | 子代理停止时 | 否 |
| SubagentStart | 子代理开始时 | 否 |
| StopFailure | API 错误导致停止时 | 否 |
| TaskCreated | 任务创建时 | 否 |
| TaskCompleted | 任务完成时 | 否 |
| TeammateIdle | 多代理工作流中队友空闲时 | 否 |
| WorktreeCreate | 工作树创建时 | 否 |
| WorktreeRemove | 工作树移除时 | 否 |
| CwdChanged | 当前工作目录变更时 | 否 |
| FileChanged | 文件变更时 | 否 |
| ConfigChange | 配置文件变更时 | 否 |
| InstructionsLoaded | CLAUDE.md 加载时 | 否 |
| PreCompact | 会话压缩前 | 否 |
| PostCompact | 会话压缩后 | 否 |
| Elicitation | 模型主动询问用户前 | 否 |
| ElicitationResult | 用户回复后 | 否 |
| Notification | 通知事件 | 否 |
| Setup | `--init` / `--init-only` / `--maintenance` 时 | 否 |
| PermissionRequest | 权限请求时 | 否 |
| PermissionDenied | 权限被拒绝时 | 否 |

## 后续增强（V1 范围外）

- 飞书 webhook 签名验证
- 支持多接收人 / 群聊
- 消息历史持久化
- Web UI 会话管理
- Linux systemd 支持
- Slack / 企业微信适配器
