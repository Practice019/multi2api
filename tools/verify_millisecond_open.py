#!/usr/bin/env python3
"""验收「打开页面**毫秒**加载整页数据」—— 唯一能证明用户体感的测量。

# 为什么单独一个文件（AGENTS 第 2 条：尽可能多写独立文件）

这个测量要反复跑（每次改缓存/预热都要重测），而它需要：起一个**保留磁盘缓存**的
实例、用真实浏览器打开、按"数据真的画出来了"那一刻计时。把它混进别的一次性脚本里，
下次就得重写一遍。

# 它测什么（以及**不**测什么）

⚠ 测的是**用户体感**：从 `goto` 到「表格里出现了真实数字」的毫秒数。
⚠ **不是**测 HTTP 耗时 —— 服务端 `/admin/loomy/tasks?page=1` 早就 4~24ms 了
（2026-10-18 实测），而用户打开页面却要等好几秒。**两者差距的来源**才是要测的东西：

	① 前端 `lazyCache`（localStorage）里有没有上一份 ⇒ 有则**同步画**、零请求
	② 服务端 `auths/loomy/panels.json` 里有没有数据 ⇒ 有则那次 `?page=` 请求
	   回的是真数据而不是 `stale: true` 占位

⇒ 所以本脚本**必须保留** `panels.json`（那是"正常重启"的形状），
  ⚠ 而不是像冷启动那样删掉它 —— 删了测的就是"第一次"（那必然慢，且不是用户日常路径）。

# 用法

	python tools/verify_millisecond_open.py            # 保留缓存（日常路径）
	python tools/verify_millisecond_open.py --cold     # 删掉缓存（冷启动，首个打开）
"""

import json
import os
import re
import subprocess
import sys
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PORT = 7864
BASE = f"http://127.0.0.1:{PORT}"
PANELS = os.path.join(ROOT, "auths", "loomy", "panels.json")
PROBE_CFG = os.path.join(ROOT, "config-7864-probe.json")


def _kill_port(port: int) -> None:
    """按**端口查 pid**再杀 —— 不按进程名（那会误杀用户的 7863）。"""
    try:
        out = subprocess.run(["pwsh", "-NoProfile", "-Command",
                              f"Get-NetTCPConnection -LocalPort {port} -State Listen "
                              f"-ErrorAction SilentlyContinue | Select-Object -First 1 -ExpandProperty OwningProcess"],
                             capture_output=True, text=True, timeout=30).stdout.strip()
        if out.isdigit():
            subprocess.run(["taskkill", "/PID", out, "/F"], capture_output=True, timeout=30)
            time.sleep(2)
    except Exception:
        pass


def _write_probe_cfg() -> str:
    with open(os.path.join(ROOT, "config.json"), encoding="utf-8") as f:
        cfg = json.load(f)
    cfg["listen"] = f"127.0.0.1:{PORT}"
    with open(PROBE_CFG, "w", encoding="utf-8") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)
    return cfg.get("api_key", "")


def _start_server() -> None:
    subprocess.Popen(
        [os.path.join(ROOT, "bin", "wb2api-server.exe"), "-config", os.path.basename(PROBE_CFG)],
        cwd=ROOT, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))


def _wait_up(timeout=60) -> bool:
    t0 = time.time()
    while time.time() - t0 < timeout:
        try:
            urllib.request.urlopen(BASE + "/", timeout=2)
            return True
        except Exception:
            time.sleep(0.5)
    return False


def _measure(api_key: str, label: str) -> dict:
    """用真实浏览器测"到出现真实数字"的毫秒数。"""
    from playwright.sync_api import sync_playwright

    res = {"label": label}
    with sync_playwright() as p:
        b = p.chromium.launch()
        ctx = b.new_context(viewport={"width": 1600, "height": 950})
        pg = ctx.new_page()
        pg.add_init_script("try{localStorage.setItem('wb2api.key', %r);}catch(e){}" % api_key)

        t0 = time.time()
        pg.goto(BASE + "/", wait_until="domcontentloaded", timeout=60000)
        res["goto_ms"] = round((time.time() - t0) * 1000)

        # 点开「新手任务」（面板渲染的入口）
        t_click = time.time()
        pg.evaluate("""() => {
            const b = [...document.querySelectorAll('button[data-nav]')]
                      .find(x => (x.textContent||'').includes('新手任务'));
            if (b) b.click();
        }""")
        res["click_ms"] = round((time.time() - t_click) * 1000)

        # ⚠ 等到"表格里出现了真实数字"—— 这是用户肉眼看到数据的那一刻。
        #   上限 20 秒：超过就说明这次没有走到"毫秒"路径，要把实际情况报出来。
        deadline = time.time() + 20
        state = {"rows": 0, "reading": 0, "nums": 0}
        while time.time() < deadline:
            state = pg.evaluate("""() => {
                const t = document.querySelector('div[id$="-tasks"] table tbody');
                if (!t) return {rows: 0, reading: 0, nums: 0};
                const rs = [...t.querySelectorAll('tr')];
                return {
                    rows: rs.length,
                    reading: rs.filter(r => (r.textContent||'').includes('正在读取')).length,
                    // "有数字的行" = 已得积分那列不是占位
                    nums: rs.filter(r => /\\d+\\/\\d+/.test(r.textContent||'')).length,
                };
            }""")
            if state["nums"] > 0:
                break
            pg.wait_for_timeout(50)
        res["first_data_ms"] = round((time.time() - t_click) * 1000)
        res.update(state)
        pg.screenshot(path=os.path.join(ROOT, "shot_verify_ms.png"))
        b.close()
    return res


def main() -> int:
    cold = "--cold" in sys.argv
    if cold and os.path.exists(PANELS):
        os.remove(PANELS)
        print('  [--cold] 已删 panels.json（测的是「第一次打开」，那必然要等）')
    elif os.path.exists(PANELS):
        n = 0
        try:
            with open(PANELS, encoding="utf-8") as f:
                d = json.load(f)
            n = len(d.get("tasks") or {})
        except Exception:
            pass
        print(f"  保留 panels.json（里面有 {n} 条 tasks）—— 这就是**正常重启**的形状")

    _kill_port(PORT)
    api_key = _write_probe_cfg()
    _start_server()
    if not _wait_up():
        print("  ❌ 实例没起来")
        return 1
    print("  实例已起，等 20 秒让预热第一轮跑起来…")
    time.sleep(20)

    r1 = _measure(api_key, "第一次打开")
    print(f"\n  ┌─ {r1['label']} ─────────────────────────")
    print(f"  │ goto           {r1['goto_ms']:>6} ms")
    print(f"  │ 点「新手任务」  {r1['click_ms']:>6} ms")
    print(f"  │ ⚠ **首屏有数据** {r1['first_data_ms']:>6} ms   ← 用户体感就看这个")
    print(f"  │ 行={r1['rows']} · 有数字的行={r1['nums']} · 仍在「正在读取」={r1['reading']}")
    verdict = "✅ 毫秒级" if r1["first_data_ms"] < 1000 else ("⚠ 偏慢（<3s）" if r1["first_data_ms"] < 3000 else "❌ 仍需等待")
    print(f"  └─ 判定：{verdict}")

    _kill_port(PORT)
    for f in (PROBE_CFG,):
        try:
            os.remove(f)
        except Exception:
            pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
