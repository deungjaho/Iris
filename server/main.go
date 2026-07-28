package main

// Iris server: 微信通道 v1
// 长轮询 ilink API 拉消息，每收到消息 spawn claude --print，转发输出到微信
// 支持多 session 管理、斜杠命令、session resume

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"iris/wechat"
)

// Config Iris 配置
type Config struct {
	Account      wechat.Account      `json:"account"`
	Cwd          string              `json:"cwd"`
	SystemPrompt string              `json:"systemPrompt,omitempty"`
	DefaultMode  string              `json:"defaultMode,omitempty"` // 默认 acceptEdits
	Remote       wechat.RemoteConfig `json:"remote,omitempty"`      // 远程 SSH 配置
	Home         HomeConfig          `json:"home,omitempty"`        // Home session 配置
	MnemosURL    string              `json:"mnemosURL,omitempty"`   // Mnemos 向量库地址（预留）
}

// HomeConfig Home session 配置
type HomeConfig struct {
	Cwd          string `json:"cwd,omitempty"`          // 沙盒目录
	Mode         string `json:"mode,omitempty"`         // 默认 acceptEdits
	SystemPrompt string `json:"systemPrompt,omitempty"` // Home agent 专属 prompt
	APIKey       string `json:"apiKey,omitempty"`       // 专用 hydra API key（用于 per-key 调度策略）
}

var (
	configPath = flag.String("config", "", "config file path (default: ~/.config/iris/config.json)")
	cwd        = flag.String("cwd", "", "working directory for claude (default: current dir)")
	sysPrompt  = flag.String("system-prompt", "", "append system prompt for claude")
	verbose    = flag.Bool("v", false, "verbose logging")
)

const MaxSessionsPerUser = 20

func main() {
	if len(os.Args) > 1 && os.Args[1] == "login" {
		runLogin()
		return
	}

	flag.Parse()

	// 默认配置路径
	cfgPath := *configPath
	if cfgPath == "" {
		home, _ := os.UserHomeDir()
		cfgPath = filepath.Join(home, ".config", "iris", "config.json")
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if *cwd != "" {
		cfg.Cwd = *cwd
	}
	if cfg.Cwd == "" {
		cfg.Cwd, _ = os.Getwd()
	}
	if *sysPrompt != "" {
		cfg.SystemPrompt = *sysPrompt
	}
	if cfg.DefaultMode == "" {
		cfg.DefaultMode = "acceptEdits"
	}

	if cfg.Account.Token == "" || cfg.Account.BaseURL == "" {
		log.Fatalf("config missing account.token or account.baseUrl")
	}

	log.Printf("Iris starting: cwd=%s user=%s mode=%s", cfg.Cwd, cfg.Account.UserID, cfg.DefaultMode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 信号处理
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("shutting down...")
		cancel()
	}()

	client := wechat.NewClient(cfg.Account)

	// 通知微信端 bot 上线
	if err := client.NotifyStart(ctx); err != nil {
		log.Printf("notifyStart warning: %v", err)
	}
	defer client.NotifyStop(context.Background())

	// session 管理：每个 from_user_id 有多个 session
	sessions := newSessionManager(client, cfg)

	// 启动时先 drain 历史消息，避免重复处理
	log.Printf("draining historical messages...")
	for {
		resp, err := client.GetUpdates(ctx)
		if err != nil {
			log.Printf("drain error: %v", err)
			break
		}
		if len(resp.Msgs) == 0 {
			log.Printf("drain done (no backlog)")
			break
		}
		log.Printf("drain: skipped %d historical msgs", len(resp.Msgs))
	}

	// 长轮询循环
	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()

	for {
		select {
		case <-pollCtx.Done():
			return
		default:
		}

		resp, err := client.GetUpdates(pollCtx)
		if err != nil {
			if pollCtx.Err() != nil {
				return
			}
			log.Printf("getupdates error: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}

		if *verbose && len(resp.Msgs) > 0 {
			log.Printf("getupdates: %d msgs", len(resp.Msgs))
		}

		for _, msg := range resp.Msgs {
			text := msg.ExtractText()
			if text == "" {
				continue
			}
			// 更新 context token
			if msg.ContextToken != "" {
				client.UpdateContextToken(msg.ContextToken)
			}
			from := msg.FromUserID
			if from == "" {
				continue
			}
			log.Printf("msg from %s: %s", from, truncate(text, 60))

			// 分发到 session manager
			sessions.HandleMessage(ctx, from, text)
		}
	}
}

// === Session 管理 ===

// sessionInfo 存储一个 claude session 的元数据
type sessionInfo struct {
	ID         string // claude session_id（首次运行后捕获）
	Label      string // 自动从首条消息生成或 /new 时指定
	CreatedAt  time.Time
	LastActive time.Time
	Mode       string // acceptEdits / plan / normal
	IsRemote   bool   // 是否在远程机器上跑
	RemoteCwd  string // 远程工作目录（仅 IsRemote 时有效）
	LocalCwd   string // 本地工作目录（非 remote 且非 home 时有效）
	IsHome     bool   // 是否为 home session（受保护，不可删除）
}

// runningSession 跟踪正在运行的 bridge
type runningSession struct {
	replyChan chan string
	cancel    context.CancelFunc
	info      *sessionInfo
	bridge    *wechat.Bridge
}

// userState 一个微信用户的所有 session 状态
type userState struct {
	sessions           []*sessionInfo
	activeIdx          int // -1 表示无活动 session
	running            *runningSession
	lastRemoteSessions  []wechat.RemoteSession // :ls m 缓存
	lastOmarchySessions []wechat.RemoteSession // :ls o 缓存
	lastTmuxAgents      []wechat.TmuxAgent     // :ls t 缓存
}

// sessionManager 管理所有用户的 session
type sessionManager struct {
	client *wechat.Client
	cfg    *Config
	mu     sync.Mutex
	users  map[string]*userState
}

func newSessionManager(client *wechat.Client, cfg *Config) *sessionManager {
	return &sessionManager{
		client: client,
		cfg:    cfg,
		users:  make(map[string]*userState),
	}
}

func (sm *sessionManager) getOrCreateUser(from string) *userState {
	user := sm.users[from]
	if user == nil {
		user = &userState{activeIdx: -1}
		// 自动创建 home session（index 0）
		home := sm.createHomeSession()
		user.sessions = append(user.sessions, home)
		user.activeIdx = 0
		sm.users[from] = user
	}
	return user
}

// createHomeSession 创建 home session
func (sm *sessionManager) createHomeSession() *sessionInfo {
	homeCwd := expandHomePath(sm.cfg.Home.Cwd, ".local/share/iris/sandbox")
	if err := os.MkdirAll(homeCwd, 0755); err != nil {
		log.Printf("createHomeSession: MkdirAll failed: %v", err)
	}

	homeMode := sm.cfg.Home.Mode
	if homeMode == "" {
		homeMode = sm.cfg.DefaultMode
		if homeMode == "" {
			homeMode = "acceptEdits"
		}
	}

	homePrompt := sm.cfg.Home.SystemPrompt
	if homePrompt == "" {
		homePrompt = "你是 Iris 的 home agent，处理日常查询和临时任务。回答简洁（3行以内），主动执行，不要长篇解释。不确定时说不确定。"
	}

	return &sessionInfo{
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
		Label:      "🏠 home",
		Mode:       homeMode,
		IsHome:     true,
	}
}

// HandleMessage 处理来自微信用户的消息
// 前缀体系（均支持半角/全角）：
//   : / ：  — Iris 系统命令（:new :list :mode 等）
//   / / ／  — 转发给 Claude Code（/help /clear 等）
//   ! / ！  — 直接执行 bash 命令
//   @ / ＠  — 切换目标/上下文（@mac @omarchy @t0 @s0）
//   $ / ＄  — 快速状态查询
func (sm *sessionManager) HandleMessage(ctx context.Context, from, text string) {
	if len(text) == 0 {
		return
	}

	// 检查前缀（支持半角/全角）
	switch {
	case strings.HasPrefix(text, ":") || strings.HasPrefix(text, "："):
		sm.handleSysCommand(from, text)
		return
	case strings.HasPrefix(text, "/") || strings.HasPrefix(text, "／"):
		// / 开头：转发给 Claude Code
		sm.mu.Lock()
		user := sm.getOrCreateUser(from)
		running := user.running != nil
		sm.mu.Unlock()
		if !running {
			sm.client.SendMessage(context.Background(), from,
				"⚠️ 没有运行中的会话\n用 `:new` 创建或发消息开始对话")
			return
		}
		sm.forwardToSession(ctx, from, text)
		return
	case strings.HasPrefix(text, "!") || strings.HasPrefix(text, "！"):
		sm.handleBashCommand(from, text)
		return
	case strings.HasPrefix(text, "@") || strings.HasPrefix(text, "＠"):
		sm.handleSwitchTarget(from, text)
		return
	case strings.HasPrefix(text, "$") || strings.HasPrefix(text, "＄"):
		sm.handleQuickStatus(from, text)
		return
	}

	// 普通消息：转发给 session
	sm.forwardToSession(ctx, from, text)
}

// forwardToSession 转发消息到当前 session（运行中或新建）
func (sm *sessionManager) forwardToSession(ctx context.Context, from, text string) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)

	// 有运行中的 session → 转发到 replyChan
	if user.running != nil {
		select {
		case user.running.replyChan <- text:
		default:
			log.Printf("replyChan full for %s, dropping: %s", from, truncate(text, 60))
		}
		user.running.info.LastActive = time.Now()
		sm.mu.Unlock()
		return
	}

	// 没有运行中的 session，需要启动一个
	// 在锁内确定 info，避免竞态
	var info *sessionInfo
	if user.activeIdx >= 0 && user.activeIdx < len(user.sessions) {
		// resume 活动 session
		info = user.sessions[user.activeIdx]
		info.LastActive = time.Now()
	} else {
		// 创建新 session
		info = &sessionInfo{
			CreatedAt:  time.Now(),
			LastActive: time.Now(),
			Label:      truncate(text, 25),
			Mode:       sm.cfg.DefaultMode,
		}
		user.sessions = append(user.sessions, info)
		user.activeIdx = len(user.sessions) - 1
		sm.trimSessions(user)
		// trimSessions 可能调整 activeIdx，重新获取 info
		if user.activeIdx >= 0 && user.activeIdx < len(user.sessions) {
			info = user.sessions[user.activeIdx]
		}
	}

	// 标记正在启动，防止并发启动同一 session
	// 用 running 占位，startSession 会替换为真正的 running
	// 这里不能直接设 running，因为 bridge 还没创建
	// 改为：在锁内调用 startSession 的锁内部分
	sm.mu.Unlock()

	// 启动 session（startSession 内部会重新加锁并检查）
	sm.startSession(ctx, from, info, text)
}

// startSession 启动一个 bridge session
func (sm *sessionManager) startSession(ctx context.Context, from string, info *sessionInfo, firstMsg string) {
	sessCtx, sessCancel := context.WithCancel(ctx)
	replyChan := make(chan string, 10)

	// 确定 CWD：home session 用 homeCwd，本地 session 用 LocalCwd，否则用 cfg.Cwd
	cwd := sm.cfg.Cwd
	systemPrompt := sm.cfg.SystemPrompt
	var apiKey string
	if info.IsHome {
		cwd = expandHomePath(sm.cfg.Home.Cwd, ".local/share/iris/sandbox")
		if sm.cfg.Home.SystemPrompt != "" {
			systemPrompt = sm.cfg.Home.SystemPrompt
		}
		apiKey = sm.cfg.Home.APIKey
	} else if info.LocalCwd != "" {
		cwd = info.LocalCwd
	}

	// 构建 remote 配置（仅当 session 标记为 remote）
	var remote *wechat.RemoteConfig
	if info.IsRemote {
		remote = &wechat.RemoteConfig{
			Host:    sm.cfg.Remote.Host,
			Cwd:     info.RemoteCwd,
			Enabled: true,
		}
	}

	bridge := wechat.NewBridge(sm.client, from, cwd, info.Mode, info.ID, systemPrompt, remote, apiKey)

	running := &runningSession{
		replyChan: replyChan,
		cancel:    sessCancel,
		info:      info,
		bridge:    bridge,
	}

	// 加锁设置 running，先停止已有 session
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	if user.running != nil {
		// 已有运行中的 session，停止它
		user.running.cancel()
	}
	user.running = running
	sm.mu.Unlock()

	// 发状态头
	machine := "💻 omarchy"
	statusCwd := cwd // 用 startSession 里确定的 cwd
	if info.IsRemote {
		machine = "🖥 mac"
		statusCwd = info.RemoteCwd
		if statusCwd == "" {
			statusCwd = sm.cfg.Remote.Cwd
		}
	}
	if info.IsHome {
		machine = "🏠 home"
	}
	if info.LocalCwd != "" && !info.IsRemote {
		statusCwd = info.LocalCwd
	}
	mode := info.Mode
	if mode == "" {
		mode = "acceptEdits"
	}
	label := info.Label
	if label == "" {
		label = "(未命名)"
	}
	resumeTag := ""
	if info.ID != "" {
		resumeTag = " resume"
	}
	sm.client.SendMessage(context.Background(), from,
		fmt.Sprintf("%s `%s` | `%s` | %s%s", machine, statusCwd, mode, label, resumeTag))

	go func() {
		defer sessCancel()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC in bridge.Run for %s: %v", from, r)
				sm.client.SendMessage(context.Background(), from, "❌ 内部错误（已恢复）")
			}
		}()

		err := bridge.Run(sessCtx, firstMsg, replyChan)
		if err != nil && sessCtx.Err() == nil {
			log.Printf("bridge error for %s: %v", from, err)
			sm.client.SendMessage(context.Background(), from, fmt.Sprintf("❌ 内部错误: %v", err))
		}

		// 捕获 session ID（加锁，防止 info 被删除后悬空）
		sm.mu.Lock()
		if sid := bridge.SessionID(); sid != "" {
			// 检查 info 是否还在 user.sessions 中
			if u := sm.users[from]; u != nil {
				for _, s := range u.sessions {
					if s == info {
						info.ID = sid
						break
					}
				}
			}
		}
		// 标记为不再运行
		if u := sm.users[from]; u != nil && u.running == running {
			u.running = nil
		}
		sm.mu.Unlock()

		log.Printf("session ended for %s (sid=%s)", from, truncate(info.ID, 12))
	}()
}

// stopRunningLocked 停止运行中的 session（调用者需持锁）
func (sm *sessionManager) stopRunningLocked(from string) {
	user := sm.users[from]
	if user == nil || user.running == nil {
		return
	}
	running := user.running
	user.running = nil
	running.cancel()
}

// expandHomePath 展开 ~ 前缀，fallback 为默认值
func expandHomePath(cwd, fallback string) string {
	if cwd == "" {
		cwd = fallback
	}
	if cwd == "~" {
		homeDir, _ := os.UserHomeDir()
		return homeDir
	}
	if strings.HasPrefix(cwd, "~/") {
		homeDir, _ := os.UserHomeDir()
		return homeDir + cwd[1:]
	}
	return cwd
}

// trimSessions 限制 session 数量，删除最旧的非活动 session
func (sm *sessionManager) trimSessions(user *userState) {
	for len(user.sessions) > MaxSessionsPerUser {
		// 找最旧的非活动、非 home session 删除
		oldestIdx := -1
		var oldestTime time.Time
		for i, s := range user.sessions {
			if i == user.activeIdx || s.IsHome {
				continue
			}
			if oldestIdx == -1 || s.LastActive.Before(oldestTime) {
				oldestIdx = i
				oldestTime = s.LastActive
			}
		}
		if oldestIdx == -1 {
			break // 全是活动 session，不删
		}
		user.sessions = append(user.sessions[:oldestIdx], user.sessions[oldestIdx+1:]...)
		if user.activeIdx > oldestIdx {
			user.activeIdx--
		}
	}
}

// === Bash 直接执行 ===

// handleBashCommand 处理 ! 或 ！前缀的消息，直接执行 bash 命令
// 根据当前 session 的 IsRemote 决定在本地还是远程执行
func (sm *sessionManager) handleBashCommand(from, text string) {
	// 去掉前缀（支持 ! 和 ！）
	cmdStr := strings.TrimPrefix(text, "!")
	cmdStr = strings.TrimPrefix(cmdStr, "！")
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		sm.client.SendMessage(context.Background(), from, "用法: ! <命令>\n例: !ls -la\n例: !git status")
		return
	}

	// 检查当前 session 是否远程模式
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	isRemote := false
	remoteCwd := ""
	if user.activeIdx >= 0 && user.activeIdx < len(user.sessions) {
		isRemote = user.sessions[user.activeIdx].IsRemote
		remoteCwd = user.sessions[user.activeIdx].RemoteCwd
	}
	sm.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var output []byte
	var err error
	if isRemote && sm.cfg.Remote.Host != "" {
		// 远程执行
		cwd := remoteCwd
		if cwd == "" {
			cwd = sm.cfg.Remote.Cwd
		}
		remoteCmd := fmt.Sprintf("cd '%s' && %s", cwd, cmdStr)
		cmd := exec.CommandContext(ctx, "ssh",
			"-o", "ConnectTimeout=10",
			sm.cfg.Remote.Host,
			"bash -lc "+shellQuote(remoteCmd))
		output, err = cmd.CombinedOutput()
	} else {
		// 本地执行
		cmd := exec.CommandContext(ctx, "bash", "-c", cmdStr)
		cmd.Dir = sm.cfg.Cwd
		output, err = cmd.CombinedOutput()
	}

	// 格式化输出
	var msg string
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if ok {
			msg = fmt.Sprintf("❌ 退出码 %d\n%s", exitErr.ExitCode(), truncate(string(output), 1500))
		} else {
			msg = fmt.Sprintf("❌ %v\n%s", err, truncate(string(output), 1500))
		}
	} else {
		out := strings.TrimRight(string(output), "\n")
		if out == "" {
			msg = "✅ (无输出)"
		} else {
			msg = "✅\n" + truncate(out, 1500)
		}
	}
	sm.client.SendMessage(context.Background(), from, msg)
}

// === @ 切换目标 ===

// handleSwitchTarget 处理 @ 前缀消息，:go 的快捷方式
// @        — 回 home
// @m0      — 接管 mac session 0
// @o0      — 接管 omarchy session 0
// @t0      — 接管 tmux agent 0
// @0       — 切到 Iris session 0
// @<标签>  — 按标签模糊匹配
func (sm *sessionManager) handleSwitchTarget(from, text string) {
	// 全角 ＠ 转半角 @
	text = strings.Replace(text, "＠", "@", 1)
	arg := strings.TrimSpace(text[1:])

	if arg == "" {
		// @ = 回 home
		sm.goHome(from)
		return
	}

	// 转发给 :go 处理
	sm.cmdGo(from, arg)
}

// === $ 状态查询 ===

// handleQuickStatus 处理 $ 前缀消息，快速显示状态
func (sm *sessionManager) handleQuickStatus(from, text string) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)

	// 机器 + session + 模式 + 运行状态
	machine := "💻 omarchy"
	sessInfo := "无会话"
	runStatus := "⏸ 暂停"

	if user.activeIdx >= 0 && user.activeIdx < len(user.sessions) {
		info := user.sessions[user.activeIdx]
		if info.IsRemote {
			machine = "🖥 mac"
		} else if info.IsHome {
			machine = "🏠 home"
		}
		mode := info.Mode
		if mode == "" {
			mode = "acceptEdits"
		}
		label := info.Label
		if label == "" {
			label = "(无标题)"
		}
		sessInfo = fmt.Sprintf("`[%d]` %s `%s`", user.activeIdx, label, mode)
		if info.ID != "" {
			sessInfo += fmt.Sprintf(" sid:`%s`", truncate(info.ID, 8))
		}
		if user.running != nil {
			runStatus = "▶ 运行中"
		}
	}
	totalSessions := len(user.sessions)
	sm.mu.Unlock()

	msg := fmt.Sprintf("%s | %s | %s | 共%d个",
		machine, sessInfo, runStatus, totalSessions)
	sm.client.SendMessage(context.Background(), from, msg)
}

// === 系统命令（: 前缀）===

func (sm *sessionManager) handleSysCommand(from, text string) {
	// 全角 ： 转半角 :
	text = strings.Replace(text, "：", ":", 1)
	parts := strings.SplitN(text, " ", 2)
	cmd := parts[0]
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	switch cmd {
	case ":help", ":?", ":h":
		sm.sendHelp(from)
	case ":ls", ":list":
		sm.cmdList(from, args)
	case ":go":
		sm.cmdGo(from, args)
	case ":new":
		sm.cmdNew(from, args)
	case ":rm", ":del":
		sm.cmdDeleteSession(from, args)
	case ":mode":
		sm.cmdSetMode(from, args)
	case ":stop":
		sm.cmdStop(from)
	case ":status", ":st":
		sm.cmdStatus(from)
	default:
		sm.client.SendMessage(context.Background(), from,
			fmt.Sprintf("未知命令: `%s`\n发 `:help` 查看", cmd))
	}
}

func (sm *sessionManager) sendHelp(from string) {
	help := strings.Join([]string{
		"**🤖 Iris 命令**",
		"",
		"| 前缀 | 含义 |",
		"|------|------|",
		"| `:` | 系统命令 |",
		"| `/` | 传给Claude |",
		"| `!` | 执行bash |",
		"| `@` | 快捷切换 |",
		"| `$` | 状态查询 |",
		"",
		"**命令**",
		"| 命令 | 说明 |",
		"|------|------|",
		"| `:ls [m/o/t]` | 列出session |",
		"| `:go <目标>` | 切换/接管 |",
		"| `:new m/o <dir>` | 新建session |",
		"| `:rm <n>` | 删除session |",
		"| `:mode <m>` | 切换模式 |",
		"| `:stop` | 停止会话 |",
		"| `:status` | 查看状态 |",
		"",
		"**目标格式**",
		"| 目标 | 含义 |",
		"|------|------|",
		"| `home` | 回home |",
		"| `m0` `o0` | mac/omarchy session |",
		"| `t0` | tmux agent |",
		"| `0` | Iris session 0 |",
		"",
		"**示例**",
		"`:ls m` 列出Mac session",
		"`:go m0` 接管Mac session 0",
		"`@t0` 接管tmux agent 0",
		"`@` 回home",
		"`:new m ~/Work` Mac新建",
		"",
		"直接发消息即可对话",
		"发 `stop` 可中断生成",
	}, "\n")
	sm.client.SendMessage(context.Background(), from, help)
}

// === :ls 列出 session ===

func (sm *sessionManager) cmdList(from, args string) {
	loc := strings.TrimSpace(args)
	switch loc {
	case "", "home", "h":
		sm.cmdListIris(from)
	case "m", "mac":
		sm.cmdListMachineSessions(from, "mac")
	case "o", "omarchy":
		sm.cmdListMachineSessions(from, "omarchy")
	case "t", "tmux":
		sm.cmdListTmux(from)
	default:
		sm.client.SendMessage(context.Background(), from,
			fmt.Sprintf("用法: `:ls [m/o/t]`\n`m`=mac `o`=omarchy `t`=tmux"))
	}
}

// cmdListIris 列出 Iris 管理的 session
func (sm *sessionManager) cmdListIris(from string) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	if len(user.sessions) == 0 {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from, "没有会话")
		return
	}

	var b strings.Builder
	b.WriteString("| # | 标签 | 模式 | 位置 | 时间 |\n")
	b.WriteString("|---|------|------|------|------|\n")
	for i, s := range user.sessions {
		marker := ""
		if i == user.activeIdx {
			marker = "**"
		}
		running := ""
		if i == user.activeIdx && user.running != nil {
			running = "▶"
		}
		label := s.Label
		if label == "" {
			label = "(未命名)"
		}
		mode := s.Mode
		if mode == "" {
			mode = "acceptEdits"
		}
		loc := "💻"
		if s.IsRemote {
			loc = "🖥"
		}
		if s.IsHome {
			loc = "🏠"
		}
		timeStr := s.LastActive.Format("01-02 15:04")
		b.WriteString(fmt.Sprintf("| %s%d%s%s | %s | `%s` | %s | %s |\n",
			marker, i, marker, running, label, mode, loc, timeStr))
	}
	sm.mu.Unlock()

	sm.client.SendMessage(context.Background(), from, b.String())
}

// cmdListMachineSessions 列出指定机器上的 claude session
func (sm *sessionManager) cmdListMachineSessions(from, machine string) {
	if machine == "mac" && sm.cfg.Remote.Host == "" {
		sm.client.SendMessage(context.Background(), from, "未配置远程主机")
		return
	}

	// 确定查询参数
	var sshHost, cwd string
	if machine == "mac" {
		sshHost = sm.cfg.Remote.Host
		cwd = sm.cfg.Remote.Cwd
		// 如果当前活动 session 是远程的，用它的 cwd
		sm.mu.Lock()
		user := sm.getOrCreateUser(from)
		if user.activeIdx >= 0 && user.activeIdx < len(user.sessions) {
			if user.sessions[user.activeIdx].IsRemote && user.sessions[user.activeIdx].RemoteCwd != "" {
				cwd = user.sessions[user.activeIdx].RemoteCwd
			}
		}
		sm.mu.Unlock()
	} else {
		// omarchy 本地
		sshHost = ""
		cwd = sm.cfg.Cwd
	}

	if cwd == "" {
		cwd = "."
	}

	sm.client.SendMessage(context.Background(), from, fmt.Sprintf("🔍 查询 %s session...", machine))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var sessions []wechat.RemoteSession
	var err error
	if machine == "mac" {
		sessions, err = wechat.ListRemoteSessions(ctx, sshHost, cwd)
	} else {
		// omarchy 本地查询——直接调 ListRemoteSessions 但 sshHost 为空时本地执行
		// 简单做法：用本地 python
		sessions, err = sm.listLocalSessions(ctx, cwd)
	}
	if err != nil {
		sm.client.SendMessage(context.Background(), from,
			fmt.Sprintf("❌ 查询失败: %v", err))
		return
	}

	if len(sessions) == 0 {
		sm.client.SendMessage(context.Background(), from,
			fmt.Sprintf("📋 %s 没有session", machine))
		return
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("📋 %s session (%d个)\n\n", machine, len(sessions)))
	b.WriteString("| # | 标签 | SID | 时间 |\n")
	b.WriteString("|---|------|-----|------|\n")
	for i, s := range sessions {
		title := truncate(s.Title, 25)
		if title == "" {
			title = "(无标题)"
		}
		sid := truncate(s.ID, 8)
		b.WriteString(fmt.Sprintf("| %d | %s | `%s` | %s |\n", i, title, sid, s.Modified))
	}
	b.WriteString(fmt.Sprintf("\n`:go %s<n>` 接管", machine[0:1]))
	sm.client.SendMessage(context.Background(), from, b.String())

	// 缓存查询结果
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	if machine == "mac" {
		user.lastRemoteSessions = sessions
		user.lastTmuxAgents = nil // 清空 tmux 缓存，避免序号混混
	} else {
		user.lastOmarchySessions = sessions
		user.lastTmuxAgents = nil
	}
	sm.mu.Unlock()
}

// listLocalSessions 查询本地 omarchy 上的 claude session
func (sm *sessionManager) listLocalSessions(ctx context.Context, cwd string) ([]wechat.RemoteSession, error) {
	// 复用 wechat.ListRemoteSessions 的逻辑，但本地执行
	// 简单做法：直接调 python
	encoded := strings.ReplaceAll(cwd, "/", "-")
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

	cmd := exec.CommandContext(ctx, "python3", "-c", script)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("local list: %w", err)
	}

	var raw []struct {
		ID    string  `json:"id"`
		Title string  `json:"title"`
		Mtime float64 `json:"mtime"`
	}
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	var sessions []wechat.RemoteSession
	for _, r := range raw {
		sessions = append(sessions, wechat.RemoteSession{
			ID:       r.ID,
			Title:    r.Title,
			Project:  encoded,
			Modified: time.Unix(int64(r.Mtime), 0).Format("01-02 15:04"),
		})
	}
	return sessions, nil
}

// cmdListTmux 列出 tmux agent
func (sm *sessionManager) cmdListTmux(from string) {
	if sm.cfg.Remote.Host == "" {
		sm.client.SendMessage(context.Background(), from, "未配置远程主机")
		return
	}

	sm.client.SendMessage(context.Background(), from, "🔍 查询 tmux agent...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	agents, err := wechat.ListTmuxAgents(ctx, sm.cfg.Remote.Host)
	if err != nil {
		sm.client.SendMessage(context.Background(), from,
			fmt.Sprintf("❌ 查询失败: %v", err))
		return
	}

	msg := wechat.FormatTmuxAgents(agents)
	sm.client.SendMessage(context.Background(), from, msg)

	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	user.lastTmuxAgents = agents
	sm.mu.Unlock()
}

// === :go 切换/接管 ===

func (sm *sessionManager) cmdGo(from, args string) {
	target := strings.TrimSpace(args)
	if target == "" {
		sm.client.SendMessage(context.Background(), from,
			"用法: `:go <目标>`\n`home` `m0` `o0` `t0` `0`")
		return
	}

	// :go home / :go h
	if target == "home" || target == "h" {
		sm.goHome(from)
		return
	}

	// :go m<n> — mac session
	if strings.HasPrefix(target, "m") {
		nStr := target[1:]
		if n, err := strconv.Atoi(nStr); err == nil {
			sm.goMachineSession(from, "mac", n)
			return
		}
	}

	// :go o<n> — omarchy session
	if strings.HasPrefix(target, "o") {
		nStr := target[1:]
		if n, err := strconv.Atoi(nStr); err == nil {
			sm.goMachineSession(from, "omarchy", n)
			return
		}
	}

	// :go t<n> — tmux agent
	if strings.HasPrefix(target, "t") {
		nStr := target[1:]
		if n, err := strconv.Atoi(nStr); err == nil {
			sm.goTmuxAgent(from, n)
			return
		}
	}

	// :go <n> — Iris session
	if n, err := strconv.Atoi(target); err == nil {
		sm.goIrisSession(from, n)
		return
	}

	// :go <标签> — 模糊匹配
	sm.goIrisSessionByLabel(from, target)
}

// goHome 回 home session
func (sm *sessionManager) goHome(from string) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	if user.running != nil {
		sm.stopRunningLocked(from)
	}
	user.activeIdx = 0 // home 固定 index 0
	sm.mu.Unlock()
	sm.client.SendMessage(context.Background(), from, "🏠 回到 home\n发消息开始对话")
}

// goMachineSession 接管 mac 或 omarchy 上的 session
func (sm *sessionManager) goMachineSession(from, machine string, n int) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)

	var sessions []wechat.RemoteSession
	var cwd string
	if machine == "mac" {
		sessions = user.lastRemoteSessions
		cwd = sm.cfg.Remote.Cwd
	} else {
		sessions = user.lastOmarchySessions
		cwd = sm.cfg.Cwd
	}

	if n < 0 || n >= len(sessions) {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from,
			fmt.Sprintf("❌ 序号 %d 超出范围\n用 `:ls %s` 刷新", n, machine[0:1]))
		return
	}

	rs := sessions[n]
	isRemote := machine == "mac"
	info := &sessionInfo{
		ID:         rs.ID,
		Label:      rs.Title,
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
		Mode:       sm.cfg.DefaultMode,
		IsRemote:   isRemote,
	}
	if isRemote {
		info.RemoteCwd = cwd
	} else {
		info.LocalCwd = cwd
	}
	sm.stopRunningLocked(from)
	user.sessions = append(user.sessions, info)
	user.activeIdx = len(user.sessions) - 1
	sm.trimSessions(user)
	sm.mu.Unlock()

	locLabel := "💻 omarchy"
	if isRemote {
		locLabel = "🖥 mac"
	}
	sm.client.SendMessage(context.Background(), from,
		fmt.Sprintf("%s 接管 `%s`\nSID: `%s`\n发消息恢复对话",
			locLabel, rs.Title, truncate(rs.ID, 12)))
}

// goTmuxAgent 接管 tmux agent
func (sm *sessionManager) goTmuxAgent(from string, n int) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	if n < 0 || n >= len(user.lastTmuxAgents) {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from,
			fmt.Sprintf("❌ 序号 %d 超出范围\n用 `:ls t` 刷新", n))
		return
	}
	a := user.lastTmuxAgents[n]
	if a.SessionID == "" {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from, "❌ 该 agent 没有 session ID")
		return
	}
	info := &sessionInfo{
		ID:         a.SessionID,
		Label:      a.Title,
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
		Mode:       sm.cfg.DefaultMode,
		IsRemote:   true,
		RemoteCwd:  a.Cwd,
	}
	sm.stopRunningLocked(from)
	user.sessions = append(user.sessions, info)
	user.activeIdx = len(user.sessions) - 1
	sm.trimSessions(user)
	sm.mu.Unlock()

	sm.client.SendMessage(context.Background(), from,
		fmt.Sprintf("🖥 接管 `%s:%s` %s\nSID: `%s`\n发消息恢复对话",
			a.TmuxSession, a.WindowIdx, a.Title, truncate(a.SessionID, 12)))
}

// goIrisSession 切到 Iris 管理的 session
func (sm *sessionManager) goIrisSession(from string, n int) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	if n < 0 || n >= len(user.sessions) {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from,
			fmt.Sprintf("❌ 序号 %d 超出范围\n用 `:ls` 查看", n))
		return
	}
	sm.stopRunningLocked(from)
	user.activeIdx = n
	label := user.sessions[n].Label
	mode := user.sessions[n].Mode
	if mode == "" {
		mode = "acceptEdits"
	}
	sm.mu.Unlock()

	sm.client.SendMessage(context.Background(), from,
		fmt.Sprintf("🔄 切到 `[%d]` %s `%s`\n发消息恢复对话", n, label, mode))
}

// goIrisSessionByLabel 按标签模糊匹配
func (sm *sessionManager) goIrisSessionByLabel(from, label string) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	n := sm.resolveSessionIdx(user, label)
	if n < 0 {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from,
			fmt.Sprintf("找不到: `%s`\n用 `:ls` 查看", label))
		return
	}
	sm.stopRunningLocked(from)
	user.activeIdx = n
	sLabel := user.sessions[n].Label
	mode := user.sessions[n].Mode
	if mode == "" {
		mode = "acceptEdits"
	}
	sm.mu.Unlock()

	sm.client.SendMessage(context.Background(), from,
		fmt.Sprintf("🔄 切到 `[%d]` %s `%s`\n发消息恢复对话", n, sLabel, mode))
}

// === :new 新建 session ===

func (sm *sessionManager) cmdNew(from, args string) {
	parts := strings.Fields(args)
	if len(parts) == 0 {
		sm.client.SendMessage(context.Background(), from,
			"用法: `:new m/o <dir>`\n`m`=mac `o`=omarchy")
		return
	}

	machine := parts[0]
	dir := ""
	if len(parts) > 1 {
		dir = strings.Join(parts[1:], " ")
	}

	switch machine {
	case "m", "mac":
		sm.newMachineSession(from, "mac", dir)
	case "o", "omarchy":
		sm.newMachineSession(from, "omarchy", dir)
	default:
		sm.client.SendMessage(context.Background(), from,
			"用法: `:new m/o <dir>`\n`m`=mac `o`=omarchy")
	}
}

// newMachineSession 在指定机器上新建 session
func (sm *sessionManager) newMachineSession(from, machine, dir string) {
	if machine == "mac" && sm.cfg.Remote.Host == "" {
		sm.client.SendMessage(context.Background(), from, "未配置远程主机")
		return
	}

	var cwd string
	if machine == "mac" {
		cwd = dir
		if cwd == "" {
			cwd = sm.cfg.Remote.Cwd
		}
	} else {
		cwd = dir
		if cwd == "" {
			cwd = sm.cfg.Cwd
		}
	}

	label := fmt.Sprintf("%s:%s", machine, filepath.Base(cwd))

	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	info := &sessionInfo{
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
		Label:      label,
		Mode:       sm.cfg.DefaultMode,
		IsRemote:   machine == "mac",
	}
	if machine == "mac" {
		info.RemoteCwd = cwd
	} else {
		info.LocalCwd = cwd
	}
	sm.stopRunningLocked(from)
	user.sessions = append(user.sessions, info)
	user.activeIdx = len(user.sessions) - 1
	sm.trimSessions(user)
	sm.mu.Unlock()

	locLabel := "💻 omarchy"
	if machine == "mac" {
		locLabel = "🖥 mac"
	}
	sm.client.SendMessage(context.Background(), from,
		fmt.Sprintf("%s 新建 `%s`\n目录: `%s`\n发消息开始对话",
			locLabel, label, cwd))
}

func (sm *sessionManager) cmdDeleteSession(from, args string) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	n := sm.resolveSessionIdx(user, args)
	if n < 0 {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from,
			fmt.Sprintf("找不到会话: `%s`\n用 `:list` 查看", args))
		return
	}

	// 保护 home session
	if user.sessions[n].IsHome {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from,
			"🏠 home session 不可删除\n用 `@home` 回到它")
		return
	}

	if n == user.activeIdx && user.running != nil {
		sm.stopRunningLocked(from)
	}

	label := user.sessions[n].Label
	user.sessions = append(user.sessions[:n], user.sessions[n+1:]...)
	if user.activeIdx == n {
		user.activeIdx = -1
	} else if user.activeIdx > n {
		user.activeIdx--
	}
	if user.activeIdx >= len(user.sessions) {
		user.activeIdx = len(user.sessions) - 1
	}
	if len(user.sessions) == 0 {
		user.activeIdx = -1
	}
	sm.mu.Unlock()

	sm.client.SendMessage(context.Background(), from,
		fmt.Sprintf("🗑 已删除 [%d] %s", n, label))
}

// resolveSessionIdx 把参数解析为 session 序号
// 支持：纯数字（序号）、标签名（精确匹配）、标签前缀（模糊匹配）
func (sm *sessionManager) resolveSessionIdx(user *userState, args string) int {
	args = strings.TrimSpace(args)
	if args == "" {
		return -1
	}
	// 先试数字
	if n, err := strconv.Atoi(args); err == nil {
		if n >= 0 && n < len(user.sessions) {
			return n
		}
		return -1
	}
	// 标签精确匹配
	for i, s := range user.sessions {
		if s.Label == args {
			return i
		}
	}
	// 标签前缀匹配（不区分大小写）
	lower := strings.ToLower(args)
	matches := []int{}
	for i, s := range user.sessions {
		if strings.HasPrefix(strings.ToLower(s.Label), lower) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return -1
}

func (sm *sessionManager) cmdSetMode(from, args string) {
	mode := strings.TrimSpace(args)
	valid := map[string]bool{"plan": true, "acceptEdits": true, "normal": true, "default": true}
	if !valid[mode] {
		sm.client.SendMessage(context.Background(), from,
			strings.Join([]string{
				"用法: /mode <模式>",
				"plan 每次操作都问",
				"acceptEdits 自动批准编辑",
				"normal 默认策略",
			}, "\n"))
		return
	}

	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	if user.activeIdx < 0 || user.activeIdx >= len(user.sessions) {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from, "没有活动会话，先发送消息创建会话")
		return
	}

	info := user.sessions[user.activeIdx]
	oldMode := info.Mode
	if oldMode == "" {
		oldMode = "acceptEdits"
	}
	info.Mode = mode

	wasRunning := user.running != nil
	if wasRunning {
		sm.stopRunningLocked(from)
	}
	sm.mu.Unlock()

	msg := fmt.Sprintf("⚙️ 模式切换: %s → %s", oldMode, mode)
	if wasRunning {
		msg += "\n会话已暂停，发送消息以新模式恢复"
	}
	sm.client.SendMessage(context.Background(), from, msg)
}

func (sm *sessionManager) cmdStop(from string) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	if user.running == nil {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from, "没有运行中的会话")
		return
	}
	sm.stopRunningLocked(from)
	sm.mu.Unlock()

	sm.client.SendMessage(context.Background(), from,
		"⏹ 会话已停止\n发送消息可恢复对话，或 /new 开始新会话")
}

func (sm *sessionManager) cmdStatus(from string) {
	sm.mu.Lock()
	user := sm.getOrCreateUser(from)
	if user.activeIdx < 0 || user.activeIdx >= len(user.sessions) {
		sm.mu.Unlock()
		sm.client.SendMessage(context.Background(), from, "没有活动会话")
		return
	}

	info := user.sessions[user.activeIdx]
	running := "⏸ 暂停"
	if user.running != nil {
		running = "▶ 运行中"
	}
	mode := info.Mode
	if mode == "" {
		mode = "acceptEdits"
	}

	machine := "💻 omarchy"
	cwd := sm.cfg.Cwd
	if info.IsRemote {
		machine = "🖥 mac"
		cwd = info.RemoteCwd
		if cwd == "" {
			cwd = sm.cfg.Remote.Cwd
		}
	} else if info.IsHome {
		machine = "🏠 home"
		cwd = sm.cfg.Home.Cwd
		if cwd == "" {
			cwd = "~/.local/share/iris/sandbox"
		}
	} else if info.LocalCwd != "" {
		cwd = info.LocalCwd
	}

	msg := fmt.Sprintf("**📊 会话状态**\n\n| 属性 | 值 |\n|------|----|\n")
	msg += fmt.Sprintf("| 机器 | %s |\n", machine)
	msg += fmt.Sprintf("| 序号 | `[%d]` |\n", user.activeIdx)
	msg += fmt.Sprintf("| 标签 | %s |\n", info.Label)
	msg += fmt.Sprintf("| 模式 | `%s` |\n", mode)
	msg += fmt.Sprintf("| 目录 | `%s` |\n", cwd)
	msg += fmt.Sprintf("| 状态 | %s |\n", running)
	msg += fmt.Sprintf("| 创建 | %s |\n", info.CreatedAt.Format("01-02 15:04"))
	msg += fmt.Sprintf("| 最近 | %s |\n", info.LastActive.Format("01-02 15:04"))
	if info.ID != "" {
		msg += fmt.Sprintf("| SID | `%s` |\n", truncate(info.ID, 16))
	}
	totalSessions := len(user.sessions)
	sm.mu.Unlock()

	msg += fmt.Sprintf("\n共 `%d` 个会话", totalSessions)
	sm.client.SendMessage(context.Background(), from, msg)
}

// === 配置 & 登录 ===

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// shellQuote 用单引号包裹 shell 字符串
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// runLogin 生成二维码，轮询扫码，保存凭证到 config.json
func runLogin() {
	home, _ := os.UserHomeDir()
	cfgDir := filepath.Join(home, ".config", "iris")
	cfgPath := filepath.Join(cfgDir, "config.json")
	os.MkdirAll(cfgDir, 0755)

	// 读取已有 config 拿 local_token_list 和其他配置
	var existing Config
	if data, err := os.ReadFile(cfgPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	baseURL := "https://ilinkai.weixin.qq.com"
	var localTokens []string
	if existing.Account.Token != "" {
		localTokens = append(localTokens, existing.Account.Token)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Println("请求二维码...")
	qr, err := wechat.FetchQRCode(ctx, baseURL, localTokens)
	if err != nil {
		log.Fatalf("fetch qrcode: %v", err)
	}

	fmt.Printf("\n请用微信扫码登录:\n%s\n\n", qr.QRCodeImgContent)
	fmt.Println("等待扫码...")

	status, err := wechat.PollQRStatus(ctx, baseURL, qr.QRCode)
	if err != nil {
		log.Fatalf("poll status: %v", err)
	}

	fmt.Printf("\n登录成功!\n")
	fmt.Printf("  bot_token: %s\n", status.BotToken)
	fmt.Printf("  user_id:   %s\n", status.IlinkUserID)
	fmt.Printf("  base_url:  %s\n", status.BaseURL)

	// 保存到 config.json，保留 cwd 和 systemPrompt
	cfg := Config{
		Account: wechat.Account{
			Token:   status.BotToken,
			BaseURL: status.BaseURL,
			UserID:  status.IlinkUserID,
		},
		Cwd:          existing.Cwd,
		SystemPrompt: existing.SystemPrompt,
		DefaultMode:  existing.DefaultMode,
		Remote:       existing.Remote,
		Home:         existing.Home,
		MnemosURL:    existing.MnemosURL,
	}
	if cfg.Cwd == "" {
		cfg.Cwd, _ = os.Getwd()
	}
	if cfg.DefaultMode == "" {
		cfg.DefaultMode = "acceptEdits"
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		log.Fatalf("write config: %v", err)
	}
	fmt.Printf("\n凭证已保存到 %s\n", cfgPath)
}
