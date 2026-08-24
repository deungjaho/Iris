package wechat

// tmux send-keys：给已有 tmux pane 发送消息。
// 不改变 Iris 的消息转发目标，只是单向投递。

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// SendKeysToPane 给 tmux pane 发送消息
// sshHost 为空时本地执行
// paneID: tmux pane ID，如 %5
// text: 要发送的文本
func SendKeysToPane(ctx context.Context, sshHost, paneID, text string) error {
	if paneID == "" {
		return fmt.Errorf("pane ID is required")
	}
	if text == "" {
		return fmt.Errorf("text is required")
	}

	// tmux send-keys -t <pane> "<text>" Enter
	args := []string{"send-keys", "-t", paneID, text, "Enter"}

	if sshHost == "" {
		// 本地执行
		cmd := exec.CommandContext(ctx, "tmux", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tmux send-keys: %w (output: %s)", err, truncate(string(output), 100))
		}
		return nil
	}

	// 远程执行
	remoteCmd := fmt.Sprintf("tmux %s", strings.Join(quoteArgs(args), " "))
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "ConnectTimeout=10",
		sshHost,
		"bash -lc "+shellQuote(remoteCmd))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ssh tmux send-keys: %w (output: %s)", err, truncate(string(output), 100))
	}
	return nil
}

// ListLocalPanes 列出本地 tmux pane（不通过 SSH）
// 复用 TmuxAgent 结构，但本地执行
func ListLocalPanes(ctx context.Context) ([]TmuxAgent, error) {
	script := localPaneScript()
	cmd := exec.CommandContext(ctx, "python3", "-c", script)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list local panes: %w", err)
	}
	var agents []TmuxAgent
	if err := json.Unmarshal(output, &agents); err != nil {
		return nil, fmt.Errorf("parse local panes: %w (raw: %s)", err, truncate(string(output), 200))
	}
	return agents, nil
}

// localPaneScript 返回本地 pane 查询的 Python 脚本
// 与 tmux_sessions.go 的远程脚本逻辑相同，但不通过 SSH
func localPaneScript() string {
	return `
import json, os, sys, subprocess, glob

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

    if cmd not in ("claude", "devin", "node", "codex"):
        continue

    agent_type = cmd
    if cmd == "node":
        try:
            ps = subprocess.run(["ps", "-p", pane_pid, "-o", "args="],
                              capture_output=True, text=True, timeout=2)
            if "claude" in ps.stdout:
                agent_type = "claude"
            else:
                continue
        except:
            continue

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
}

// quoteArgs 给 args 加引号（用于 SSH 远程命令）
func quoteArgs(args []string) []string {
	result := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t'\"") {
			result[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		} else {
			result[i] = a
		}
	}
	return result
}
