# Iris Lab - Worker 规范

## 项目定位

Iris 是微信远程控制 Claude Code 的桥接服务（Go）。本仓库 `/home/camt/Work/Iris-lab` 是从生产 `/home/camt/Work/Iris` 脱敏复制的 Git baseline，用于安全/可靠性/评估/验证工作。

## 铁律

1. **严禁编辑 `/home/camt/Work/Iris`（生产目录）** — 那是 iris.service 正在使用的部署副本，无 Git
2. **严禁重启或替换 iris.service** — 未经备份、rollback、独立验证、部署 checkpoint
3. **所有改动只在 Iris-lab 或其 worktree 内进行**
4. **测试使用 fake WeChat/agent adapter 和非生产端口** — 不连真实 ilink API，不占用 7890 代理
5. **不提交 token/cookie/session/log/env/runtime DB** — 见 .gitignore

## 生产 SHA256 基准

`/home/camt/Work/Iris-control/audit/iris-prod-sha256.manifest` 记录了生产文件的 SHA256。任何对生产目录的非法修改可通过对比 manifest 发现。

## Worker 角色分工

| Worker | 职责 | 可写 |
|--------|------|------|
| iris-devin-security | 安全审计与修复（命令注入、鉴权、密钥泄露、日志泄露） | 是 |
| iris-devin-reliability | 可靠性改进（错误处理、重连、优雅关闭、状态持久化） | 是 |
| iris-devin-eval | 评估与测试基础设施（fake adapter、非生产端口测试用例） | 是 |
| iris-devin-verifier | 只验证不写实现 — 审查其他 worker 的产出 | 否 |

## 报告机制

每次进展写入 `/home/camt/Work/Pantheon-control/inbox/iris.md`（PROJECT_MASTER_REPORT_V1 格式）。读取 `/home/camt/Work/Pantheon-control/outbox/iris.md` 获取指令。

## 技术栈

- Go 1.26.5（`go build -o iris .` 在 server/ 目录）
- systemd user service（`~/.config/systemd/user/iris.service`）
- ilink WeChat Bot API（`https://ilinkai.weixin.qq.com`）
- claude --print stream-json 子进程协议

## 编码规范

- 不加"改动记录注释"
- diff 最小化，不加不需要的文件
- 改动理由写在回复或 commit message 里
- 移除死代码、未用 import
