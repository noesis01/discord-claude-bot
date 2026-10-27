package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Bridge: Discord 메시지 ↔ claude 실행. 채널(스레드·DM)마다 별도 세션, 한 번에 하나씩 처리.
type Bridge struct {
	cfg     *Config
	s       *discordgo.Session
	state   *State
	persona string
	log     *log.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	runs  map[string]*Run
}

func newBridge(cfg *Config, s *discordgo.Session, st *State, persona string, lg *log.Logger) *Bridge {
	return &Bridge{cfg: cfg, s: s, state: st, persona: persona, log: lg,
		locks: map[string]*sync.Mutex{}, runs: map[string]*Run{}}
}

func (b *Bridge) botID() string { return b.s.State.User.ID }

func (b *Bridge) onReady(s *discordgo.Session, r *discordgo.Ready) {
	b.log.Printf("INFO logged in as %s (%s)", r.User.String(), r.User.ID)
	if len(b.cfg.AllowedUsers) == 0 {
		b.log.Printf("WARNING ALLOWED_USER_IDS is empty: nobody can use this bot")
	}
}

// ---- routing ---------------------------------------------------------------

func (b *Bridge) channel(id string) *discordgo.Channel {
	if ch, err := b.s.State.Channel(id); err == nil {
		return ch
	}
	ch, err := b.s.Channel(id)
	if err != nil {
		return nil
	}
	return ch
}

// botManagedRoles: 이 봇에 붙은 관리형(봇 전용) 역할. @자동완성이 봇 대신 이 역할을 고를 수 있다.
func (b *Bridge) botManagedRoles(guildID string) []string {
	if guildID == "" {
		return nil
	}
	me, err := b.s.State.Member(guildID, b.botID())
	if err != nil {
		if me, err = b.s.GuildMember(guildID, b.botID()); err != nil {
			return nil
		}
		_ = b.s.State.MemberAdd(me)
	}
	var out []string
	for _, rid := range me.Roles {
		r, err := b.s.State.Role(guildID, rid)
		if err == nil && r.Managed {
			out = append(out, rid)
		}
	}
	return out
}

func (b *Bridge) isTriggered(m *discordgo.MessageCreate) bool {
	if m.GuildID == "" {
		return true // DM
	}
	for _, u := range m.Mentions {
		if u.ID == b.botID() {
			return true
		}
	}
	if len(m.MentionRoles) > 0 {
		mine := b.botManagedRoles(m.GuildID)
		for _, rid := range m.MentionRoles {
			for _, x := range mine {
				if rid == x {
					return true
				}
			}
		}
	}
	if ref := m.ReferencedMessage; ref != nil && ref.Author != nil && ref.Author.ID == b.botID() {
		return true
	}
	if b.cfg.ListenChannels[m.ChannelID] {
		return true
	}
	if ch := b.channel(m.ChannelID); ch != nil && ch.IsThread() &&
		(b.cfg.ListenChannels[ch.ParentID] || ch.OwnerID == b.botID()) {
		return true
	}
	return false
}

func (b *Bridge) stripMention(m *discordgo.MessageCreate) string {
	text := regexp.MustCompile(`<@!?`+regexp.QuoteMeta(b.botID())+`>`).ReplaceAllString(m.Content, "")
	for _, rid := range b.botManagedRoles(m.GuildID) {
		text = strings.ReplaceAll(text, "<@&"+rid+">", "")
	}
	return strings.TrimSpace(text)
}

func (b *Bridge) where(m *discordgo.Message) string {
	if m.GuildID == "" {
		return "DM"
	}
	if ch := b.channel(m.ChannelID); ch != nil && ch.Name != "" {
		return "#" + ch.Name
	}
	return "#" + m.ChannelID
}

func displayName(m *discordgo.Message) string {
	if m.Member != nil && m.Member.Nick != "" {
		return m.Member.Nick
	}
	if m.Author != nil {
		return m.Author.DisplayName()
	}
	return "?"
}

func (b *Bridge) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || !b.isTriggered(m) {
		return
	}
	if !b.cfg.AllowedUsers[m.Author.ID] {
		b.log.Printf("INFO ignored message from unauthorized user %s (%s)", m.Author.String(), m.Author.ID)
		return
	}

	text := b.stripMention(m)
	files := ""
	if len(m.Attachments) > 0 {
		files = fmt.Sprintf(" (+%d files)", len(m.Attachments))
	}
	b.log.Printf("INFO message from %s in %s: %s%s", m.Author.String(), b.where(m.Message),
		strings.ReplaceAll(runeCut(text, 100), "\n", " "), files)
	key := m.ChannelID
	conv := b.state.Get(key)

	if strings.HasPrefix(text, "!") && b.handleCommand(m, key, conv, text) {
		return
	}
	if text == "" && len(m.Attachments) == 0 {
		return
	}

	b.mu.Lock()
	lock, ok := b.locks[key]
	if !ok {
		lock = &sync.Mutex{}
		b.locks[key] = lock
	}
	b.mu.Unlock()
	if !lock.TryLock() {
		_ = s.MessageReactionAdd(m.ChannelID, m.ID, "⏳")
		lock.Lock()
	}
	defer lock.Unlock()
	_ = s.MessageReactionRemove(m.ChannelID, m.ID, "⏳", "@me")
	b.process(m, key, conv, text)
}

func (b *Bridge) handleCommand(m *discordgo.MessageCreate, key string, conv *Conv, text string) bool {
	cmd, arg, _ := strings.Cut(text, " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "!help":
		b.reply(m.Message, "**명령어**\n"+
			"`!new` 새 대화 시작 (세션 초기화)\n"+
			"`!stop` 진행 중인 작업 중단\n"+
			"`!status` 현재 세션/작업 디렉터리/모델 정보\n"+
			"`!cwd <경로>` 작업 디렉터리 변경 (새 세션 시작)\n"+
			"그 외 메시지는 모두 Claude에게 전달됩니다. 채널(스레드)마다 별도 세션입니다.", true, nil)
	case "!new":
		b.state.Do(func() { conv.SetSession("") })
		b.saveState()
		b.reply(m.Message, "🆕 새 대화를 시작합니다.", true, nil)
	case "!stop":
		b.mu.Lock()
		run := b.runs[key]
		b.mu.Unlock()
		if run != nil {
			b.log.Printf("INFO stop requested")
			run.Stop()
			b.reply(m.Message, "🛑 중단 요청을 보냈습니다.", true, nil)
		} else {
			b.reply(m.Message, "진행 중인 작업이 없습니다.", true, nil)
		}
	case "!status":
		b.mu.Lock()
		_, running := b.runs[key]
		b.mu.Unlock()
		busy := "대기"
		if running {
			busy = "실행 중"
		}
		var cwd, sid string
		b.state.Do(func() { cwd, sid = conv.Cwd, conv.Session() })
		if sid == "" {
			sid = "(새 세션)"
		}
		b.reply(m.Message, fmt.Sprintf("봇: `%s` · 모델: `%s` · effort: `%s` · 권한: `%s`\n작업 디렉터리: `%s`\n세션: `%s` · 상태: %s",
			b.cfg.BotName, b.cfg.Model, b.cfg.Effort, b.cfg.PermissionMode, cwd, sid, busy), true, nil)
	case "!cwd":
		var cur string
		b.state.Do(func() { cur = conv.Cwd })
		if arg == "" {
			b.reply(m.Message, fmt.Sprintf("현재 작업 디렉터리: `%s`", cur), true, nil)
			return true
		}
		p := arg
		if !filepath.IsAbs(expandUser(arg)) {
			p = filepath.Join(cur, arg)
		}
		p, _ = filepath.Abs(expandUser(p))
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			b.reply(m.Message, fmt.Sprintf("디렉터리가 없습니다: `%s`", p), true, nil)
			return true
		}
		b.state.Do(func() { conv.Cwd = p; conv.SetSession("") })
		b.saveState()
		b.reply(m.Message, fmt.Sprintf("📁 작업 디렉터리를 `%s` 로 바꾸고 새 대화를 시작합니다.", p), true, nil)
	default:
		return false // not a bot command: pass through to Claude
	}
	return true
}

// ---- running claude ----------------------------------------------------------

func (b *Bridge) saveState() {
	if err := b.state.Save(); err != nil {
		b.log.Printf("ERROR state save: %v", err)
	}
}

func (b *Bridge) saveAttachments(m *discordgo.MessageCreate) ([]string, error) {
	var paths []string
	if len(m.Attachments) == 0 {
		return paths, nil
	}
	d := filepath.Join(b.cfg.AttachDir, m.ID)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, err
	}
	for _, a := range m.Attachments {
		p := filepath.Join(d, filepath.Base(a.Filename))
		if err := download(a.URL, p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

func download(url, path string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("attachment download: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (b *Bridge) buildPrompt(m *discordgo.MessageCreate, text string, files []string) string {
	parts := []string{fmt.Sprintf("[Discord %s · %s]", b.where(m.Message), displayName(m.Message)), text}
	if ref := m.ReferencedMessage; ref != nil {
		quoted := fmt.Sprintf("(답장 대상 메시지 - %s):\n> ", displayName(ref)) +
			strings.ReplaceAll(runeCut(ref.Content, 1500), "\n", "\n> ")
		parts = append(parts[:1], append([]string{quoted}, parts[1:]...)...)
	}
	if len(files) > 0 {
		lines := make([]string, len(files))
		for i, p := range files {
			lines[i] = "- " + p
		}
		parts = append(parts, "첨부 파일:\n"+strings.Join(lines, "\n"))
	}
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n\n")
}

// typing 은 done 이 닫힐 때까지 "입력 중…" 표시를 유지한다.
func (b *Bridge) typing(channelID string, done <-chan struct{}) {
	t := time.NewTicker(8 * time.Second)
	defer t.Stop()
	for {
		_ = b.s.ChannelTyping(channelID)
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}

func (b *Bridge) process(m *discordgo.MessageCreate, key string, conv *Conv, text string) {
	started := time.Now()
	run := &Run{}
	b.mu.Lock()
	b.runs[key] = run
	b.mu.Unlock()

	var status *discordgo.Message
	var result *Result
	func() {
		defer func() {
			if e := recover(); e != nil {
				b.log.Printf("ERROR run failed: %v", e)
				result = &Result{IsError: true, Result: fmt.Sprintf("봇 오류: %v", e)}
			}
			b.mu.Lock()
			delete(b.runs, key)
			b.mu.Unlock()
			b.saveState()
		}()
		files, err := b.saveAttachments(m)
		if err != nil {
			panic(err)
		}
		prompt := b.buildPrompt(m, text, files)
		if b.cfg.ShowStatus {
			status = b.reply(m.Message, "🤔 생각 중…", false, nil)
		}
		done := make(chan struct{})
		defer close(done)
		go b.typing(m.ChannelID, done)

		hooks := Hooks{
			OnSession: func(id string) {
				b.state.Do(func() { conv.SetSession(id) })
				b.saveState()
			},
			OnProgress: func(steps []string, toolCount int) {
				if status == nil {
					return
				}
				if len(steps) > 8 {
					steps = steps[len(steps)-8:]
				}
				el := int(time.Since(started).Seconds())
				content := fmt.Sprintf("⚙️ 작업 중 · %s · 도구 %d회\n%s", fmtElapsed(el), toolCount, strings.Join(steps, "\n"))
				_, _ = b.s.ChannelMessageEdit(status.ChannelID, status.ID, runeCut(content, 1990))
			},
			Logf: func(format string, args ...any) { b.log.Printf("ERROR "+format, args...) },
		}
		attempt := func() *Result {
			var sid, cwd string
			b.state.Do(func() { sid, cwd = conv.Session(), conv.Cwd })
			r, err := runClaude(b.cfg, b.persona, run, sid, cwd, prompt, hooks)
			if err != nil {
				panic(err)
			}
			return r
		}
		result = attempt()
		var sid string
		b.state.Do(func() { sid = conv.Session() })
		if result == nil && sid != "" && !run.Stopped() {
			// resume failed (session deleted or cwd moved): start fresh once
			b.state.Do(func() { conv.SetSession("") })
			result = attempt()
		}
	}()

	elapsed := int(time.Since(started).Seconds())
	if run.Stopped() {
		b.notify(m.Message, status, "🛑 중단됨 · "+fmtElapsed(elapsed))
		return
	}
	if result == nil {
		b.notify(m.Message, status, "❌ Claude 실행에 실패했습니다. 로그를 확인하세요.")
		return
	}
	var sid string
	b.state.Do(func() { sid = conv.Session() })
	outcome := "ok"
	if result.IsError {
		outcome = "error"
	}
	b.log.Printf("INFO finished in %ds: %s, %s turns, %d tools, session %s", elapsed, outcome, turns(result), result.ToolCount, sid)
	b.sendResult(m.Message, status, result, elapsed)
}

func turns(r *Result) string {
	if r.NumTurns == nil {
		return "?"
	}
	return fmt.Sprint(*r.NumTurns)
}

func (b *Bridge) notify(m *discordgo.Message, status *discordgo.Message, content string) {
	if status != nil {
		_, _ = b.s.ChannelMessageEdit(status.ChannelID, status.ID, content)
	} else {
		b.reply(m, content, false, nil)
	}
}

// reply: msg 에 대한 답장. mention=false 면 답장 대상에게 알림을 보내지 않는다 (discord.py mention_author=False).
func (b *Bridge) reply(m *discordgo.Message, content string, mention bool, files []*discordgo.File) *discordgo.Message {
	out, err := b.s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Content:   content,
		Files:     files,
		Reference: m.SoftReference(),
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers,
				discordgo.AllowedMentionTypeRoles, discordgo.AllowedMentionTypeEveryone},
			RepliedUser: mention,
		},
	})
	if err != nil {
		b.log.Printf("ERROR send: %v", err)
		return nil
	}
	return out
}

func (b *Bridge) sendResult(m *discordgo.Message, status *discordgo.Message, result *Result, elapsed int) {
	text := result.Result
	if result.IsError && text == "" {
		api := ""
		if truthy(result.APIErrorStatus) {
			api = fmt.Sprint(result.APIErrorStatus)
		}
		text = fmt.Sprintf("오류: %s %s", result.Subtype, api)
	}

	var uploads, missing []string
	for _, mm := range uploadRE.FindAllStringSubmatch(text, -1) {
		p := expandUser(mm[1])
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Size() <= uploadMaxBytes {
			uploads = append(uploads, p)
		} else {
			missing = append(missing, p)
		}
	}
	text = strings.TrimSpace(uploadRE.ReplaceAllString(text, ""))
	if len(missing) > 0 {
		q := make([]string, len(missing))
		for i, x := range missing {
			q[i] = "`" + x + "`"
		}
		text += "\n\n⚠️ 첨부 실패(없거나 10MB 초과): " + strings.Join(q, ", ")
	}

	icon := "✅"
	if result.IsError {
		icon = "⚠️"
	}
	if status != nil {
		_, _ = b.s.ChannelMessageEdit(status.ChannelID, status.ID,
			fmt.Sprintf("%s 완료 · %s · 도구 %d회 · %s턴", icon, fmtElapsed(elapsed), result.ToolCount, turns(result)))
	}

	var files []*discordgo.File
	if len(uploads) > 10 {
		uploads = uploads[:10]
	}
	for _, p := range uploads {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		defer f.Close()
		files = append(files, &discordgo.File{Name: filepath.Base(p), Reader: f})
	}
	if runeLen(text) > attachThreshold {
		full := filepath.Join(b.cfg.DataDir, "reply-"+m.ID+".md")
		if err := os.WriteFile(full, []byte(text), 0o644); err == nil {
			if f, err := os.Open(full); err == nil {
				defer f.Close()
				files = append(files, &discordgo.File{Name: "reply.md", Reader: f})
			}
		}
		head := runeCut(text, attachThreshold-200)
		if i := strings.LastIndex(head, "\n"); i >= 0 {
			head = head[:i]
		}
		text = head + "\n\n… (전체 내용은 첨부된 reply.md 참고)"
	}

	var chunks []string
	if text != "" {
		chunks = splitMessage(text, msgLimit)
	}
	for i, chunk := range chunks {
		var fs []*discordgo.File
		if i == len(chunks)-1 {
			fs = files
		}
		if i == 0 {
			b.reply(m, chunk, false, fs)
		} else if _, err := b.s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{Content: chunk, Files: fs}); err != nil {
			b.log.Printf("ERROR send: %v", err)
		}
	}
	if len(chunks) == 0 && len(files) > 0 {
		b.reply(m, "", false, files)
	}
}
