package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 길이·자르기는 모두 문자(rune) 단위 - Python 의 len()·슬라이스와 같게.

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func runeCut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// lineEnds: Python splitlines 가 줄 끝으로 보는 문자들
const lineEnds = "\r\n\v\f\x1c\x1d\x1e\u0085  "

// splitLinesKeepEnds 는 Python str.splitlines(keepends=True) 와 같은 경계로 나눈다.
func splitLinesKeepEnds(s string) []string {
	var out []string
	r := []rune(s)
	start := 0
	for i := 0; i < len(r); i++ {
		switch r[i] {
		case '\r':
			if i+1 < len(r) && r[i+1] == '\n' {
				i++
			}
			out = append(out, string(r[start:i+1]))
			start = i + 1
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, string(r[start:i+1]))
			start = i + 1
		}
	}
	if start < len(r) {
		out = append(out, string(r[start:]))
	}
	return out
}

// splitMessage: 줄 경계로 나누고, 조각 사이에 걸친 ``` 코드블록은 닫았다가 다음 조각에서 다시 연다.
func splitMessage(text string, limit int) []string {
	width := limit - 40 // room for a reopened fence header and closing fence
	var lines []string
	for _, line := range splitLinesKeepEnds(text) {
		r := []rune(line)
		for len(r) > width {
			lines = append(lines, string(r[:width])+"\n")
			r = r[width:]
		}
		lines = append(lines, string(r))
	}

	var chunks []string
	cur, curLen := "", 0
	var fence *string
	for _, line := range lines {
		ll := runeLen(line)
		if strings.TrimSpace(cur) != "" && curLen+ll+5 > limit {
			if fence != nil {
				if strings.HasSuffix(cur, "\n") {
					cur += "```"
				} else {
					cur += "\n```"
				}
			}
			chunks = append(chunks, cur)
			cur = ""
			if fence != nil {
				cur = "```" + *fence + "\n"
			}
			curLen = runeLen(cur)
		}
		cur += line
		curLen += ll
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "```") {
			if fence == nil {
				f := strings.TrimSpace(stripped[3:])
				fence = &f
			} else {
				fence = nil
			}
		}
	}
	if strings.TrimSpace(cur) != "" {
		chunks = append(chunks, cur)
	}
	if len(chunks) == 0 {
		return []string{"(빈 응답)"}
	}
	return chunks
}

func short(v any, n int) string {
	s := strings.Join(strings.Fields(fmt.Sprint(v)), " ")
	if runeLen(s) <= n {
		return s
	}
	return runeCut(s, n-1) + "…"
}

// str 은 Python 의 inp.get(k, "") 처럼 없으면 빈 문자열.
func str(inp map[string]any, k string) any {
	if v, ok := inp[k]; ok && v != nil {
		return v
	}
	return ""
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	}
	return true
}

// describeTool: 진행 상태 메시지에 보여 줄 도구 호출 한 줄 요약
func describeTool(name string, inp map[string]any) string {
	switch name {
	case "Bash":
		return "`$ " + short(str(inp, "command"), 80) + "`"
	case "Read", "Edit", "Write", "NotebookEdit":
		p := str(inp, "file_path")
		if !truthy(p) {
			p = str(inp, "notebook_path")
		}
		return name + " `" + short(p, 80) + "`"
	case "Grep", "Glob":
		return name + " `" + short(str(inp, "pattern"), 80) + "`"
	case "WebFetch":
		return "WebFetch " + short(str(inp, "url"), 80)
	case "WebSearch":
		return "WebSearch `" + short(str(inp, "query"), 80) + "`"
	case "Task", "Agent":
		return "서브에이전트: " + short(str(inp, "description"), 80)
	}
	return name
}

func fmtElapsed(sec int) string { return fmt.Sprintf("%dm%02ds", sec/60, sec%60) }
