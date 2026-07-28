package wechat

// tmux session 索引：通过 SSH 查询远程机器上 tmux 里运行的 claude/devin agent
// 按 tmux session → window 分组，提取每个 agent 的 claude session ID

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// TmuxAgent tmux 里运行的一个 agent
type TmuxAgent struct {
	TmuxSession string `json:"tmux_session"`
	WindowIdx   string `json:"window_idx"`
	WindowName  string `json:"window_name"`
	PaneID      string `json:"pane_id"`
	AgentType   string `json:"agent_type"` // claude / devin
	Cwd         string `json:"cwd"`
	SessionID   string `json:"session_id"` // claude session ID
	Title       string `json:"title"`      // session 标题或首条消息
}

// ListTmuxAgents 通过 SSH 列出远程 tmux 里运行的 agent
func ListTmuxAgents(ctx context.Context, sshHost string) ([]TmuxAgent, error) {
	// 远程 Python 脚本：
	// 1. tmux list-panes -a 获取所有 pane
	// 2. 过滤出 claude/devin 进程
	// 3. 对每个 agent pane，根据 cwd 找最近的 claude session ID
	script := `
import json, os, sys, subprocess, glob

# 获取所有 tmux pane
try:
    result = subprocess.run(
        ["tmux", "list-panes", "-a", "-F",
         "#{session_name}|#{window_index}|#{window_name}|#{pane_index}|#{pane_id}|#{pane_pid}|#{pane_current_command}|#{pane_current_path}"],
        capture_output=True, text=True, timeout=5)
except Exception:
    print("[]")
    sys.exit(0)

agents = []
for line in result.stdout.strip().split("\n"):
    if not line:
        continue
    parts = line.split("|")
    if len(parts) < 8:
        continue
    sess, win_idx, win_name, pane_idx, pane_id, pane_pid, cmd, cwd = parts[:8]

    # 只关注 claude 和 devin 进程
    if cmd not in ("claude", "devin", "node"):
        continue

    # 对于 node，检查是否是 claude（claude 是 node 进程）
    agent_type = cmd
    if cmd == "node":
        # 检查进程参数是否包含 claude
        try:
            ps = subprocess.run(["ps", "-p", pane_pid, "-o", "args="],
                              capture_output=True, text=True, timeout=2)
            if "claude" in ps.stdout:
                agent_type = "claude"
            else:
                continue
        except:
            continue

    # 根据 cwd 找 claude session ID
    session_id = ""
    title = ""
    if cwd:
        encoded = cwd.replace("/", "-")
        project_dir = os.path.expanduser("~/.claude/projects/" + encoded)
        if os.path.isdir(project_dir):
            files = sorted(glob.glob(project_dir + "/*.jsonl"),
                          key=os.path.getmtime, reverse=True)
            if files:
                session_id = os.path.basename(files[0]).replace(".jsonl", "")
                # 提取标题
                try:
                    with open(files[0]) as fh:
                        for fline in fh:
                            try:
                                d = json.loads(fline)
                            except:
                                continue
                            if d.get("type") == "custom-title":
                                title = d.get("customTitle", "")
                                break
                            elif d.get("type") == "user":
                                msg = d.get("message", {})
                                content = msg.get("content", "")
                                if isinstance(content, list):
                                    for c in content:
                                        if c.get("type") == "text":
                                            t = c.get("text", "")
                                            if t and not t.startswith("<"):
                                                title = t[:30]
                                                break
                                elif isinstance(content, str) and not content.startswith("<"):
                                    title = content[:30]
                                if title:
                                    break
                except:
                    pass

    agents.append({
        "tmux_session": sess,
        "window_idx": win_idx,
        "window_name": win_name,
        "pane_id": pane_id,
        "agent_type": agent_type,
        "cwd": cwd,
        "session_id": session_id,
        "title": title,
    })

print(json.dumps(agents))
`

	args := []string{
		"-o", "ConnectTimeout=10",
		sshHost,
		"bash -lc " + shellQuote("python3 -c "+shellQuote(script)),
	}

	cmd := exec.CommandContext(ctx, "ssh", args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ssh list tmux: %w", err)
	}

	var agents []TmuxAgent
	if err := json.Unmarshal(output, &agents); err != nil {
		return nil, fmt.Errorf("parse tmux agents: %w (raw: %s)", err, string(output))
	}

	// 按 tmux session 分组排序
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].TmuxSession != agents[j].TmuxSession {
			return agents[i].TmuxSession < agents[j].TmuxSession
		}
		return agents[i].WindowIdx < agents[j].WindowIdx
	})

	return agents, nil
}

// FormatTmuxAgents 把 tmux agent 列表格式化为微信消息
// 返回格式化后的文本和扁平化的 agent 列表（用于 /use <序号>）
func FormatTmuxAgents(agents []TmuxAgent) string {
	if len(agents) == 0 {
		return "📋 tmux 里没有 agent"
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("📋 tmux agent (%d个)\n", len(agents)))

	prevSession := ""
	idx := 0
	for _, a := range agents {
		if a.TmuxSession != prevSession {
			b.WriteString(a.TmuxSession + "\n")
			prevSession = a.TmuxSession
		}
		label := a.Title
		if label == "" {
			label = a.WindowName
		}
		if len(label) > 12 {
			label = label[:12]
		}
		b.WriteString(fmt.Sprintf(" [%d] %s %s\n", idx, label, a.AgentType))
		idx++
	}
	b.WriteString("\n/use <序号> 接管")
	return b.String()
}
