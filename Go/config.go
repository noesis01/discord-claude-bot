package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Python 버전(bot.py)과 같은 환경 변수를 읽는다 (bots/<봇>/bot.env).
type Config struct {
	Root           string // data/, bots/ 의 기준 폴더 (BOT_ROOT, 기본: 현재 작업 디렉터리)
	BotName        string
	Token          string
	AllowedUsers   map[string]bool
	ListenChannels map[string]bool
	Model          string
	Effort         string
	PermissionMode string
	DefaultCwd     string
	ClaudeBin      string
	PersonaFile    string
	ShowStatus     bool
	DataDir        string
	StateFile      string
	AttachDir      string
}

const (
	msgLimit           = 1900 // Discord hard limit is 2000; leave room for fence fixes
	attachThreshold    = 12000
	uploadMaxBytes     = 10 * 1024 * 1024
	statusEditInterval = 3 * time.Second
)

var uploadRE = regexp.MustCompile(`\[\[upload:\s*(.+?)\s*\]\]`)

const discordGuide = `
# Discord 브리지 안내
너는 Discord 봇을 통해 사용자와 대화하고 있다. 사용자는 터미널이 아니라 Discord 메시지로 네 답변을 본다.
- 최종 답변은 Discord 마크다운으로 작성한다. 표(table)는 Discord에서 렌더링되지 않으므로 목록이나 코드블록을 쓴다.
- 중간 과정은 봇이 자동으로 요약해 보여주므로, 최종 답변에는 결과와 핵심만 담는다.
- 사용자는 실행 도중 권한 프롬프트에 답할 수 없다. 확인이 꼭 필요한 위험한 작업(되돌리기 어려운 삭제, force push, 외부로 공개되는 작업 등)은 실행하지 말고 답변에서 먼저 확인을 요청한다.
- 파일을 사용자에게 첨부로 보내려면 답변에 ` + "`[[upload: /절대/경로]]`" + ` 를 한 줄로 적는다 (파일당 10MB 이하).
- 사용자가 첨부한 파일은 메시지 끝에 로컬 경로로 전달된다.
`

// getenv 는 Python os.environ.get(key, def) 처럼 "설정 안 됨"일 때만 기본값을 쓴다 (빈 값은 빈 값).
func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func idSet(v string) map[string]bool {
	set := map[string]bool{}
	for _, x := range strings.Split(strings.ReplaceAll(v, " ", ""), ",") {
		if x != "" {
			set[x] = true
		}
	}
	return set
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

func loadConfig() (*Config, error) {
	token, ok := os.LookupEnv("DISCORD_TOKEN")
	if !ok {
		return nil, fmt.Errorf("DISCORD_TOKEN 이 설정되지 않았습니다")
	}
	root := getenv("BOT_ROOT", "")
	if root == "" {
		root, _ = os.Getwd()
	}
	root, _ = filepath.Abs(root)
	c := &Config{
		Root:           root,
		BotName:        getenv("BOT_NAME", "claude"),
		Token:          token,
		AllowedUsers:   idSet(getenv("ALLOWED_USER_IDS", "")),
		ListenChannels: idSet(getenv("LISTEN_CHANNEL_IDS", "")),
		Model:          getenv("CLAUDE_MODEL", "claude-opus-5-5"),
		Effort:         getenv("CLAUDE_EFFORT", "high"),
		PermissionMode: getenv("CLAUDE_PERMISSION_MODE", "auto"),
		DefaultCwd:     expandUser(getenv("WORKDIR", "~")),
		ClaudeBin:      getenv("CLAUDE_BIN", "claude"),
		PersonaFile:    getenv("PERSONA_FILE", ""),
	}
	switch strings.ToLower(getenv("SHOW_STATUS", "true")) {
	case "0", "false", "no":
		c.ShowStatus = false
	default:
		c.ShowStatus = true
	}
	c.DataDir = filepath.Join(root, "data", c.BotName)
	c.StateFile = filepath.Join(c.DataDir, "state.json")
	c.AttachDir = filepath.Join(c.DataDir, "attachments")
	return c, nil
}

// loadPersona: 봇별 역할 프롬프트 + Discord 안내 (--append-system-prompt 로 전달)
func loadPersona(c *Config) (string, error) {
	text := discordGuide
	if c.PersonaFile != "" {
		p := c.PersonaFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.Root, p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		text = string(b) + "\n" + text
	}
	return text, nil
}
