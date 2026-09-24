#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""scripts/mimo/read-mimo-cookies.py — 统一从浏览器获取小米账号 Cookie（passToken 套），
彻底解耦桌面端：桌面客户端软件不再是必需项。

来源顺序（任一命中即用；浏览器优先，桌面库最后兜底）：
  1. Edge 全部 profile（Default + Profile N）   — encrypted_value v10 → DPAPI 解密
  2. Chrome 全部 profile（Default + Profile N） — 同上（v20 app-bound 无法 DPAPI 解 → 跳过）
  3. MiMo 桌面端 Cookie 库（value 明文；仅当本机装过客户端且登录过）

账号匹配：
  传 --uid=<登录账号userId> 时只输出 userId==uid 的会话套（多 profile 多账号时
  精确取登录账号的 passToken；不匹配 → 退出码 1，网关保持 paid 并提示）。

输出 JSON: {"passToken","cUserId","userId","deviceId"}
全部未命中 → 退出码 1 + stderr 原因（网关探测失败 = 登录保持 paid 兜底，不阻断）。
"""
import ctypes
import ctypes.wintypes as wt
import glob
import json
import os
import secrets
import shutil
import sqlite3
import sys
import tempfile

LOCAL = os.environ.get("LOCALAPPDATA", "")
APPDATA = os.environ.get("APPDATA", "")

WANT_UID = None
for _a in sys.argv[1:]:
    if _a.startswith("--uid="):
        WANT_UID = _a.split("=", 1)[1]


def browser_cookie_dbs(base, browser):
    """Edge/Chrome 全部 profile 的 Cookies 路径。"""
    root = os.path.join(LOCAL, browser, "User Data")
    if not os.path.isdir(root):
        return []
    dbs = []
    for prof in ["Default"] + sorted(
            p for p in glob.glob(os.path.join(root, "Profile*"))
            if os.path.isdir(p)):
        dbs.append(os.path.join(prof, "Network", "Cookies"))
    return dbs


CANDIDATES = (
    browser_cookie_dbs(LOCAL, "Microsoft/Edge") + browser_cookie_dbs(LOCAL, "Google/Chrome")
    + [
        # 桌面端兜底（value 列明文；无客户端软件时这两个不存在，直接跳过）
        os.path.join(APPDATA, "Xiaomi MiMo", "Partitions", "xiaomi-account", "Network", "Cookies"),
        os.path.join(APPDATA, "Xiaomi MiMo", "Network", "Cookies"),
    ]
)
NEED = ("passtoken", "cuserid", "userid", "deviceid")


class DATA_BLOB(ctypes.Structure):
    _fields_ = [("cbData", wt.DWORD), ("pbData", ctypes.POINTER(ctypes.c_char))]


def dpapi_unprotect(blob):
    out = DATA_BLOB()
    if not ctypes.windll.crypt32.CryptUnprotectData(
            ctypes.byref(blob), None, None, None, None, 0, ctypes.byref(out)):
        return None
    data = ctypes.string_at(out.pbData, out.cbData)
    ctypes.windll.kernel32.LocalFree(out.pbData)
    return data


def decrypt_value(raw):
    """Chromium cookie：value 明文 / encrypted_value v10(DPAPI)。v20(APP_BOUND) 无法解 → None。"""
    if raw is None:
        return ""
    if isinstance(raw, str):
        return raw
    if not raw:
        return ""
    if raw[:3] == b"v10":
        blob = DATA_BLOB(len(raw) - 3, ctypes.cast(raw[3:], ctypes.POINTER(ctypes.c_char)))
        d = dpapi_unprotect(blob)
        return d.decode("utf-8", "replace") if d else ""
    if raw[:3] == b"v20":
        return None  # app-bound：本进程解不了，跳过该 cookie
    return raw.decode("utf-8", "replace")


def read_db(db_path):
    """{name_lower: value}，仅 NEED 项。打不开返回 {}。"""
    tmp = os.path.join(tempfile.gettempdir(), "mimo_ck_%d" % os.getpid())
    try:
        shutil.copy2(db_path, tmp)  # 绕运行中文件锁
    except Exception:
        tmp = db_path
    try:
        con = sqlite3.connect("file:" + tmp.replace("\\", "/") + "?mode=ro", uri=True)
    except Exception:
        return {}
    jar = {}
    try:
        cur = con.cursor()
        cur.execute("SELECT name, value, encrypted_value FROM cookies "
                    "WHERE host_key LIKE '%xiaomi%'")
        for name, val, enc in cur.fetchall():
            n = str(name).lower()
            if n not in NEED:
                continue
            v = ""
            if val:
                v = str(val)
            elif enc:
                v = decrypt_value(enc)
                if v is None:
                    continue  # v20 不可解
            if v:
                jar[n] = v
    except Exception:
        pass
    finally:
        try:
            con.close()
        except Exception:
            pass
        if tmp != db_path:
            try:
                os.remove(tmp)
            except Exception:
                pass
    return jar


def main():
    last = "no cookie db found"
    mismatch = None
    for db in CANDIDATES:
        if not (db and os.path.exists(db)):
            continue
        jar = read_db(db)
        if not (jar.get("passtoken") and jar.get("cuserid")):
            last = "db %s readable but missing passToken/cUserId" % db
            continue
        uid = jar.get("userid", "")
        if WANT_UID and uid != WANT_UID:
            mismatch = "found session of uid=%s, want %s (db: %s)" % (uid or "?", WANT_UID, db)
            continue
        # route 通道只认 PC 客户端设备指纹（pc_ 前缀）；网页登录的 wb_ 指纹
        # 会被 serviceLogin 拒绝 → 统一规范化为 pc_ + 32hex。
        dev = jar.get("deviceid", "")
        if dev and not dev.startswith("pc_"):
            dev = "pc_" + secrets.token_hex(16)
        print(json.dumps({
            "passToken": jar["passtoken"],
            "cUserId": jar["cuserid"],
            "userId": uid,
            "deviceId": dev,
        }))
        sys.exit(0)
    if mismatch:
        sys.stderr.write(mismatch + "\n")
    else:
        sys.stderr.write(last + "\n")
    sys.exit(1)


if __name__ == "__main__":
    main()
