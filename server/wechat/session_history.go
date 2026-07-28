package wechat

// 读取 claude session 的历史记录
// 通过 SSH 读远程 ~/.claude/projects/<路径编码>/<session_id>.jsonl
// 提取 user/assistant 消息，格式化输出

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// SessionHistory 获取远程 session 的历史消息
// sshHost: SSH 别名
// cwd: 远程工作目录（用于确定 projects 子目录）
// sessionID: claude session ID
// limit: 最多提取多少条消息
func SessionHistory(ctx context.Context, sshHost, cwd, sessionID string, limit int) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("没有 session ID")
	}
	if limit <= 0 {
		limit = 20
	}

	// 路径编码
	encoded := encodePath(cwd)

	script := fmt.Sprintf(`
import json, os, sys

sid = %q
encoded = %q
project_dir = os.path.expanduser("~/.claude/projects/" + encoded)
fpath = os.path.join(project_dir, sid + ".jsonl")
if not os.path.isfile(fpath):
    print("[]")
    sys.exit(0)

messages = []
try:
    with open(fpath) as fh:
        for line in fh:
            if not line.strip():
                continue
            try:
                d = json.loads(line)
            except:
                continue
            t = d.get("type", "")
            if t == "user":
                msg = d.get("message", {})
                content = msg.get("content", "")
                text = ""
                if isinstance(content, list):
                    for c in content:
                        if c.get("type") == "text":
                            text = c.get("text", "")
                            break
                elif isinstance(content, str):
                    text = content
                if text and not text.startswith("<"):
                    messages.append({"role": "user", "text": text[:200]})
            elif t == "assistant":
                msg = d.get("message", {})
                content = msg.get("content", [])
                text = ""
                if isinstance(content, list):
                    for c in content:
                        if c.get("type") == "text" and c.get("text"):
                            text = c["text"]
                            break
                if text:
                    messages.append({"role": "assistant", "text": text[:300]})
except:
    pass

# 只取最后 N 条
if len(messages) > %d:
    messages = messages[-%d:]
print(json.dumps(messages, ensure_ascii=False))
`, sessionID, encoded, limit, limit)

	args := []string{
		"-o", "ConnectTimeout=10",
		sshHost,
		"bash -lc " + shellQuote("python3 -c "+shellQuote(script)),
	}

	cmd := exec.CommandContext(ctx, "ssh", args...)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ssh read history: %w", err)
	}

	// 解析并格式化
	return formatHistory(output), nil
}

// encodePath 路径编码：/Users/tangtszho/Work → -Users-tangtszho-Work
func encodePath(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}

// formatHistory 把 JSON 数组格式化为微信消息
func formatHistory(jsonData []byte) string {
	type msg struct {
		Role string `json:"role"`
		Text string `json:"text"`
	}
	var msgs []msg
	if err := json.Unmarshal(jsonData, &msgs); err != nil {
		return "❌ 解析历史失败: " + string(jsonData)
	}
	if len(msgs) == 0 {
		return "📋 没有历史记录"
	}

	var b strings.Builder
	b.WriteString("📋 历史记录\n")
	for _, m := range msgs {
		prefix := "🤖 "
		if m.Role == "user" {
			prefix = "👤 "
		}
		text := m.Text
		if len([]rune(text)) > 100 {
			text = string([]rune(text)[:100]) + "..."
		}
		b.WriteString(prefix + text + "\n")
	}
	return b.String()
}
