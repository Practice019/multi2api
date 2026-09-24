# -*- coding: utf-8 -*-
# mimo_sync.py — 一次性上送 passToken 套到 multi2api 网关（此后换票由网关全自动完成）。
#
# 用法（Windows）：
#   1) 完全退出 MiMo 桌面端（Cookie 库会被占用；退出后 value 列为明文 —— 探测报告 §5.3）
#   2) python mimo_sync.py
#   3) 重新打开桌面端（不影响，之后桌面端换票无需再管 —— 网关 SSO 链自己滚动）
#
# 只推 30 天的 passToken+cUserId+userId+deviceId（都不是密钥级 secret 里最敏感的
# serviceToken 反而不用推：网关会现场用 SSO 链换）。传输走你网关的 api_key 鉴权。
import json
import os
import shutil
import sqlite3
import sys
import tempfile
import urllib.request

GW    = os.environ.get("MIMO_GW", "http://8.148.203.162:7864")  # 测试实例；生产改 7863
TOKEN = os.environ.get("MIMO_GW_TOKEN", "")              # 管理钥匙（建议 env，别写文件里）

# 桌面端 Cookie 库候选（探测报告 §2.2：分区目录为主，兼容根目录）
DB_CANDIDATES = [
    os.path.join(os.environ.get("APPDATA", ""), "Xiaomi MiMo", "Partitions", "xiaomi-account", "Network", "Cookies"),
    os.path.join(os.environ.get("APPDATA", ""), "Xiaomi MiMo", "Network", "Cookies"),
]
NEED = ("passToken", "cUserId", "userId", "deviceId")


def grab_cookies(db):
    tmp = os.path.join(tempfile.gettempdir(), "mimo_ck_copy")
    shutil.copy2(db, tmp)  # 绕开运行中的文件锁
    con = sqlite3.connect("file:" + tmp.replace("\\", "/") + "?mode=ro", uri=True)
    cur = con.cursor()
    cur.execute("SELECT name, value, host_key, expires_utc FROM cookies "
                "WHERE host_key LIKE '%xiaomi%' AND name IN (%s)" % ",".join("?" * len(NEED)), NEED)
    jar = {}
    for name, value, host, exp in cur.fetchall():
        if name not in jar or (exp or 0) > 0:  # 多域重复时取有过期时间的条目
            jar[name] = value
    con.close()
    os.remove(tmp)
    return jar


def main():
    if not TOKEN:
        sys.exit("先设环境变量 MIMO_GW_TOKEN=<网关管理钥匙>")
    db = next((p for p in DB_CANDIDATES if p and os.path.exists(p)), None)
    if not db:
        sys.exit("找不到桌面端 Cookie 库（确认 MiMo 已登录过）: %s" % DB_CANDIDATES)
    jar = grab_cookies(db)
    missing = [n for n in NEED if not jar.get(n)]
    if "passToken" in missing or "cUserId" in missing:
        sys.exit("Cookie 库缺 %s —— passToken/cUserId 是 SSO 链最低要求" % missing)
    cookie = "; ".join("%s=%s" % (n, jar[n]) for n in NEED if jar.get(n))
    body = json.dumps({"cookie": cookie}).encode()
    req = urllib.request.Request("%s/admin/mimo/sync?token=%s" % (GW, TOKEN),
                                 data=body, headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            out = json.loads(r.read().decode())
    except Exception as e:
        sys.exit("同步失败: %s" % e)
    print("网关回执:", json.dumps(out, ensure_ascii=False))
    if out.get("ok"):
        print("✅ passToken 已入池（网关会自己跑 SSO 链换 serviceToken，约 30 天内免维护）")
    else:
        print("❌ passToken 无效或已过期：重开桌面端登录一次再跑本脚本")


if __name__ == "__main__":
    main()
