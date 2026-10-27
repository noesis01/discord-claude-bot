"""Python 원본(bot.py)의 split_message·describe_tool 결과를 testdata/golden.json 으로 저장 - Go 테스트가 같은지 비교.
실행: DISCORD_TOKEN=x <discord.py 가 있는 python> Go/testdata/gen_golden.py   (저장소 루트에서)"""
import json, os, random, sys
sys.path.insert(0, os.getcwd())
import bot

random.seed(7)
fence = "```"
cases = {
    "empty": "",
    "short": "안녕하세요\n두 번째 줄",
    "crlf_and_seps": "a\r\nb\rc\x0bd\x0ce f g\x85h",
    "long_line": "가" * 5000,
    "long_mixed": "\n".join(("줄 %d " % i) + "x" * random.randint(0, 300) for i in range(80)),
    "fence_across": "앞부분\n" + fence + "python\n" + "\n".join("print(%d)  # %s" % (i, "y" * 60) for i in range(120)) + "\n" + fence + "\n뒷부분",
    "two_fences": (fence + "\n" + "a\n" * 900 + fence + "\n중간\n" + fence + "bash\n" + "echo hi\n" * 400 + fence),
    "unclosed_fence": fence + "json\n" + '{"k": "' + "v" * 2500 + '"}\n' * 3,
    "whitespace_only": "   \n\n  ",
    "emoji": "🙂" * 2100 + "\n끝",
}
tools = [
    ["Bash", {"command": "ls   -la\n /tmp && echo " + "z" * 100}],
    ["Read", {"file_path": "/a/b.txt"}],
    ["NotebookEdit", {"notebook_path": "/n.ipynb"}],
    ["Edit", {"file_path": "", "notebook_path": "/x"}],
    ["Grep", {"pattern": "foo.*bar"}],
    ["WebFetch", {"url": "https://example.com/" + "p" * 100}],
    ["WebSearch", {"query": "디스코드 봇"}],
    ["Agent", {"description": "분석 작업"}],
    ["Bash", {}],
    ["TodoWrite", {"todos": []}],
]
out = {"split": {k: {"input": v, "output": bot.split_message(v)} for k, v in cases.items()},
       "tools": [{"name": n, "input": i, "output": bot.describe_tool(n, i)} for n, i in tools]}
json.dump(out, open("Go/testdata/golden.json", "w", encoding="utf-8"), ensure_ascii=False, indent=1)
print("cases", len(cases), "tools", len(tools))
