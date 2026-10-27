package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 가짜 claude: 받은 인자·표준 입력을 기록하고, 정해진 stream-json 을 출력한다.
func fakeClaude(t *testing.T, script string) *Config {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + dir + "/args\ncat > " + dir + "/stdin\npwd > " + dir + "/cwd\n" + script
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Config{ClaudeBin: bin, Model: "m", Effort: "high", PermissionMode: "auto", DefaultCwd: dir}
}

func hooks(sessions *[]string) Hooks {
	return Hooks{
		OnSession: func(id string) { *sessions = append(*sessions, id) },
		Logf:      func(string, ...any) {},
	}
}

func TestRunClaudeStream(t *testing.T) {
	c := fakeClaude(t, `cat <<'EOF'
not json
{"type":"system","subtype":"init","session_id":"s-1"}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}},{"type":"text","text":"  첫 줄\n둘째 줄"}]}}
{"type":"assistant","parent_tool_use_id":"sub","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/x"}}]}}
{"type":"result","subtype":"success","is_error":false,"result":"끝 [[upload: /tmp/a]]","num_turns":3,"session_id":"s-2"}
EOF`)
	var sessions []string
	h := hooks(&sessions)
	var lastSteps []string
	h.OnProgress = func(steps []string, n int) { lastSteps = append([]string(nil), steps...) }
	r, err := runClaude(c, "PERSONA", &Run{}, "old", c.DefaultCwd, "프롬프트", h)
	if err != nil || r == nil {
		t.Fatalf("result %v err %v", r, err)
	}
	if r.Result != "끝 [[upload: /tmp/a]]" || *r.NumTurns != 3 || r.ToolCount != 1 {
		t.Errorf("unexpected result %+v", r)
	}
	if strings.Join(sessions, ",") != "s-1,s-2" {
		t.Errorf("sessions %v", sessions)
	}
	if strings.Join(lastSteps, "|") != "🔧 `$ ls`|💬 첫 줄" {
		t.Errorf("progress steps %q", lastSteps) // 이벤트 하나를 다 처리한 뒤 첫 갱신 (이후 3초 간격)
	}
	args, _ := os.ReadFile(filepath.Join(c.DefaultCwd, "args"))
	for _, want := range []string{"-p", "stream-json", "--resume\nold", "--append-system-prompt\nPERSONA", "--permission-prompts\nnone"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	if in, _ := os.ReadFile(filepath.Join(c.DefaultCwd, "stdin")); string(in) != "프롬프트" {
		t.Errorf("stdin %q", in)
	}
}

func TestRunClaudeResumeFailure(t *testing.T) {
	c := fakeClaude(t, `echo '{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"errors":["No conversation found"]}'`)
	var sessions []string
	r, err := runClaude(c, "", &Run{}, "gone", "/nonexistent-dir", "", hooks(&sessions))
	if err != nil || r != nil {
		t.Fatalf("want nil result (caller retries fresh), got %+v %v", r, err)
	}
	if cwd, _ := os.ReadFile(filepath.Join(c.DefaultCwd, "cwd")); strings.TrimSpace(string(cwd)) != c.DefaultCwd {
		t.Errorf("missing cwd should fall back to default, ran in %q", cwd)
	}
}

func TestRunClaudeNoResult(t *testing.T) {
	c := fakeClaude(t, `echo boom >&2; exit 3`)
	var logged string
	h := Hooks{OnSession: func(string) {}, Logf: func(f string, a ...any) { logged = fmt.Sprintf(f, a...) }}
	r, err := runClaude(c, "", &Run{}, "", c.DefaultCwd, "", h)
	if err != nil || r != nil {
		t.Fatalf("got %+v %v", r, err)
	}
	if !strings.Contains(logged, "exited 3") || !strings.Contains(logged, "boom") {
		t.Errorf("log %q", logged)
	}
}

func TestRunStopKillsProcessGroup(t *testing.T) {
	// 자식 프로세스(sleep)까지 같이 멈춰야 한다 - claude 가 띄운 셸 명령 포함
	c := fakeClaude(t, `echo '{"type":"system","subtype":"init","session_id":"s"}'; sleep 30 & sleep 30; wait`)
	run := &Run{}
	var sessions []string
	go func() { time.Sleep(500 * time.Millisecond); run.Stop() }()
	start := time.Now()
	r, err := runClaude(c, "", run, "", c.DefaultCwd, "", hooks(&sessions))
	if err != nil || r != nil || !run.Stopped() {
		t.Fatalf("got %+v %v stopped=%v", r, err, run.Stopped())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("stop took %v", d)
	}
}

func TestStateCompatibleWithPython(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	// Python 버전이 쓴 형식 그대로 읽고 쓴다
	os.WriteFile(p, []byte(`{"123": {"session_id": null, "cwd": "/home"}, "456": {"session_id": "abc", "cwd": "/tmp"}}`), 0o644)
	st, err := loadState(p, "/def")
	if err != nil {
		t.Fatal(err)
	}
	if st.Get("456").Session() != "abc" || st.Get("123").Session() != "" || st.Get("789").Cwd != "/def" {
		t.Fatal("unexpected state")
	}
	st.Get("123").SetSession("new")
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, want := range []string{`"session_id": "new"`, `"session_id": null`, `"cwd": "/def"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("saved state missing %s:\n%s", want, b)
		}
	}
}

// 실제 claude CLI 로 한 번 실행 (사용량이 들므로 CLAUDE_E2E=1 일 때만): CLAUDE_E2E=1 go test -run Real -v
func TestRunClaudeReal(t *testing.T) {
	if os.Getenv("CLAUDE_E2E") != "1" {
		t.Skip("CLAUDE_E2E=1 일 때만")
	}
	c := &Config{ClaudeBin: "claude", Model: getenv("E2E_MODEL", "claude-haiku-5-5"), Effort: "low", PermissionMode: "auto", DefaultCwd: t.TempDir()}
	var sessions []string
	r, err := runClaude(c, discordGuide, &Run{}, "", c.DefaultCwd, "Reply with exactly: pong", hooks(&sessions))
	if err != nil || r == nil {
		t.Fatalf("result %+v err %v", r, err)
	}
	t.Logf("result=%q is_error=%v turns=%s session=%v", r.Result, r.IsError, turns(r), sessions)
	if r.IsError || !strings.Contains(strings.ToLower(r.Result), "pong") || len(sessions) == 0 {
		t.Fatal("unexpected")
	}
	// 같은 세션 이어서 (--resume)
	r2, err := runClaude(c, discordGuide, &Run{}, sessions[len(sessions)-1], c.DefaultCwd, "What word did you just reply with? One word.", hooks(&sessions))
	if err != nil || r2 == nil || !strings.Contains(strings.ToLower(r2.Result), "pong") {
		t.Fatalf("resume: %+v %v", r2, err)
	}
	t.Logf("resume result=%q", r2.Result)
}
