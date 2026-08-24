package main

// 意图判断：规则匹配，不调 LLM。
// 准确率有限，自然语言会误判。用户应优先用 : 命令显式指定意图。
// 无前缀消息走意图判断只是 fallback。

import (
	"strings"
)

// Intent 意图类型
type Intent int

const (
	IntentConversation Intent = iota // 对话类（默认 fallback）
	IntentQuery                      // 查询类
	IntentDirective                  // 指令类
	IntentHelp                       // 帮助类
)

// DetectIntent 规则匹配，不调 LLM
func DetectIntent(text string) Intent {
	text = strings.TrimSpace(text)
	if text == "" {
		return IntentConversation
	}
	lower := strings.ToLower(text)

	// 帮助类
	helpKeywords := []string{"help", "帮助", "怎么用", "命令列表", ":help", ":?"}
	for _, kw := range helpKeywords {
		if strings.Contains(lower, kw) {
			return IntentHelp
		}
	}

	// 查询类
	queryKeywords := []string{
		"看看", "进度", "状态", "怎么样", "如何",
		"list", "status", "ls", "查看", "查询", "显示",
		"哪些", "有没有", "跑完了吗", "完成了",
	}
	for _, kw := range queryKeywords {
		if strings.Contains(lower, kw) {
			return IntentQuery
		}
	}

	// 指令类
	directiveKeywords := []string{
		"告诉", "让", "做", "执行", "send", "run",
		"开始", "启动", "停止", "重启", "继续",
		"给", "通知", "提醒",
	}
	for _, kw := range directiveKeywords {
		if strings.Contains(lower, kw) {
			return IntentDirective
		}
	}

	// 默认：对话类
	return IntentConversation
}

// IntentString 返回意图的字符串表示
func IntentString(i Intent) string {
	switch i {
	case IntentQuery:
		return "query"
	case IntentDirective:
		return "directive"
	case IntentHelp:
		return "help"
	default:
		return "conversation"
	}
}
