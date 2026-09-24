# -*- coding: utf-8 -*-
# mimo_sync_addon.py — mitmproxy 插件：桌面端换发 serviceToken 时自动同步到 multi2api。
#
# 装法（一次性）：跑 mitmdump 时带 -s：
#   mitmdump --listen-port 8080 -s mimo_sync_addon.py
# 之后用你教程 §3.1 的方式让桌面端走这个代理；每次小米给它 307 Set-Cookie
# serviceToken，本插件立刻把最新四件套 POST 到网关 /admin/mimo/sync ——
# 桌面重启换票 = 网关自动跟上，全程零手动。
#
# 配置：环境变量 MIMO_GW（默认 7864 测试实例）与 MIMO_GW_TOKEN（必填）。
import json
import urllib.request

import os

GW    = os.environ.get("MIMO_GW", "http://8.148.203.162:7864")
TOKEN = os.environ.get("MIMO_GW_TOKEN", "")  # 管理钥匙走 env，别写进文件

jar = {}  # 攒账号套四件套（passToken 会被 serviceLogin 续签 —— 抓最新的）


def response(flow):
    host = flow.request.pretty_host or ""
    if "mimo-server-cn" not in host and "xiaomimimo.com" not in host:
        return
    try:
        sets = flow.response.headers.get_all("set-cookie") or []
    except Exception:
        return
    touched = False
    for sc in sets:
        first = (sc.split(";", 1)[0] or "").strip()
        if "=" not in first:
            continue
        name, val = first.split("=", 1)
        name = name.strip().lower()
        if name in ("passtoken", "cuserid", "userid", "deviceid"):
            if val and jar.get(name) != val:
                jar[name] = val
                touched = True
    if not touched or "passtoken" not in jar or "cuserid" not in jar or "userid" not in jar:
        return
    cookie = "passToken={0}; cUserId={1}; userId={2}; deviceId={3}".format(
        jar.get("passtoken", ""), jar.get("cuserid", ""), jar.get("userid", ""), jar.get("deviceid", ""))
    try:
        body = json.dumps({"cookie": cookie}).encode()
        req = urllib.request.Request("%s/admin/mimo/sync?token=%s" % (GW, TOKEN),
                                     data=body, headers={"Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=30) as r:
            out = json.loads(r.read().decode())
        print("[mimo-sync] gateway ->", json.dumps(out, ensure_ascii=False))
    except Exception as e:
        print("[mimo-sync] POST failed:", e)
