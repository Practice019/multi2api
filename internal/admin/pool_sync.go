package admin

import (
	"errors"
	"log"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// 本文件：**把凭证目录同步进账号池**，并让"启动时"与"用户点重载"走同一条路。
//
// # 它修的是什么（2026-10-18 实测发现，比"刷新不够频繁"更上游）
//
// 用户报"前端显示的额度是过时的"。查下来最上游的原因是：
//
//	data/state.json 里 `accounts` **是空的**（只有 `Pool.New(cfg.StateFile)`，从不灌池）
//	而 auths/loomy/ 下有 **45 个真凭证**
//
// ⇒ ⚠ `refreshQuotas` 内部靠 `Pool.Status(uid)` ⇒ **拿不到任何账号**
//   ⇒ **额度无论如何都刷不进去** —— 不管刷新循环跑得多勤。
//
// ⚠ 而"灌池"那条路**早就写好了**：`/admin/accounts/reload` 端点（`accountsReload`）
//   扫凭证目录 → `poolAuthsFromCreds` 投影 → `Pool.SyncToDir*`。
//   **问题只是：它只在有人点按钮时才发生**。
//   ⇒ 进程起来之后、用户点之前，池子一直是空的。
//
// 所以本文件把那段逻辑抽成 `reloadProviderInto`，让启动流程与端点**共用一份**
// （AGENTS 第 6 条：同类型代码能抽就抽 —— 抄第二份的代价是"将来只改对一处"）。

// reloadProviderInto 扫一个上游的凭证目录，并把它同步进账号池。
//
// 返回 (scanned, before, after, dir, err)：
//   - scanned 扫到的凭证条数
//   - before/after 同步前后池子里的账号条数（⚠ 补数人最关心的一对数）
//   - dir 真正扫的那个目录（排障时"到底扫了哪"一眼可见）
//
// ⚠ 目录判据**逐字**与 `accountsReload` 原来那段一致（上游自报优先，回落
// `auth.UpstreamDir`）—— 抽出来时没有动任何一条语义，只把
// `writeError` 换成了返回 error，让调用方能自己决定怎么报。
func (h *Handler) reloadProviderInto(provider string) (scanned, before, after int, dir string, err error) {
	if h == nil {
		return 0, 0, 0, "", errNoProvider
	}
	if strings.TrimSpace(provider) == "" {
		return 0, 0, 0, "", errNoProvider
	}
	// ⚠⚠ **池子为 nil 时不早退**（这是抽函数时踩到的一个回归，被
	//   `TestReloadNilPoolDoesNotPanic` / `TestReloadFallsBackToUpstreamDirWhenNoAppDirExt` 抓到）：
	//   "扫了哪个目录、扫到几条"**与池子在不在无关** —— 回执里的 `dir` 是排障时
	//   最常被问的那个答案，凭"没池子"就把它吞掉等于把排障手段一起吞掉。
	//   ⇒ 扫描照做，只在**写池那一步**判空。

	// 兼容读的根：`h.cfg.AuthDir` 现在是默认上游的子目录（`auths/workbuddy/`），
	// 但迁移期凭证可能还在 `auths/` 根。
	base := h.cfg.AuthsBase
	if base == "" {
		base = h.cfg.AuthDir
	}

	// 目录：**上游自报优先**（目录是上游的事实，核心不该猜）。
	dir = ""
	if p, ok := h.providerByID(provider); ok {
		if ext, ok2 := gateway.ExtOf[gateway.AuthDirExt](p); ok2 {
			dir = ext.AuthDir()
		}
	}
	if dir == "" {
		// 回落也走 auth.UpstreamDir —— **不自己拼 filepath.Join**：
		// "auths/<provider>/ 长什么样"这个判据只该写一份。
		dir = auth.UpstreamDir(base, provider)
	}

	// 用**上游自己的解析器**。⚠ 核心不硬编码任何上游的凭证格式 ——
	// 这里曾经用 `auth.LoadDirCompat`（workbuddy 的解析器）扫 codearts，
	// 结果把 workbuddy 的旧凭证当成了 codearts 的账号（见 accountsReload 的注释）。
	loaded, exists := h.credentialsOf(provider)
	if !exists {
		return 0, 0, 0, dir, errNoCredentialLoader
	}

	// 优先用「带 secret」的扫描器（可选能力）——否则池里那些新 uid 会"没有凭证"，
	// 该号被选中时取不到可用的上游 Auth ⇒ 必失败。
	var creds []gateway.Credential
	var secrets map[string]any
	if withSecrets, ok := h.credentialSecretsOf(provider); ok {
		items, serr := withSecrets(dir)
		if serr != nil {
			return 0, 0, 0, dir, serr
		}
		secrets = make(map[string]any, len(items))
		for _, it := range items {
			creds = append(creds, it.Credential)
			if it.Secret != nil {
				secrets[it.Credential.UID] = it.Secret
			}
		}
	} else {
		var cerr error
		creds, cerr = loaded(dir)
		if cerr != nil {
			return 0, 0, 0, dir, cerr
		}
	}

	auths := poolAuthsFromCreds(creds, secrets)
	scanned = len(auths)

	// ⚠ 池子可能为 nil（本包其它地方都判了空）。漏判的后果是**进程级 panic**。
	//   ⚠ 但**扫描已经做完了**（上面），所以这里只跳过"写池"那一步 ——
	//     回执里的 scanned/dir 仍然有效（见函数开头那段回归记录）。
	if h.cfg.Pool == nil {
		log.Printf("admin: %s 扫描到 %d 个凭证，但没有账号池可同步（dir=%s）", provider, scanned, dir)
		return scanned, 0, 0, dir, nil
	}

	before = len(h.cfg.Pool.List())
	if secrets != nil {
		h.cfg.Pool.SyncToDirWithSecrets(provider, auths, secrets)
	} else {
		h.cfg.Pool.SyncToDirFor(provider, auths)
	}
	after = len(h.cfg.Pool.List())

	// ⚠ 日志打 `dir=`（真正扫的目录）而不是 `h.cfg.AuthDir` ——
	// 按上游扫描之后两者不再是同一个值，打配置值等于没打。
	log.Printf("admin: 同步凭证进池 dir=%s provider=%s scanned=%d pool %d -> %d",
		dir, provider, scanned, before, after)
	return scanned, before, after, dir, nil
}

// SyncPoolFromCredentials 对**每个已注册上游**做一次"凭证目录 → 账号池"的同步。
//
// ⚠⚠ 这是**启动时**必须跑的一步：不跑的话池子是空的，而症状是
// "额度永远是旧的 / 选号没有账号 / 账号池页面空着"，且**不会报错**。
//
//	实测（2026-10-18）：`data/state.json` 里 accounts 为空，而 `auths/loomy/` 有 45 个凭证
//	⇒ `refreshQuotas` 拿不到一个账号 ⇒ 额度刷新循环等于空转。
//
// ⚠ 每个上游各自一个 `recover`：某个上游的解析器 panic 不该让整轮同步停摆
//
//	（其余上游的账号本来是对的，不该被连坐）。
//
// ⚠ 失败**只记日志、不返回错误**：调用方是启动流程，它不该因为"某个上游目录
//
//	暂时读不到"而拒绝启动 —— 用户点一次「重载 auths」就能补上。
//
// ⚠ 返回"新增到池子里的账号总数"，供日志与测试断言。
func (h *Handler) SyncPoolFromCredentials() int {
	if h == nil || h.cfg.Pool == nil {
		return 0
	}
	reg := h.cfg.Registry
	if reg == nil {
		return 0
	}

	total := 0
	for _, p := range reg.All() {
		pid := p.ID()
		if strings.TrimSpace(pid) == "" {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("admin: ⚠ 同步上游 %s 的凭证时 panic 已被隔离：%v", pid, r)
				}
			}()
			_, before, after, dir, err := h.reloadProviderInto(pid)
			if err != nil {
				// ⚠ 不是所有上游都必须有凭证目录才能启动：没实现
				//   `CredentialLoader` 的上游天然没有，那是**正常**的，不是错。
				if errors.Is(err, errNoCredentialLoader) {
					return
				}
				log.Printf("admin: 跳过上游 %s 的凭证同步（dir=%s）：%v", pid, dir, err)
				return
			}
			if n := after - before; n != 0 {
				total += n
			}
		}()
	}
	if total != 0 {
		log.Printf("admin: ✅ 启动同步完成 —— 从凭证目录补进池子 %d 个账号（池子现有 %d 个）",
			total, len(h.cfg.Pool.List()))
	}
	return total
}

var (
	// errNoProvider 调用方没给上游 id。
	errNoProvider = errors.New("未指定上游")
	// errNoCredentialLoader 该上游没有声明 `gateway.CredentialLoader`。
	//
	// ⚠ 这是**正常情况**而不是错误：凭证格式是上游的事实，核心不硬编码；
	//   一个上游不声明加载器，就说明"它没有可从目录扫描的凭证"。
	//   调用方据此**静默跳过**（否则每次启动都会为每个这样的上游打一行噪音）。
	errNoCredentialLoader = errors.New("上游未声明 CredentialLoader（无可扫描凭证）")
)
