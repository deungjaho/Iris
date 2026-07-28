package wechat

// Bridge: 把 claude stream-json 事件转成微信消息，处理权限请求
//
// 流程：
//   1. 收到微信消息 → spawn claude → SendUser(消息)
//   2. claude 输出 assistant text → 分段发微信
//   3. claude 输出 tool_use → 发 "🔧 执行: Bash(git diff)" 到微信
//   4. claude 输出 permission_request → 发微信问 Y/N → 等下一条微信消息 → SendControlResponse
//   5. claude 输出 result → 发 "✅ 完成" 到微信
//   6. 用户发 stop → Stop claude

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Bridge 单次对话的桥接器
type Bridge struct {
	client       *Client
	bridge       *ClaudeBridge
	toUser       string
	cwd          string
	mode         string // permission mode: acceptEdits / plan / normal
	resumeID     string // 非空时 resume 之前的 session
	systemPrompt string
	remote       *RemoteConfig // 非空时通过 SSH 在远程跑 claude
	apiKey       string         // 非空时覆盖 ANTHROPIC_AUTH_TOKEN（用于 per-key 调度策略）

	mu          sync.Mutex
	sessionID   string          // 从 init 事件捕获
	allowAlways map[string]bool // Y 批准过的工具，后续自动允许
}

func NewBridge(client *Client, toUser, cwd, mode, resumeID, systemPrompt string, remote *RemoteConfig, apiKey string) *Bridge {
	return &Bridge{
		client:       client,
		toUser:       toUser,
		cwd:          cwd,
		mode:         mode,
		resumeID:     resumeID,
		systemPrompt: systemPrompt,
		remote:       remote,
		apiKey:       apiKey,
		allowAlways:  make(map[string]bool),
	}
}

// SessionID 返回捕获到的 claude session_id（bridge 运行后才有值）
func (b *Bridge) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// Run 启动 claude，处理事件，直到 claude 退出或 ctx 取消
// userMsg 是第一条用户消息
// replyChan 用于接收用户后续消息（权限响应、stop 等）
func (b *Bridge) Run(ctx context.Context, userMsg string, replyChan <-chan string) error {
	var bridge *ClaudeBridge
	var err error
	if b.remote != nil && b.remote.Enabled && b.remote.Host != "" {
		remoteCwd := b.remote.Cwd
		if remoteCwd == "" {
			remoteCwd = b.cwd
		}
		bridge, err = NewRemoteClaudeBridge(ctx, b.remote.Host, remoteCwd, b.systemPrompt, b.mode, b.resumeID, b.apiKey)
	} else {
		bridge, err = NewClaudeBridge(ctx, b.cwd, b.systemPrompt, b.mode, b.resumeID, b.apiKey)
	}
	if err != nil {
		b.client.SendMessage(ctx, b.toUser, fmt.Sprintf("❌ 启动 claude 失败: %v", err))
		return err
	}
	b.bridge = bridge
	defer bridge.Stop()

	// 发第一条消息
	if err := bridge.SendUser(userMsg); err != nil {
		return fmt.Errorf("send user msg: %w", err)
	}

	// 发"正在处理"提示
	b.client.SendTyping(ctx, b.toUser)

	events := bridge.Events()
	waitingForPermission := false
	pendingRequestID := ""
	pendingToolName := ""

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case userReply, ok := <-replyChan:
			if !ok {
				// replyChan 关闭，清理权限等待状态
				if waitingForPermission && pendingRequestID != "" {
					bridge.SendControlResponse("deny", pendingRequestID)
					waitingForPermission = false
					pendingRequestID = ""
					pendingToolName = ""
				}
				return nil
			}
			reply := strings.TrimSpace(userReply)
			if strings.EqualFold(reply, "stop") || reply == "/stop" {
				b.client.SendMessage(ctx, b.toUser, "⏹ 已中断")
				bridge.Stop()
				return nil
			}
			if waitingForPermission {
				behavior := "deny"
				if reply == "y" || reply == "yes" || reply == "是" {
					behavior = "allow"
				} else if reply == "Y" {
					behavior = "allow"
					b.allowAlways[pendingToolName] = true
					log.Printf("allowAlways: %s", pendingToolName)
				}
				bridge.SendControlResponse(behavior, pendingRequestID)
				waitingForPermission = false
				pendingRequestID = ""
				pendingToolName = ""
				b.client.SendTyping(ctx, b.toUser)
			} else {
				// 普通后续消息
				bridge.SendUser(reply)
				b.client.SendTyping(ctx, b.toUser)
			}

		case ev, ok := <-events:
			if !ok {
				// claude stdout EOF
				return nil
			}
			log.Printf("event: type=%s subtype=%s", ev.Type, ev.Subtype)
			switch ev.Type {
			case "system":
				if ev.Subtype == "init" && ev.SessionID != "" {
					b.mu.Lock()
					b.sessionID = ev.SessionID
					b.mu.Unlock()
				}

			case "assistant":
				text, tools := ExtractAssistantContent(ev.Message)
				// 先发工具调用进度
				for _, tool := range tools {
					b.client.SendMessage(ctx, b.toUser, formatToolUse(tool))
				}
				if text != "" {
					log.Printf("assistant text: %s", truncate(text, 80))
					b.sendSegments(ctx, text)
				}

			case "control_request":
				// claude 请求工具权限
				toolName, toolInput := ExtractControlRequestInfo(ev)

				// 如果用户之前用 Y 批准过这个工具，自动允许
				if b.allowAlways[toolName] {
					bridge.SendControlResponse("allow", ev.RequestID)
					b.client.SendTyping(ctx, b.toUser)
					log.Printf("auto-allow (always): %s", toolName)
					break
				}

				msg := fmt.Sprintf("⚠️ 权限请求\n工具: `%s`", toolName)
				if toolInput != "" {
					msg += fmt.Sprintf("\n内容: %s", truncate(toolInput, 200))
				}
				msg += "\n\n`y` 允许 `n` 拒绝 `Y` 始终允许"
				b.client.SendMessage(ctx, b.toUser, msg)
				pendingRequestID = ev.RequestID
				pendingToolName = toolName
				waitingForPermission = true

			case "result":
				if ev.SessionID != "" {
					b.mu.Lock()
					b.sessionID = ev.SessionID
					b.mu.Unlock()
				}
				if ev.IsError {
					b.client.SendMessage(ctx, b.toUser, "❌ "+truncate(ev.Result, 500))
				} else {
					b.client.SendMessage(ctx, b.toUser, "✅ 完成")
				}
			}
		}
	}
}

func (b *Bridge) sendSegments(ctx context.Context, text string) {
	segments := SplitMessage(text)
	for i, seg := range segments {
		if len(segments) > 1 {
			seg = fmt.Sprintf("[%d/%d] %s", i+1, len(segments), seg)
		}
		if err := b.client.SendMessage(ctx, b.toUser, seg); err != nil {
			log.Printf("send message failed: %v", err)
			return
		}
		// 避免频率限制
		if i < len(segments)-1 {
			time.Sleep(300 * time.Millisecond)
		}
	}
}

// formatToolUse 格式化 tool_use 块为微信消息
func formatToolUse(tool ToolUseInfo) string {
	summary := formatToolInput(tool.Name, tool.Input)
	if summary == "" {
		return fmt.Sprintf("🔧 %s", tool.Name)
	}
	return fmt.Sprintf("🔧 %s(%s)", tool.Name, truncate(summary, 120))
}

// formatToolInput 提取工具输入的关键信息
func formatToolInput(name string, input map[string]any) string {
	if input == nil {
		return ""
	}
	switch name {
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return cmd
		}
	case "Read", "Write", "Edit", "NotebookEdit":
		if path, ok := input["file_path"].(string); ok {
			return path
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return pattern
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			return pattern
		}
	case "WebSearch":
		if q, ok := input["query"].(string); ok {
			return q
		}
	case "WebFetch":
		if u, ok := input["url"].(string); ok {
			return u
		}
	case "Task":
		if desc, ok := input["description"].(string); ok {
			return desc
		}
	}
	// 通用：显示前几个 key=value
	var parts []string
	for k, v := range input {
		s := fmt.Sprintf("%v", v)
		if len([]rune(s)) > 60 {
			s = string([]rune(s)[:60]) + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
		if len(parts) >= 3 {
			break
		}
	}
	return strings.Join(parts, ", ")
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
