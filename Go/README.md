# Claude Discord 봇 — Go 포팅

상위 폴더의 Python 봇(`bot.py`)과 **같은 동작**을 하는 Go 버전입니다. 단일 실행 파일이라 Python·venv 없이 배포할 수 있습니다.

- 같은 환경 변수(`bots/<봇>/bot.env`), 같은 페르소나 파일, 같은 `data/<봇>/state.json` 형식을 씁니다 → Python 버전과 바꿔 끼워 쓸 수 있습니다 (세션 그대로 이어짐).
- 같은 명령어(`!help` `!new` `!stop` `!status` `!cwd`), 같은 반응 조건(DM·멘션·봇 역할 멘션·답장·`LISTEN_CHANNEL_IDS`·스레드), 같은 상태 메시지·첨부(`[[upload: 경로]]`)·긴 답변 분할 규칙.
- Discord 라이브러리: [discordgo](https://github.com/bwmarrin/discordgo) v0.29

| 파일 | 내용 (bot.py 대응) |
|---|---|
| `config.go` | 환경 변수, 상수, Discord 안내 프롬프트, 페르소나 (`load_persona`) |
| `state.go` | 채널별 세션·작업 디렉터리 저장 (`State`) |
| `text.go` | 메시지 분할 (`split_message`), 도구 요약 (`describe_tool`) |
| `claude.go` | `claude -p` 실행·stream-json 처리·중단 (`Run`, `run_claude`) |
| `bridge.go` | Discord 이벤트·명령어·답장·첨부 (`Bridge`) |
| `main.go` | 시작·종료 |

## 빌드
```bash
cd Go
go build -o discord-claude-bot .       # Go 1.26+
```

## 실행
Python 버전과 같은 폴더 구조를 기준으로 합니다 (`bots/`, `data/` 가 있는 저장소 루트가 기준 폴더).
```bash
cp bots/coder/bot.env.example bots/coder/bot.env      # 토큰·사용자 ID 입력
set -a; . bots/coder/bot.env; set +a
./Go/discord-claude-bot                                # 현재 디렉터리 = 기준 폴더 (또는 BOT_ROOT=/경로)
```
서비스로 돌릴 때는 `discordbot-go@.service` 를 `~/.config/systemd/user/` 에 복사 (경로는 환경에 맞게 수정).
**같은 봇 토큰으로 Python 버전과 동시에 실행하지 마세요** (두 프로세스가 모두 답합니다).

Go 버전에만 있는 환경 변수: `BOT_ROOT` (기준 폴더, 기본: 현재 디렉터리). 나머지는 Python 버전과 같습니다 (`DISCORD_TOKEN`, `ALLOWED_USER_IDS`, `LISTEN_CHANNEL_IDS`, `BOT_NAME`, `PERSONA_FILE`, `WORKDIR`, `CLAUDE_MODEL`, `CLAUDE_EFFORT`, `CLAUDE_PERMISSION_MODE`, `CLAUDE_BIN`, `SHOW_STATUS`).

## 테스트
```bash
go test ./...                                 # 단위 테스트 (가짜 claude 로 실행·중단·세션 처리)
CLAUDE_E2E=1 go test -run Real -v ./...       # 실제 claude CLI 로 한 번 실행 + --resume (사용량 발생)
```
- `TestSplitMessageMatchesPython`, `TestDescribeToolMatchesPython`: Python 원본 결과(`testdata/golden.json`)와 글자 단위로 같은지 비교. 원본을 바꾸면 저장소 루트에서 `DISCORD_TOKEN=x python Go/testdata/gen_golden.py` 로 다시 생성.

## Python 버전과 다른 점
- 길이 계산·자르기는 Python 과 같게 문자(rune) 단위로 맞췄습니다.
- 동시성: Python 은 asyncio 단일 스레드, Go 는 채널마다 고루틴 + 채널별 잠금 (같은 채널 메시지는 순서대로, 다른 채널은 동시에).
- 로그 형식만 약간 다릅니다 (`<봇이름> 날짜 시각 LEVEL 메시지`).
