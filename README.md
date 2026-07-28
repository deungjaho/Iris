# Iris

微信远程控制 Claude Code 的桥接服务。通过微信消息远程管理多台机器上的 Claude Code session。

## 架构

```
微信用户
  │ ilink API
  ▼
Iris Server (omarchy)
  ├─ 🏠 home sandbox — 入口，简单任务
  ├─ 🖥 mac session — SSH 到 Mac 执行 claude
  ├─ 💻 omarchy session — 本地执行 claude
  └─ 🖥 tmux agent — 接管 Mac tmux 里已有的 claude
```

### 核心概念

- **Home session**：唯一临时 session，在 omarchy 的 sandbox 目录运行，启动时自动创建，不可删除
- **Mac/Omarchy session**：通过 `:go` 接管已有 session 或 `:new` 新建
- **Tmux agent**：接管 Mac tmux 里正在运行的 claude/devin

## 命令体系

### 前缀

| 前缀 | 含义 |
|------|------|
| `:` `：` | 系统命令 |
| `/` `／` | 转发给 Claude |
| `!` `！` | 执行 bash |
| `@` `＠` | 快捷切换（= `:go`） |
| `$` `＄` | 状态查询 |

### 命令

| 命令 | 说明 |
|------|------|
| `:ls [m/o/t]` | 列出 session（无参数=Iris session） |
| `:go <目标>` | 切换/接管 |
| `:new m/o <dir>` | 新建 session |
| `:rm <n>` | 删除 session |
| `:mode <m>` | 切换模式 |
| `:stop` | 停止会话 |
| `:status` | 查看状态 |
| `:help` | 帮助 |

### 目标格式

| 目标 | 含义 |
|------|------|
| `home` `h` | 回 home |
| `m0` | mac session 0 |
| `o0` | omarchy session 0 |
| `t0` | tmux agent 0 |
| `0` | Iris session 0 |
| `<标签>` | 模糊匹配 |

### 权限批准

| 输入 | 含义 |
|------|------|
| `y` | 允许一次 |
| `n` | 拒绝 |
| `Y` | 始终允许（session 内该工具自动批准） |

## 配置

`~/.config/iris/config.json`:

```json
{
  "account": {
    "token": "...",
    "baseUrl": "https://ilinkai.weixin.qq.com",
    "userId": "..."
  },
  "cwd": "/home/camt/Work",
  "systemPrompt": "...",
  "defaultMode": "acceptEdits",
  "remote": {
    "host": "macbook",
    "cwd": "/Users/tangtszho/Work"
  },
  "home": {
    "cwd": "~/.local/share/iris/sandbox",
    "mode": "acceptEdits",
    "systemPrompt": "..."
  },
  "mnemosURL": "http://localhost:8765"
}
```

## 部署

```bash
# 编译
cd server && go build -o ~/.local/bin/iris .

# 登录（首次）
iris login

# 启动
systemctl --user start iris
```

## 文件结构

```
server/
├── main.go                      # 入口、sessionManager、命令处理
├── wechat/
│   ├── bridge.go                # Bridge：claude 事件 → 微信消息
│   ├── claude_bridge.go         # ClaudeBridge：spawn claude --print
│   ├── ilink.go                 # ilink API 客户端
│   ├── message.go               # 消息分段
│   ├── escape.go                # 微信 markdown 转义
│   ├── remote_sessions.go       # 远程 session 查询
│   └── tmux_sessions.go         # tmux agent 查询
└── deploy/
    └── iris.service             # systemd service
```
