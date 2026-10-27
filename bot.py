"""Discord <-> Claude Code bridge.

One process per bot. Each Discord channel/thread/DM maps to its own Claude Code
session, which is resumed on every message so the conversation keeps context.
Configuration comes from environment variables (see bots/*/bot.env).
"""

import asyncio
import json
import logging
import os
import re
import signal
import time
from pathlib import Path

import discord

ROOT = Path(__file__).resolve().parent

BOT_NAME = os.environ.get("BOT_NAME", "claude")
TOKEN = os.environ["DISCORD_TOKEN"]
ALLOWED_USERS = {int(x) for x in os.environ.get("ALLOWED_USER_IDS", "").replace(" ", "").split(",") if x}
LISTEN_CHANNELS = {int(x) for x in os.environ.get("LISTEN_CHANNEL_IDS", "").replace(" ", "").split(",") if x}
MODEL = os.environ.get("CLAUDE_MODEL", "claude-opus-5-5")
EFFORT = os.environ.get("CLAUDE_EFFORT", "high")
PERMISSION_MODE = os.environ.get("CLAUDE_PERMISSION_MODE", "auto")
DEFAULT_CWD = os.path.expanduser(os.environ.get("WORKDIR", "~"))
CLAUDE_BIN = os.environ.get("CLAUDE_BIN", "claude")
PERSONA_FILE = os.environ.get("PERSONA_FILE", "")
# false: no progress/summary message, only the typing indicator and the reply itself
SHOW_STATUS = os.environ.get("SHOW_STATUS", "true").lower() not in ("0", "false", "no")

DATA_DIR = ROOT / "data" / BOT_NAME
STATE_FILE = DATA_DIR / "state.json"
ATTACH_DIR = DATA_DIR / "attachments"

MSG_LIMIT = 1900          # Discord hard limit is 2000; leave room for fence fixes
ATTACH_THRESHOLD = 12000  # longer replies are also uploaded as a .md file
UPLOAD_MAX_BYTES = 10 * 1024 * 1024
STATUS_EDIT_INTERVAL = 3.0
UPLOAD_RE = re.compile(r"\[\[upload:\s*(.+?)\s*\]\]")

log = logging.getLogger(BOT_NAME)

DISCORD_GUIDE = """
# Discord 브리지 안내
너는 Discord 봇을 통해 사용자와 대화하고 있다. 사용자는 터미널이 아니라 Discord 메시지로 네 답변을 본다.
- 최종 답변은 Discord 마크다운으로 작성한다. 표(table)는 Discord에서 렌더링되지 않으므로 목록이나 코드블록을 쓴다.
- 중간 과정은 봇이 자동으로 요약해 보여주므로, 최종 답변에는 결과와 핵심만 담는다.
- 사용자는 실행 도중 권한 프롬프트에 답할 수 없다. 확인이 꼭 필요한 위험한 작업(되돌리기 어려운 삭제, force push, 외부로 공개되는 작업 등)은 실행하지 말고 답변에서 먼저 확인을 요청한다.
- 파일을 사용자에게 첨부로 보내려면 답변에 `[[upload: /절대/경로]]` 를 한 줄로 적는다 (파일당 10MB 이하).
- 사용자가 첨부한 파일은 메시지 끝에 로컬 경로로 전달된다.
"""


def load_persona() -> str:
    text = DISCORD_GUIDE
    if PERSONA_FILE:
        p = Path(PERSONA_FILE)
        if not p.is_absolute():
            p = ROOT / p
        text = p.read_text(encoding="utf-8") + "\n" + text
    return text


class State:
    """Per-conversation session ids and working directories, persisted to disk."""

    def __init__(self, path: Path):
        self.path = path
        self.data: dict[str, dict] = {}
        if path.exists():
            self.data = json.loads(path.read_text())

    def get(self, key: int) -> dict:
        return self.data.setdefault(str(key), {"session_id": None, "cwd": DEFAULT_CWD})

    def save(self):
        tmp = self.path.with_suffix(".tmp")
        tmp.write_text(json.dumps(self.data, indent=2, ensure_ascii=False))
        tmp.replace(self.path)


def split_message(text: str, limit: int = MSG_LIMIT) -> list[str]:
    """Split on line boundaries, closing and reopening ``` fences across chunks."""
    width = limit - 40  # room for a reopened fence header and closing fence
    lines = []
    for line in text.splitlines(keepends=True):
        while len(line) > width:
            lines.append(line[:width] + "\n")
            line = line[width:]
        lines.append(line)

    chunks, cur, fence = [], "", None
    for line in lines:
        if cur.strip() and len(cur) + len(line) + 5 > limit:
            if fence is not None:
                cur += "```" if cur.endswith("\n") else "\n```"
            chunks.append(cur)
            cur = f"```{fence}\n" if fence is not None else ""
        cur += line
        stripped = line.strip()
        if stripped.startswith("```"):
            fence = stripped[3:].strip() if fence is None else None
    if cur.strip():
        chunks.append(cur)
    return chunks or ["(빈 응답)"]


def describe_tool(name: str, inp: dict) -> str:
    def short(s, n=80):
        s = " ".join(str(s).split())
        return s if len(s) <= n else s[: n - 1] + "…"

    if name == "Bash":
        return f"`$ {short(inp.get('command', ''))}`"
    if name in ("Read", "Edit", "Write", "NotebookEdit"):
        return f"{name} `{short(inp.get('file_path') or inp.get('notebook_path', ''))}`"
    if name in ("Grep", "Glob"):
        return f"{name} `{short(inp.get('pattern', ''))}`"
    if name in ("WebFetch",):
        return f"WebFetch {short(inp.get('url', ''))}"
    if name in ("WebSearch",):
        return f"WebSearch `{short(inp.get('query', ''))}`"
    if name in ("Task", "Agent"):
        return f"서브에이전트: {short(inp.get('description', ''))}"
    return name


class Run:
    """One `claude -p` invocation for a conversation."""

    def __init__(self):
        self.proc: asyncio.subprocess.Process | None = None
        self.stopped = False

    def stop(self):
        self.stopped = True
        if self.proc and self.proc.returncode is None:
            try:
                os.killpg(self.proc.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass


class Bridge(discord.Client):
    def __init__(self):
        intents = discord.Intents.default()
        intents.message_content = True
        super().__init__(intents=intents)
        DATA_DIR.mkdir(parents=True, exist_ok=True)
        ATTACH_DIR.mkdir(parents=True, exist_ok=True)
        self.state = State(STATE_FILE)
        self.persona = load_persona()
        self.locks: dict[int, asyncio.Lock] = {}
        self.runs: dict[int, Run] = {}

    async def on_ready(self):
        log.info("logged in as %s (%s)", self.user, self.user.id)
        if not ALLOWED_USERS:
            log.warning("ALLOWED_USER_IDS is empty: nobody can use this bot")

    # ---- routing ---------------------------------------------------------

    def is_triggered(self, msg: discord.Message) -> bool:
        if isinstance(msg.channel, discord.DMChannel):
            return True
        if self.user in msg.mentions:
            return True
        # @-autocomplete may pick the bot's managed role instead of the bot user
        if msg.guild and any(r in msg.guild.me.roles for r in msg.role_mentions if r.is_bot_managed()):
            return True
        ref = msg.reference and (msg.reference.resolved or msg.reference.cached_message)
        if isinstance(ref, discord.Message) and ref.author.id == self.user.id:
            return True
        ch = msg.channel
        if ch.id in LISTEN_CHANNELS:
            return True
        if isinstance(ch, discord.Thread) and (ch.parent_id in LISTEN_CHANNELS or ch.owner_id == self.user.id):
            return True
        return False

    def strip_mention(self, msg: discord.Message) -> str:
        text = re.sub(rf"<@!?{self.user.id}>", "", msg.content)
        if msg.guild:
            for r in msg.guild.me.roles:
                if r.is_bot_managed():
                    text = text.replace(r.mention, "")
        return text.strip()

    async def on_message(self, msg: discord.Message):
        if msg.author.bot or not self.is_triggered(msg):
            return
        if msg.author.id not in ALLOWED_USERS:
            log.info("ignored message from unauthorized user %s (%s)", msg.author, msg.author.id)
            return

        text = self.strip_mention(msg)
        where = "DM" if isinstance(msg.channel, discord.DMChannel) else f"#{getattr(msg.channel, 'name', msg.channel.id)}"
        log.info("message from %s in %s: %s%s", msg.author, where, text[:100].replace("\n", " "),
                 f" (+{len(msg.attachments)} files)" if msg.attachments else "")
        key = msg.channel.id
        conv = self.state.get(key)

        if text.startswith("!"):
            if await self.handle_command(msg, key, conv, text):
                return

        if not text and not msg.attachments:
            return

        lock = self.locks.setdefault(key, asyncio.Lock())
        if lock.locked():
            await msg.add_reaction("⏳")
        async with lock:
            try:
                await msg.remove_reaction("⏳", self.user)
            except discord.HTTPException:
                pass
            await self.process(msg, key, conv, text)

    async def handle_command(self, msg, key, conv, text) -> bool:
        cmd, _, arg = text.partition(" ")
        arg = arg.strip()
        if cmd == "!help":
            await msg.reply(
                "**명령어**\n"
                "`!new` 새 대화 시작 (세션 초기화)\n"
                "`!stop` 진행 중인 작업 중단\n"
                "`!status` 현재 세션/작업 디렉터리/모델 정보\n"
                "`!cwd <경로>` 작업 디렉터리 변경 (새 세션 시작)\n"
                "그 외 메시지는 모두 Claude에게 전달됩니다. 채널(스레드)마다 별도 세션입니다."
            )
        elif cmd == "!new":
            conv["session_id"] = None
            self.state.save()
            await msg.reply("🆕 새 대화를 시작합니다.")
        elif cmd == "!stop":
            run = self.runs.get(key)
            if run:
                log.info("stop requested")
                run.stop()
                await msg.reply("🛑 중단 요청을 보냈습니다.")
            else:
                await msg.reply("진행 중인 작업이 없습니다.")
        elif cmd == "!status":
            busy = "실행 중" if key in self.runs else "대기"
            await msg.reply(
                f"봇: `{BOT_NAME}` · 모델: `{MODEL}` · effort: `{EFFORT}` · 권한: `{PERMISSION_MODE}`\n"
                f"작업 디렉터리: `{conv['cwd']}`\n세션: `{conv['session_id'] or '(새 세션)'}` · 상태: {busy}"
            )
        elif cmd == "!cwd":
            if not arg:
                await msg.reply(f"현재 작업 디렉터리: `{conv['cwd']}`")
                return True
            path = os.path.abspath(os.path.expanduser(arg if os.path.isabs(os.path.expanduser(arg)) else os.path.join(conv["cwd"], arg)))
            if not os.path.isdir(path):
                await msg.reply(f"디렉터리가 없습니다: `{path}`")
                return True
            conv["cwd"], conv["session_id"] = path, None
            self.state.save()
            await msg.reply(f"📁 작업 디렉터리를 `{path}` 로 바꾸고 새 대화를 시작합니다.")
        else:
            return False  # not a bot command: pass through to Claude
        return True

    # ---- running claude --------------------------------------------------

    async def save_attachments(self, msg: discord.Message) -> list[str]:
        paths = []
        if not msg.attachments:
            return paths
        d = ATTACH_DIR / str(msg.id)
        d.mkdir(parents=True, exist_ok=True)
        for a in msg.attachments:
            p = d / Path(a.filename).name
            await a.save(p)
            paths.append(str(p))
        return paths

    def build_prompt(self, msg: discord.Message, text: str, files: list[str]) -> str:
        where = "DM" if isinstance(msg.channel, discord.DMChannel) else f"#{getattr(msg.channel, 'name', msg.channel.id)}"
        parts = [f"[Discord {where} · {msg.author.display_name}]", text]
        if msg.reference and isinstance(msg.reference.resolved, discord.Message):
            ref = msg.reference.resolved
            parts.insert(1, f"(답장 대상 메시지 - {ref.author.display_name}):\n> " + ref.content[:1500].replace("\n", "\n> "))
        if files:
            parts.append("첨부 파일:\n" + "\n".join(f"- {p}" for p in files))
        return "\n\n".join(p for p in parts if p)

    def claude_args(self, session_id: str | None) -> list[str]:
        args = [
            CLAUDE_BIN, "-p",
            "--output-format", "stream-json", "--verbose",
            "--model", MODEL,
            "--effort", EFFORT,
            "--permission-mode", PERMISSION_MODE,
            "--permission-prompts", "none",
            "--append-system-prompt", self.persona,
        ]
        if session_id:
            args += ["--resume", session_id]
        return args

    async def process(self, msg: discord.Message, key: int, conv: dict, text: str):
        files = await self.save_attachments(msg)
        prompt = self.build_prompt(msg, text, files)
        status = await msg.reply("🤔 생각 중…", mention_author=False) if SHOW_STATUS else None
        started = time.monotonic()
        run = Run()
        self.runs[key] = run
        try:
            async with msg.channel.typing():
                result = await self.run_claude(run, conv, prompt, status, started)
                if result is None and conv["session_id"] and not run.stopped:
                    # resume failed (session deleted or cwd moved): start fresh once
                    conv["session_id"] = None
                    result = await self.run_claude(run, conv, prompt, status, started)
        except Exception as e:
            log.exception("run failed")
            result = {"is_error": True, "result": f"봇 오류: {e!r}"}
        finally:
            self.runs.pop(key, None)
            self.state.save()

        elapsed = int(time.monotonic() - started)
        if run.stopped:
            await self.notify(msg, status, f"🛑 중단됨 · {elapsed // 60}m{elapsed % 60:02d}s")
            return
        if result is None:
            await self.notify(msg, status, "❌ Claude 실행에 실패했습니다. 로그를 확인하세요.")
            return
        log.info("finished in %ss: %s, %s turns, %s tools, session %s", elapsed,
                 "error" if result.get("is_error") else "ok", result.get("num_turns"),
                 result.get("_tool_count"), conv["session_id"])
        await self.send_result(msg, status, result, elapsed)

    async def notify(self, msg: discord.Message, status: discord.Message | None, content: str):
        if status:
            await status.edit(content=content)
        else:
            await msg.reply(content, mention_author=False)

    async def run_claude(self, run: Run, conv: dict, prompt: str, status: discord.Message | None, started: float):
        cwd = conv["cwd"] if os.path.isdir(conv["cwd"]) else DEFAULT_CWD
        run.proc = await asyncio.create_subprocess_exec(
            *self.claude_args(conv["session_id"]),
            cwd=cwd,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            start_new_session=True,
            limit=64 * 1024 * 1024,
        )
        run.proc.stdin.write(prompt.encode())
        await run.proc.stdin.drain()
        run.proc.stdin.close()

        stderr_task = asyncio.create_task(run.proc.stderr.read())
        steps: list[str] = []
        tool_count = 0
        result = None
        last_edit = 0.0

        async for raw in run.proc.stdout:
            try:
                ev = json.loads(raw)
            except json.JSONDecodeError:
                continue
            t = ev.get("type")
            if t == "system" and ev.get("subtype") == "init":
                conv["session_id"] = ev.get("session_id")
                self.state.save()
            elif t == "assistant" and not ev.get("parent_tool_use_id"):
                for block in ev["message"].get("content", []):
                    if block.get("type") == "tool_use":
                        tool_count += 1
                        steps.append("🔧 " + describe_tool(block["name"], block.get("input", {})))
                    elif block.get("type") == "text" and block["text"].strip():
                        first = block["text"].strip().splitlines()[0]
                        steps.append("💬 " + (first[:150] + ("…" if len(first) > 150 else "")))
            elif t == "result":
                result = ev
                if ev.get("session_id"):
                    conv["session_id"] = ev["session_id"]

            now = time.monotonic()
            if status and steps and now - last_edit > STATUS_EDIT_INTERVAL and result is None:
                last_edit = now
                el = int(now - started)
                body = "\n".join(steps[-8:])
                try:
                    await status.edit(content=f"⚙️ 작업 중 · {el // 60}m{el % 60:02d}s · 도구 {tool_count}회\n{body}"[:1990])
                except discord.HTTPException:
                    pass

        await run.proc.wait()
        stderr = (await stderr_task).decode(errors="replace")
        if result is None and not run.stopped:
            log.error("claude exited %s without result: %s", run.proc.returncode, stderr[-2000:])
        if result and result.get("subtype") == "error_during_execution" and result.get("num_turns") == 0:
            log.error("claude failed before the first turn: %s", result.get("errors"))
            return None  # e.g. resume of a missing session; caller retries fresh
        if result is not None:
            result["_tool_count"] = tool_count
        return result

    async def send_result(self, msg: discord.Message, status: discord.Message | None, result: dict, elapsed: int):
        text = result.get("result") or ""
        if result.get("is_error") and not text:
            text = f"오류: {result.get('subtype')} {result.get('api_error_status') or ''}"

        uploads, missing = [], []
        for m in UPLOAD_RE.finditer(text):
            p = Path(os.path.expanduser(m.group(1)))
            if p.is_file() and p.stat().st_size <= UPLOAD_MAX_BYTES:
                uploads.append(p)
            else:
                missing.append(str(p))
        text = UPLOAD_RE.sub("", text).strip()
        if missing:
            text += "\n\n⚠️ 첨부 실패(없거나 10MB 초과): " + ", ".join(f"`{m}`" for m in missing)

        icon = "⚠️" if result.get("is_error") else "✅"
        tools = result.get("_tool_count", 0)
        if status:
            await status.edit(content=f"{icon} 완료 · {elapsed // 60}m{elapsed % 60:02d}s · 도구 {tools}회 · {result.get('num_turns', '?')}턴")

        files = [discord.File(p) for p in uploads[:10]]
        if len(text) > ATTACH_THRESHOLD:
            full = DATA_DIR / f"reply-{msg.id}.md"
            full.write_text(text, encoding="utf-8")
            files.append(discord.File(full, filename="reply.md"))
            text = text[:ATTACH_THRESHOLD - 200].rsplit("\n", 1)[0] + "\n\n… (전체 내용은 첨부된 reply.md 참고)"

        chunks = split_message(text) if text else []
        for i, chunk in enumerate(chunks):
            last = i == len(chunks) - 1
            kwargs = {"files": files} if last and files else {}
            if i == 0:
                await msg.reply(chunk, mention_author=False, **kwargs)
            else:
                await msg.channel.send(chunk, **kwargs)
        if not chunks and files:
            await msg.reply(files=files, mention_author=False)


def main():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(levelname)s %(message)s")
    Bridge().run(TOKEN, log_handler=None)


if __name__ == "__main__":
    main()
