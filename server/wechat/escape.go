package wechat

// 微信 markdown 转义
// 微信会把下划线 _ 当作斜体标记吞噬，需要转义成全角 ＿（U+FF3F）
// 视觉几乎不可区分，但不会被 markdown 解析器消费
//
// 参考：https://github.com/corespeed-io/wechatbot/issues/79

import "strings"

// EscapeMarkdown 转义微信 markdown 渲染会吞噬的字符
// 目前只转义下划线 _（最常见的问题）
// 不转义 * ` | # 等，因为它们在表格、代码块、粗体中是有意义的
func EscapeMarkdown(s string) string {
	// 下划线 → 全角下划线
	s = strings.ReplaceAll(s, "_", "＿")
	return s
}

// EscapeAndSend 发送消息前自动转义
// 注意：只对"系统消息"转义，不对 claude 的回复转义
// （claude 的回复可能包含代码块，代码块里的下划线不应该转义）
