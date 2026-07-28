package wechat

// 远程 session 列表查询：通过 SSH 列出远程机器上的 claude session
//
// claude session 存在 ~/.claude/projects/<路径编码>/<session_id>.jsonl
// 路径编码规则：cwd 的 / 替换为 -
// 每个 jsonl 文件第一行可能有 custom-title，否则取第一条 user message 做摘要

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// RemoteSession 远程机器上的一个 claude session
type RemoteSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Project   string `json:"project"`   // 项目路径编码
	Modified  string `json:"modified"`  // 修改时间
}

// ListRemoteSessions 通过 SSH 列出远程机器上指定 cwd 的 claude session
// sshHost: SSH 别名
// remoteCwd: 远程工作目录（用于确定 projects 子目录）
func ListRemoteSessions(ctx context.Context, sshHost, remoteCwd string) ([]RemoteSession, error) {
	// 路径编码：/Users/tangtszho/Work → -Users-tangtszho-Work
	encoded := strings.ReplaceAll(remoteCwd, "/", "-")

	// 远程脚本：列出 session 文件，提取标题或首条 user message
	script := fmt.Sprintf(`
import json, os, sys, glob

project_dir = os.path.expanduser("~/.claude/projects/%s")
if not os.path.isdir(project_dir):
    print("[]")
    sys.exit(0)

sessions = []
for f in sorted(glob.glob(project_dir + "/*.jsonl"), key=os.path.getmtime, reverse=True)[:30]:
    sid = os.path.basename(f).replace(".jsonl", "")
    title = ""
    first_msg = ""
    try:
        with open(f) as fh:
            for line in fh:
                if not line.strip():
                    continue
                try:
                    d = json.loads(line)
                except:
                    continue
                if d.get("type") == "custom-title" and not title:
                    title = d.get("customTitle", "")
                elif d.get("type") == "user" and not first_msg:
                    msg = d.get("message", {})
                    content = msg.get("content", "")
                    if isinstance(content, list):
                        for c in content:
                            if c.get("type") == "text":
                                t = c.get("text", "")
                                # 跳过内部消息（command caveat 等）
                                if t and not t.startswith("<"):
                                    first_msg = t[:40]
                                    break
                    elif isinstance(content, str):
                        if content and not content.startswith("<"):
                            first_msg = content[:40]
                if title and first_msg:
                    break
    except:
        pass
    label = title or first_msg or "(无标题)"
    mtime = os.path.getmtime(f)
    sessions.append({"id": sid, "title": label, "mtime": mtime})

print(json.dumps(sessions))
`, encoded)

	// SSH 执行 python 脚本（用 bash -lc 确保 PATH 正确）
	args := []string{
		"-o", "ConnectTimeout=10",
		sshHost,
		"bash -lc " + shellQuote("python3 -c "+shellQuote(script)),
	}

	cmd := exec.CommandContext(ctx, "ssh", args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ssh list sessions: %w", err)
	}

	var raw []struct {
		ID    string  `json:"id"`
		Title string  `json:"title"`
		Mtime float64 `json:"mtime"`
	}
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("parse sessions: %w (raw: %s)", err, string(output))
	}

	var sessions []RemoteSession
	for _, r := range raw {
		sessions = append(sessions, RemoteSession{
			ID:       r.ID,
			Title:    r.Title,
			Project:  encoded,
			Modified: time.Unix(int64(r.Mtime), 0).Format("01-02 15:04"),
		})
	}
	return sessions, nil
}
