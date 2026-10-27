// Discord <-> Claude Code bridge (Go 포팅, 원본: ../bot.py).
//
// 봇 하나당 프로세스 하나. Discord 채널·스레드·DM 마다 Claude Code 세션 하나를 두고,
// 메시지마다 그 세션을 이어서(--resume) 대화 맥락을 유지한다. 설정은 환경 변수 (bots/*/bot.env).
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	lg := log.New(os.Stderr, cfg.BotName+" ", log.LstdFlags)
	for _, d := range []string{cfg.DataDir, cfg.AttachDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			lg.Fatal(err)
		}
	}
	st, err := loadState(cfg.StateFile, cfg.DefaultCwd)
	if err != nil {
		lg.Fatalf("state: %v", err)
	}
	persona, err := loadPersona(cfg)
	if err != nil {
		lg.Fatalf("persona: %v", err)
	}

	s, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		lg.Fatal(err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent
	b := newBridge(cfg, s, st, persona, lg)
	s.AddHandler(b.onReady)
	s.AddHandler(b.onMessage)
	if err := s.Open(); err != nil {
		lg.Fatalf("discord: %v", err)
	}
	defer s.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	lg.Printf("INFO shutting down")
}
