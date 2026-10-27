# Claude Discord 봇 (coder / docs)

Discord 메시지를 이 PC의 Claude Code CLI(`claude -p`)로 전달하는 브리지입니다.
두 봇 모두 `claude-opus-5-5` + effort `high`로 실행되고, 이 PC의 Claude 로그인과 환경을 그대로 씁니다.

| 봇 | 용도 | 기본 작업 디렉터리 |
|---|---|---|
| `coder` | 코딩, git, 범용 작업 | `/home/chlee` |
| `docs` | 문서화, 분석, 범용 작업 | `/apps/discordbot/workspace/docs` |
| `chat` | 잡담, 잡일 (전용 채널에서 멘션 없이 반응) | `/apps/discordbot/workspace/chat` |

## 구조
- `bot.py` — 봇 본체(두 봇이 같은 코드를 쓰고 설정만 다름)
- `bots/<봇>/bot.env` — 토큰, 허용 사용자, 모델 등 설정
- `bots/<봇>/persona.md` — 봇별 역할 프롬프트(`--append-system-prompt`로 추가됨)
- `data/<봇>/state.json` — 채널별 Claude 세션 ID와 작업 디렉터리
- `~/.config/systemd/user/discordbot@.service` — 서비스 유닛 (`discordbot@coder`, `discordbot@docs`)

## 사용법
- DM, 봇 멘션(봇 역할 멘션 포함), 봇 메시지에 대한 답장, 또는 `LISTEN_CHANNEL_IDS`에 지정한 채널(과 그 아래 스레드)에서 반응합니다.
- 두 봇을 한 채널에 함께 두려면 `LISTEN_CHANNEL_IDS`를 비워두세요. 멘션한 봇만 대답합니다.
- 채널/스레드/DM마다 별도 세션이며 대화 맥락이 이어집니다. 작업별로 스레드를 만들어 쓰면 편합니다.
- 명령어: `!help`, `!new`(새 세션), `!stop`(중단), `!status`, `!cwd <경로>`(작업 디렉터리 변경)
- 파일을 첨부하면 `data/<봇>/attachments/`에 저장되어 Claude에 경로가 전달됩니다.
- Claude가 답변에 `[[upload: /경로]]`를 쓰면 해당 파일이 Discord로 첨부됩니다.
- 진행 중에는 상태 메시지에 실행한 도구가 실시간으로 표시됩니다.

## 권한
- `ALLOWED_USER_IDS`에 있는 사용자만 사용할 수 있습니다. 봇은 이 PC 계정(chlee) 권한으로 셸을 실행하므로 반드시 본인 ID만 넣으세요.
- `CLAUDE_PERMISSION_MODE=auto`(기본): 위험해 보이는 작업은 분류기가 자동 차단하고, 그 사실이 답변에 나타납니다.
  모든 작업을 허용하려면 `bypassPermissions`로 바꾸세요.

## 운영
```bash
systemctl --user enable --now discordbot@coder discordbot@docs   # 시작 + 부팅 시 자동 시작
systemctl --user restart discordbot@coder                        # 설정 변경 후 재시작
journalctl --user-unit discordbot@coder -f                       # 로그
```
linger가 켜져 있어(`loginctl show-user chlee | grep Linger`) 로그아웃해도 계속 실행됩니다.
두 봇은 이 PC에 로그인된 같은 Claude 계정의 사용량 한도를 공유합니다.

## 설치 (이 저장소에서 처음 쓸 때)
```bash
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
cp bots/coder/bot.env.example bots/coder/bot.env      # 토큰·사용자 ID 입력 (bot.env 는 .gitignore 대상)
cp discordbot@.service ~/.config/systemd/user/          # 경로가 /apps/discordbot 이 아니면 유닛 안의 경로 수정
systemctl --user daemon-reload && systemctl --user enable --now discordbot@coder
```
Claude Code CLI(`claude`)가 설치·로그인되어 있어야 합니다.
