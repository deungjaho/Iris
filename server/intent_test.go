package main

import "testing"

func TestDetectIntent(t *testing.T) {
	tests := []struct {
		text   string
		intent Intent
	}{
		// 帮助类
		{"help", IntentHelp},
		{"帮助", IntentHelp},
		{"怎么用", IntentHelp},
		{":help", IntentHelp},

		// 查询类
		{"看看进度", IntentQuery},
		{"状态怎么样", IntentQuery},
		{"list runs", IntentQuery},
		{"查看 agent", IntentQuery},
		{"跑完了吗", IntentQuery},

		// 指令类
		{"告诉 agent 做某事", IntentDirective},
		{"让它重新跑", IntentDirective},
		{"执行测试", IntentDirective},
		{"停止 run", IntentDirective},

		// 对话类（默认 fallback）
		{"那个测试怎么样了", IntentQuery},    // "怎么样" 匹配
		{"让它重新跑一遍", IntentDirective}, // "让" 匹配
		{"你好", IntentConversation},
		{"今天天气怎么样", IntentQuery}, // "怎么样" 匹配
		{"翻译一下这段话", IntentConversation},
		{"总结一下", IntentConversation},
		{"", IntentConversation},
	}

	for _, tt := range tests {
		got := DetectIntent(tt.text)
		if got != tt.intent {
			t.Errorf("DetectIntent(%q) = %s, want %s", tt.text, IntentString(got), IntentString(tt.intent))
		}
	}
}

func TestIntentString(t *testing.T) {
	tests := []struct {
		intent Intent
		want   string
	}{
		{IntentConversation, "conversation"},
		{IntentQuery, "query"},
		{IntentDirective, "directive"},
		{IntentHelp, "help"},
	}
	for _, tt := range tests {
		if got := IntentString(tt.intent); got != tt.want {
			t.Errorf("IntentString(%d) = %s, want %s", tt.intent, got, tt.want)
		}
	}
}
