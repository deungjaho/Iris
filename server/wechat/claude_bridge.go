package wechat

// Claude Bridge: 管理 claude --print 子进程，处理 stream-json 双向协议
//
// claude stdin:  {"type":"user","content":"..."}  或  {"type":"control_response",...}
// claude stdout: NDJSON，每行一个事件：
//   {"type":"system","subtype":"init",...}
//   {"type":"assistant","message":{"content":[{"type":"text","text":"..."}]}}
//   {"type":"tool_use","name":"Bash","input":{...}}
//   {"type":"tool_result",...}
//   {"type":"permission_request","tool":"Bash","input":{...}}
//   {"type":"result","subtype":"success",...}

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// filterProxyEnv 移除 HTTP 代理相关环境变量
func filterProxyEnv(env []string) []string {
	var out []string
	for _, e := range env {
		if strings.HasPrefix(e, "HTTP_PROXY=") ||
			strings.HasPrefix(e, "HTTPS_PROXY=") ||
			strings.HasPrefix(e, "http_proxy=") ||
			strings.HasPrefix(e, "https_proxy=") ||
			strings.HasPrefix(e, "FTP_PROXY=") ||
			strings.HasPrefix(e, "ftp_proxy=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// overrideEnv 替换环境变量列表中某个 key 的值（不存在则追加）
func overrideEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// ClaudeEvent 从 claude stdout 解析的事件
type ClaudeEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	// system init / result 都带 session_id
	SessionID string `json:"session_id,omitempty"`
	// assistant
	Message json.RawMessage `json:"message,omitempty"`
	// tool_use
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// permission_request
	Tool string `json:"tool,omitempty"`
	// control_request
	RequestID string          `json:"request_id,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
	// result
	Result  string `json:"result,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// 原始行
	Raw string `json:"-"`
}

// ToolUseInfo 从 assistant message content 里提取的 tool_use 块
type ToolUseInfo struct {
	Name  string
	Input map[string]any
}

// ClaudeBridge 管理 claude --print 子进程（本地或远程 SSH）
type ClaudeBridge struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	mu     sync.Mutex
	alive  bool
}

// RemoteConfig 远程 SSH 配置
type RemoteConfig struct {
	Host    string `json:"host"`    // SSH 别名（如 "macbook"）
	Cwd     string `json:"cwd"`     // 远程工作目录
	Enabled bool   `json:"enabled"` // 是否启用远程模式
}

// claudeArgs 构建 claude 命令行参数
func claudeArgs(systemPrompt, mode, resumeID string) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--model", "claude-sonnet-4-6",
		"--setting-sources", "user,project",
	}
	if mode != "" {
		args = append(args, "--permission-mode", mode)
	} else {
		args = append(args, "--permission-mode", "acceptEdits")
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if systemPrompt != "" {
		args = append(args, "--append-system-prompt", systemPrompt)
	}
	return args
}

// NewClaudeBridge spawn claude --print（本地模式）
// mode: "acceptEdits", "plan", "normal" 等
// resumeID: 非空时用 --resume 恢复之前的 session
// apiKey: 非空时覆盖 ANTHROPIC_AUTH_TOKEN 环境变量（用于 per-key 调度策略）
func NewClaudeBridge(ctx context.Context, cwd, systemPrompt, mode, resumeID, apiKey string) (*ClaudeBridge, error) {
	args := claudeArgs(systemPrompt, mode, resumeID)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = cwd
	// 清掉 HTTP 代理环境变量，避免 claude 走代理连不上本地 hydra
	env := filterProxyEnv(os.Environ())
	// 如果指定了专用 API key，覆盖 ANTHROPIC_AUTH_TOKEN
	if apiKey != "" {
		env = overrideEnv(env, "ANTHROPIC_AUTH_TOKEN", apiKey)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = nil // 丢弃 stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	return &ClaudeBridge{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		alive:  true,
	}, nil
}

// NewRemoteClaudeBridge 通过 SSH 在远程机器上 spawn claude --print
func NewRemoteClaudeBridge(ctx context.Context, sshHost, remoteCwd, systemPrompt, mode, resumeID, apiKey string) (*ClaudeBridge, error) {
	args := claudeArgs(systemPrompt, mode, resumeID)

	// 构建远程命令：cd 到目录 && claude <args>
	// 用 bash -lc 确保 PATH 包含 claude
	var argParts []string
	for _, a := range args {
		// 简单转义：包含空格或特殊字符的用单引号包裹
		if strings.ContainsAny(a, " \t'\"") {
			argParts = append(argParts, "'"+strings.ReplaceAll(a, "'", "'\\''")+"'")
		} else {
			argParts = append(argParts, a)
		}
	}
	// 如果指定了专用 API key，在远程命令里设置 ANTHROPIC_AUTH_TOKEN
	var envPrefix string
	if apiKey != "" {
		envPrefix = fmt.Sprintf("ANTHROPIC_AUTH_TOKEN=%s ", shellQuote(apiKey))
	}
	remoteCmd := fmt.Sprintf("cd '%s' && %sclaude %s", remoteCwd, envPrefix, strings.Join(argParts, " "))

	// ssh macbook "bash -lc 'cd /path && claude ...'"
	sshArgs := []string{
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		sshHost,
		"bash -lc " + shellQuote(remoteCmd),
	}

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	// 清掉代理环境变量
	cmd.Env = filterProxyEnv(os.Environ())

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdout pipe: %w", err)
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh: %w", err)
	}

	return &ClaudeBridge{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		alive:  true,
	}, nil
}

// shellQuote 用单引号包裹 shell 字符串
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// SendUser 发用户消息
func (b *ClaudeBridge) SendUser(content string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": content},
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal user msg: %w", err)
	}
	_, err = fmt.Fprintln(b.stdin, string(data))
	return err
}

// SendControlResponse 发权限响应
// behavior: "allow" 或 "deny"
// requestID: 来自 control_request 的 request_id
func (b *ClaudeBridge) SendControlResponse(behavior, requestID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response": map[string]any{
				"behavior": behavior,
			},
		},
	}
	if behavior == "deny" {
		msg["response"].(map[string]any)["response"].(map[string]any)["message"] = "用户通过微信拒绝"
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal control response: %w", err)
	}
	_, err = fmt.Fprintln(b.stdin, string(data))
	return err
}

// Stop 停止子进程
func (b *ClaudeBridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.alive {
		b.stdin.Close()
		b.cmd.Process.Kill()
		b.alive = false
	}
}

// IsAlive 是否还活着
func (b *ClaudeBridge) IsAlive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.alive
}

// Events 返回事件 channel，阻塞读取 claude stdout 直到 EOF
func (b *ClaudeBridge) Events() <-chan *ClaudeEvent {
	ch := make(chan *ClaudeEvent, 100)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(b.stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var ev ClaudeEvent
			ev.Raw = line
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			ch <- &ev
		}
		b.mu.Lock()
		b.alive = false
		b.mu.Unlock()
	}()
	return ch
}

// Wait 等待进程退出
func (b *ClaudeBridge) Wait() error {
	return b.cmd.Wait()
}

// ExtractAssistantText 从 assistant message 里提取文本（兼容旧调用）
func ExtractAssistantText(msgRaw json.RawMessage) string {
	text, _ := ExtractAssistantContent(msgRaw)
	return text
}

// ExtractAssistantContent 从 assistant message 里提取文本和 tool_use 块
func ExtractAssistantContent(msgRaw json.RawMessage) (text string, tools []ToolUseInfo) {
	var msg struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text,omitempty"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return
	}
	var parts []string
	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		case "tool_use":
			var input map[string]any
			if len(c.Input) > 0 && string(c.Input) != "null" {
				json.Unmarshal(c.Input, &input)
			}
			tools = append(tools, ToolUseInfo{Name: c.Name, Input: input})
		}
	}
	text = strings.Join(parts, "\n")
	return
}

// ExtractControlRequestInfo 从 control_request 提取工具名和输入摘要
func ExtractControlRequestInfo(ev *ClaudeEvent) (toolName, toolInput string) {
	var req struct {
		Subtype  string          `json:"subtype"`
		ToolName string          `json:"tool_name"`
		Input    json.RawMessage `json:"input,omitempty"`
	}
	if err := json.Unmarshal(ev.Request, &req); err != nil {
		return
	}
	toolName = req.ToolName
	if len(req.Input) > 0 && string(req.Input) != "null" {
		// 提取关键字段
		var input map[string]any
		if err := json.Unmarshal(req.Input, &input); err == nil {
			if cmd, ok := input["command"].(string); ok {
				toolInput = cmd
			} else if path, ok := input["file_path"].(string); ok {
				toolInput = path
			} else {
				// 其他工具，输出 JSON 摘要
				toolInput = string(req.Input)
				if len([]rune(toolInput)) > 200 {
					toolInput = string([]rune(toolInput)[:200]) + "..."
				}
			}
		}
	}
	return
}
