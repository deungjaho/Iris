package wechat

// 消息分段：把长文本切成微信消息友好的段
// 微信单条消息约 2000 字，保守用 1500 字

import (
	"strings"
	"unicode/utf8"
)

const MaxSegment = 1500

// SplitMessage 把长文本切成多段，每段不超过 MaxSegment 字
// 优先按段落分，再按行分，最后按字符硬切
func SplitMessage(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if utf8.RuneCountInString(text) <= MaxSegment {
		return []string{text}
	}

	var segments []string
	// 先按双换行分段落
	paragraphs := strings.Split(text, "\n\n")
	var buf strings.Builder
	bufLen := 0

	for _, para := range paragraphs {
		paraLen := utf8.RuneCountInString(para)
		if paraLen > MaxSegment {
			// 段落本身太长，按行分
			if buf.Len() > 0 {
				segments = append(segments, buf.String())
				buf.Reset()
				bufLen = 0
			}
			segments = append(segments, splitByLine(para)...)
			continue
		}
		if bufLen+paraLen+2 > MaxSegment {
			segments = append(segments, buf.String())
			buf.Reset()
			bufLen = 0
		}
		if buf.Len() > 0 {
			buf.WriteString("\n\n")
			bufLen += 2
		}
		buf.WriteString(para)
		bufLen += paraLen
	}
	if buf.Len() > 0 {
		segments = append(segments, buf.String())
	}
	return segments
}

func splitByLine(text string) []string {
	var segments []string
	lines := strings.Split(text, "\n")
	var buf strings.Builder
	bufLen := 0
	for _, line := range lines {
		lineLen := utf8.RuneCountInString(line)
		if lineLen > MaxSegment {
			if buf.Len() > 0 {
				segments = append(segments, buf.String())
				buf.Reset()
				bufLen = 0
			}
			// 硬切
			segments = append(segments, splitHard(line)...)
			continue
		}
		if bufLen+lineLen+1 > MaxSegment {
			segments = append(segments, buf.String())
			buf.Reset()
			bufLen = 0
		}
		if buf.Len() > 0 {
			buf.WriteString("\n")
			bufLen += 1
		}
		buf.WriteString(line)
		bufLen += lineLen
	}
	if buf.Len() > 0 {
		segments = append(segments, buf.String())
	}
	return segments
}

func splitHard(text string) []string {
	var segments []string
	runes := []rune(text)
	for i := 0; i < len(runes); i += MaxSegment {
		end := i + MaxSegment
		if end > len(runes) {
			end = len(runes)
		}
		segments = append(segments, string(runes[i:end]))
	}
	return segments
}
