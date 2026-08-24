package wechat

// Pantheon CLI 封装：Iris 通过 pantheon CLI 查询 run 状态和发送消息。
// 所有调用是同步的，用 context timeout 限制。

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// PantheonClient 封装 pantheon CLI 调用
type PantheonClient struct {
	CliPath    string // pantheon 二进制路径，默认 "pantheon"
	SocketPath string // Unix socket 路径，空则用默认
}

// RunInfo 简化的 run 状态（run.list 返回的每个 run）
type RunInfo struct {
	RunID       string `json:"run_id"`
	ProjectID   string `json:"project_id"`
	State       string `json:"state"`
	ResultState string `json:"result_state"`
	StartedAt   string `json:"started_at"`
	EndedAt     string `json:"ended_at,omitempty"`
}

// RunListResult run.list 返回
type RunListResult struct {
	Runs []RunInfo `json:"runs"`
}

// TaskInfo run.status 里的 task
type TaskInfo struct {
	TaskID       string `json:"task_id"`
	Objective    string `json:"objective"`
	RiskLevel    string `json:"risk_level"`
	State        string `json:"state"`
	WorktreePath string `json:"worktree_path,omitempty"`
}

// AgentInfo run.status 里的 agent
type AgentInfo struct {
	AgentID string `json:"agent_id"`
	Role    string `json:"role"`
	Runtime string `json:"runtime"`
	State   string `json:"state"`
	PID     int    `json:"pid"`
}

// RunStatusResult run.status 返回
type RunStatusResult struct {
	Run   RunInfo    `json:"run"`
	Task  *TaskInfo  `json:"task,omitempty"`
	Agent *AgentInfo `json:"agent,omitempty"`
}

// MessageInfo messages.by_run 返回的单条消息
type MessageInfo struct {
	Seq        int64  `json:"seq"`
	MessageID  string `json:"message_id"`
	Type       string `json:"type"`
	SenderRole string `json:"sender_role"`
	Inline     string `json:"inline,omitempty"`
}

// MessagesByRunResult messages.by_run 返回
type MessagesByRunResult struct {
	Messages   []MessageInfo `json:"messages"`
	NextCursor int64         `json:"next_cursor"`
}

// PublishResult message.publish.envelope 返回
type PublishResult struct {
	Seq        int64  `json:"seq"`
	MessageSeq int64  `json:"message_seq"`
	MessageID  string `json:"message_id"`
	Deduped    bool   `json:"deduped"`
}

// NewPantheonClient 创建 Pantheon CLI 客户端
func NewPantheonClient(cliPath, socketPath string) *PantheonClient {
	if cliPath == "" {
		cliPath = "pantheon"
	}
	return &PantheonClient{CliPath: cliPath, SocketPath: socketPath}
}

// call 执行 pantheon CLI 命令，解析 JSON-RPC 响应
func (c *PantheonClient) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	paramJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}

	args := []string{method, string(paramJSON)}
	if c.SocketPath != "" {
		args = append([]string{"-socket", c.SocketPath}, args...)
	}

	cmd := exec.CommandContext(ctx, c.CliPath, args...)
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("pantheon %s: %w (stderr: %s)", method, err, string(output))
	}

	// 解析 JSON-RPC 响应
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(output, &resp); err != nil {
		return nil, fmt.Errorf("parse pantheon response: %w (raw: %s)", err, truncate(string(output), 200))
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("pantheon %s: %s", method, resp.Error.Message)
	}
	return resp.Result, nil
}

// ListRuns 调 pantheon run.list
func (c *PantheonClient) ListRuns(ctx context.Context) ([]RunInfo, error) {
	raw, err := c.call(ctx, "run.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var result RunListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse run.list: %w", err)
	}
	return result.Runs, nil
}

// RunStatus 调 pantheon run.status
func (c *PantheonClient) RunStatus(ctx context.Context, runID string) (*RunStatusResult, error) {
	raw, err := c.call(ctx, "run.status", map[string]any{"run_id": runID})
	if err != nil {
		return nil, err
	}
	var result RunStatusResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse run.status: %w", err)
	}
	return &result, nil
}

// MessagesByRun 调 pantheon messages.by_run
func (c *PantheonClient) MessagesByRun(ctx context.Context, runID string) (*MessagesByRunResult, error) {
	raw, err := c.call(ctx, "messages.by_run", map[string]any{"run_id": runID})
	if err != nil {
		return nil, err
	}
	var result MessagesByRunResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse messages.by_run: %w", err)
	}
	return &result, nil
}

// PublishMessage 调 pantheon message.publish.envelope
// senderRole/recipientRole: "pm", "worker", "verifier", "controller", "metis"
// msgType: "directive", "report", "state", "block", "complete", "verify", "ack", "nack"
func (c *PantheonClient) PublishMessage(ctx context.Context, runID, senderRole, recipientRole, msgType, inline string) (*PublishResult, error) {
	params := map[string]any{
		"run_id":    runID,
		"sender":    map[string]any{"role": senderRole},
		"recipient": map[string]any{"role": recipientRole},
		"type":      msgType,
		"payload_ref": map[string]any{
			"kind":   "inline",
			"inline": inline,
		},
	}
	raw, err := c.call(ctx, "message.publish.envelope", params)
	if err != nil {
		return nil, err
	}
	var result PublishResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse publish result: %w", err)
	}
	return &result, nil
}

// FormatRuns 格式化 run 列表为微信消息
func FormatRuns(runs []RunInfo, limit int) string {
	if len(runs) == 0 {
		return "📋 没有 Pantheon run"
	}
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	var b []byte
	b = append(b, fmt.Sprintf("📋 Pantheon run (%d个)\n\n", len(runs))...)
	b = append(b, "| # | Run ID | 状态 | 结果 | 开始时间 |\n"...)
	b = append(b, "|---|--------|------|------|---------|\n"...)
	for i, r := range runs {
		state := r.State
		result := r.ResultState
		if result == "" {
			result = "-"
		}
		started := r.StartedAt
		if len(started) > 16 {
			started = started[5:16] // MM-DD HH:MM
		}
		b = append(b, fmt.Sprintf("| %d | `%s` | %s | %s | %s |\n",
			i, truncate(r.RunID, 12), state, result, started)...)
	}
	return string(b)
}

// FormatRunStatus 格式化单个 run 的详细状态
func FormatRunStatus(r *RunStatusResult) string {
	var b []byte
	b = append(b, "**📊 Run 状态**\n\n"...)
	b = append(b, fmt.Sprintf("| 属性 | 值 |\n|------|----|\n")...)
	b = append(b, fmt.Sprintf("| Run ID | `%s` |\n", truncate(r.Run.RunID, 16))...)
	b = append(b, fmt.Sprintf("| 状态 | %s |\n", r.Run.State)...)
	b = append(b, fmt.Sprintf("| 结果 | %s |\n", r.Run.ResultState)...)
	if r.Task != nil {
		b = append(b, fmt.Sprintf("| 目标 | %s |\n", truncate(r.Task.Objective, 40))...)
		b = append(b, fmt.Sprintf("| 风险 | %s |\n", r.Task.RiskLevel)...)
	}
	if r.Agent != nil {
		b = append(b, fmt.Sprintf("| Agent | `%s` %s %s |\n", truncate(r.Agent.AgentID, 12), r.Agent.Role, r.Agent.Runtime)...)
		b = append(b, fmt.Sprintf("| Agent 状态 | %s |\n", r.Agent.State)...)
	}
	return string(b)
}

// FormatMessages 格式化消息列表
func FormatMessages(msgs []MessageInfo) string {
	if len(msgs) == 0 {
		return "📭 没有消息"
	}
	var b []byte
	b = append(b, fmt.Sprintf("📬 消息 (%d条)\n\n", len(msgs))...)
	for i, m := range msgs {
		inline := m.Inline
		if inline == "" {
			inline = "(无内容)"
		}
		b = append(b, fmt.Sprintf("%d. [%s] %s→%s\n   %s\n",
			i+1, m.Type, m.SenderRole, "", truncate(inline, 60))...)
	}
	return string(b)
}

// BeaconPane Beacon status 里的 pane
type BeaconPane struct {
	Session      string `json:"session"`
	Window       string `json:"window"`
	Status       string `json:"status"`
	Summary      string `json:"summary"`
	Cwd          string `json:"cwd"`
	Acknowledged bool   `json:"acknowledged"`
}

// BeaconStatus beacon status 返回
type BeaconStatus struct {
	Panes         map[string]BeaconPane `json:"panes"`
	LastCompleted string                `json:"last_completed,omitempty"`
}

// BeaconClient 封装 beacon CLI 调用
type BeaconClient struct {
	CliPath string
}

// NewBeaconClient 创建 Beacon CLI 客户端
func NewBeaconClient(cliPath string) *BeaconClient {
	if cliPath == "" {
		cliPath = "beacon"
	}
	return &BeaconClient{CliPath: cliPath}
}

// Status 调 beacon status
func (c *BeaconClient) Status(ctx context.Context) (*BeaconStatus, error) {
	cmd := exec.CommandContext(ctx, c.CliPath, "status")
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("beacon status: %w", err)
	}
	var result BeaconStatus
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("parse beacon status: %w", err)
	}
	return &result, nil
}

// FormatBeaconStatus 格式化 beacon status 为微信消息
func FormatBeaconStatus(s *BeaconStatus) string {
	if len(s.Panes) == 0 {
		return "📋 Beacon 没有追踪的 pane"
	}
	var b []byte
	b = append(b, fmt.Sprintf("📋 Beacon agent (%d个)\n\n", len(s.Panes))...)
	b = append(b, "| Pane | 状态 | 摘要 | 目录 |\n"...)
	b = append(b, "|------|------|------|------|\n"...)
	for pid, p := range s.Panes {
		statusIcon := map[string]string{
			"working":   "▶",
			"waiting":   "⏸",
			"blocked":   "🚫",
			"completed": "✅",
		}[p.Status]
		if statusIcon == "" {
			statusIcon = p.Status
		}
		b = append(b, fmt.Sprintf("| %s | %s | %s | `%s` |\n",
			pid, statusIcon, truncate(p.Summary, 20), truncate(p.Cwd, 25))...)
	}
	return string(b)
}

// DefaultTimeout 默认 CLI 调用超时
const DefaultTimeout = 10 * time.Second
