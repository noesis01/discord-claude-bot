package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Run 은 대화 하나에 대한 `claude -p` 실행 하나. !stop 으로 프로세스 그룹째 멈춘다.
type Run struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	done    bool
	stopped bool
}

func (r *Run) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	if r.cmd != nil && r.cmd.Process != nil && !r.done {
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
	}
}

func (r *Run) Stopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// Result 는 stream-json 의 마지막 "result" 이벤트.
type Result struct {
	Subtype        string `json:"subtype"`
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	NumTurns       *int   `json:"num_turns"`
	SessionID      string `json:"session_id"`
	APIErrorStatus any    `json:"api_error_status"`
	Errors         any    `json:"errors"`
	ToolCount      int    `json:"-"`
}

type contentBlock struct {
	Type  string         `json:"type"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
	Text  string         `json:"text"`
}

type streamEvent struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	SessionID       string  `json:"session_id"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         struct {
		Content []contentBlock `json:"content"`
	} `json:"message"`
}

// Hooks: 실행 중 알림 (Discord 와 분리 - 테스트에서 가짜로 바꿔 끼운다)
type Hooks struct {
	OnSession  func(id string)                     // init·result 이벤트의 세션 ID
	OnProgress func(steps []string, toolCount int) // 상태 메시지 갱신 (statusEditInterval 마다)
	Logf       func(format string, args ...any)
}

func claudeArgs(c *Config, persona, sessionID string) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json", "--verbose",
		"--model", c.Model,
		"--effort", c.Effort,
		"--permission-mode", c.PermissionMode,
		"--permission-prompts", "none",
		"--append-system-prompt", persona,
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	return args
}

// runClaude 는 claude 를 한 번 실행하고 result 이벤트를 돌려준다.
// nil, nil = 결과 없이 끝남 또는 첫 턴 전에 실패(예: 없어진 세션 resume) → 호출한 쪽이 새 세션으로 한 번 더.
func runClaude(c *Config, persona string, run *Run, sessionID, cwd, prompt string, h Hooks) (*Result, error) {
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		cwd = c.DefaultCwd
	}
	cmd := exec.Command(c.ClaudeBin, claudeArgs(c, persona, sessionID)...)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // 새 세션 = 새 프로세스 그룹 (!stop 이 자식까지 멈추게)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	run.mu.Lock()
	if run.stopped {
		run.mu.Unlock()
		return nil, nil
	}
	if err := cmd.Start(); err != nil {
		run.mu.Unlock()
		return nil, err
	}
	run.cmd, run.done = cmd, false
	run.mu.Unlock()

	errCh := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(stderr); errCh <- b }()

	var steps []string
	toolCount := 0
	var result *Result
	var lastEdit time.Time
	rd := bufio.NewReader(stdout)
	for {
		raw, rerr := rd.ReadBytes('\n')
		if len(raw) > 0 {
			var ev streamEvent
			if json.Unmarshal(raw, &ev) == nil {
				switch {
				case ev.Type == "system" && ev.Subtype == "init":
					h.OnSession(ev.SessionID)
				case ev.Type == "assistant" && (ev.ParentToolUseID == nil || *ev.ParentToolUseID == ""):
					for _, b := range ev.Message.Content {
						if b.Type == "tool_use" {
							toolCount++
							steps = append(steps, "🔧 "+describeTool(b.Name, b.Input))
						} else if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
							first := strings.TrimRight(splitLinesKeepEnds(strings.TrimSpace(b.Text))[0], lineEnds)
							s := runeCut(first, 150)
							if runeLen(first) > 150 {
								s += "…"
							}
							steps = append(steps, "💬 "+s)
						}
					}
				case ev.Type == "result":
					var r Result
					if json.Unmarshal(raw, &r) == nil {
						result = &r
						if r.SessionID != "" {
							h.OnSession(r.SessionID)
						}
					}
				}
				if h.OnProgress != nil && len(steps) > 0 && result == nil && time.Since(lastEdit) > statusEditInterval {
					lastEdit = time.Now()
					h.OnProgress(steps, toolCount)
				}
			}
		}
		if rerr != nil {
			break
		}
	}

	werr := cmd.Wait()
	run.mu.Lock()
	run.done = true
	run.mu.Unlock()
	stderrText := string(<-errCh)
	if result == nil && !run.Stopped() {
		tail := stderrText
		if r := []rune(tail); len(r) > 2000 {
			tail = string(r[len(r)-2000:])
		}
		h.Logf("claude exited %v without result: %s", exitCode(cmd, werr), tail)
	}
	if result != nil && result.Subtype == "error_during_execution" && result.NumTurns != nil && *result.NumTurns == 0 {
		h.Logf("claude failed before the first turn: %v", result.Errors)
		return nil, nil
	}
	if result != nil {
		result.ToolCount = toolCount
	}
	return result, nil
}

func exitCode(cmd *exec.Cmd, err error) any {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	return err
}
