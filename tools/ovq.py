# -*- coding: utf-8 -*-
"""直接调 OpenViking 的 MCP over HTTP（绕开 DSH 那条断线的 MCP 客户端）。

⚠ 为什么需要它（2026-10-18）：
  DSH 侧的 MCP 客户端报 `server is disconnected`，而**服务端本身是活的**
  （POST http://127.0.0.1:1933/mcp 返回 200，pid=30480 python）。
  它的协议是 MCP over HTTP + SSE，且 `ov.conf` 里 `root_api_key: null` ⇒ 无需鉴权
  ⇒ 直接发 JSON-RPC 就能查，不必等 DSH 重连。

用法：
    python tools/ovq.py grep 'new-api' 'GetRandomSatisfiedChannel|balance|quota'
    python tools/ovq.py find 'new-api 渠道选择 余额不足'
    python tools/ovq.py read 'viking://resources/repos/...'
    python tools/ovq.py tools          # 列出可用工具
"""
import json
import sys
import urllib.request

URL = "http://127.0.0.1:1933/mcp"
HDRS = {
    "Content-Type": "application/json",
    "Accept": "application/json, text/event-stream",
}


def call(method: str, params: dict, timeout: int = 120):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    req = urllib.request.Request(URL, data=body, headers=HDRS, method="POST")
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read().decode("utf-8", "replace")
    # ⚠ 响应是 SSE：可能多行 `event: message` + `data: {...}`
    for line in raw.splitlines():
        line = line.strip()
        if line.startswith("data:"):
            try:
                return json.loads(line[5:].strip())
            except Exception:
                continue
    try:
        return json.loads(raw)
    except Exception:
        return {"_raw": raw}


def tool(name: str, args: dict, timeout: int = 180):
    """调一个 MCP tool。"""
    r = call("tools/call", {"name": name, "arguments": args}, timeout=timeout)
    if "error" in r:
        return {"error": r["error"]}
    out = r.get("result") or {}
    # ⚠ 内容在 content[].text（可能是 JSON 字符串）
    texts = []
    for c in out.get("content") or []:
        if isinstance(c, dict) and c.get("type") == "text":
            texts.append(c.get("text") or "")
    joined = "\n".join(texts)
    try:
        return json.loads(joined)
    except Exception:
        return {"_text": joined}


def emit(r, limit=9000):
    """⚠ 输出**纯文本**：工具回的 `content[].text` 本来就是要给人读的，
    再套一层 json.dumps 只会把换行转成 \\n、把中文转成 \\uXXXX ⇒ 没法看。"""
    if isinstance(r, dict) and set(r.keys()) == {"_text"}:
        t = r["_text"]
    else:
        t = json.dumps(r, ensure_ascii=False, indent=1)
    print(t[:limit])


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return
    cmd = sys.argv[1]
    if cmd == "tools":
        r = call("tools/list", {})
        for t in (r.get("result") or {}).get("tools") or []:
            print("  %-16s %s" % (t.get("name"), (t.get("description") or "").split("\n")[0][:96]))
        return
    if cmd == "grep":
        uri = sys.argv[2]
        pats = sys.argv[3].split("|")
        emit(tool("grep", {"uri": uri, "pattern": pats, "case_insensitive": True, "node_limit": 20}))
        return
    if cmd == "find":
        emit(tool("find", {"query": sys.argv[2], "limit": 12, "read_content": False}))
        return
    if cmd == "read":
        emit(tool("read", {"uris": [sys.argv[2]]}), limit=14000)
        return
    if cmd == "ls":
        emit(tool("list", {"uri": sys.argv[2] if len(sys.argv) > 2 else "viking://", "limit": 300}))
        return
    if cmd == "tree":
        emit(tool("tree", {"uri": sys.argv[2], "level_limit": int(sys.argv[3]) if len(sys.argv) > 3 else 2,
                           "node_limit": 400}))
        return
    print("未知子命令:", cmd)


main()
