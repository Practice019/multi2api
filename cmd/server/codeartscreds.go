// codeartscreds.go codearts 凭证的**进程内唯一所有者**（单一所有者 store）。
//
// # 这个文件修的是什么（用户实测的 503 no_healthy_account）
//
// codearts 上游的 STS 凭证约 2 小时过期，续期用的是**一次性** refresh_token
// （用一次即作废，服务端报 STS5.1806 "the refresh token has been used"）。
//
// 改造前，同一份 `auths/codearts/*.json` 在进程里被表示成**两个不同的对象**：
//
//	池 secret 那份 ← cmd/server/multiprovider.go 的 syncCodeartsAccounts 在启动时 LoadDir 一次
//	后台任务那份   ← main.go 里 cb.SetAccounts 的闭包**每次调用都 LoadDir**
//
// 而 `Auth.refreshMu` 是**对象级**锁 —— 跨对象完全无效。于是后台续期任务
// 消费掉一次性 token、把新凭证写进自己那份对象并落盘之后，
//
//	池里那份永远停在**已作废的旧值**上，且**永不回读磁盘**
//	→ 所有对话请求续期失败 → 熔断 → 503 no_healthy_account: the refresh token has been used
//
// 磁盘上的新 token 其实是有效且从未被用过的（重启即恢复）—— 但那只是掩盖，
// 条件复现就会复发。
//
// # 不变量（这个文件存在的全部理由）
//
//	uid → **进程内唯一一个** *codearts.Auth 指针
//
// 池 secret、后台续期任务、管理端点三处必须共享它。任何一处自己造对象，
// 续期写回就有一份打不到，503 必然复发。
//
// # 为什么不能只做"缓存"
//
// 缓存只解决"不要重复构造"，解决不了**目录新鲜度**：用户跑完 cmd/login
// 或删掉一份凭证后，下一次 List 必须立刻反映出来。所以每次 List 都重新
// 枚举磁盘（低频路径，代价可忽略），但**按 uid 合并回已有对象**：
//
//	新 uid            → 用新解析的对象
//	同 uid、同路径    → 保留原指针，**一个字都不从磁盘回抄**（见下）
//	同 uid、不同路径  → 在原地更新字段，指针不变
//	磁盘上消失的 uid  → 从 store 移除
//
// 第二条是防止"把自己的在途续期结果覆盖回旧值"的关键，见 list 里的注释。
package main

import (
	"fmt"
	"log"
	"sync"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/pool"
)

// codeartsCredStore 保证「一个 uid = 一个进程内 *codearts.Auth 对象」。
//
// 并发：会被「每 refresh_interval 一次的后台任务」（Provider.localAccounts →
// SetAccounts 的闭包）与「请求路径中的模型目录 / 管理端点」同时调用，
// 因此**每个方法都自己加锁**，调用方不需要（也不应该）在外面加。
type codeartsCredStore struct {
	// mu 保护 byUID 与整个"枚举磁盘 + 合并"的过程。
	//
	// 用普通 Mutex 而不是 RWMutex：List 本身是写操作（可能建对象、改字段），
	// 而且这是启动期/低频路径，读写锁的收益到不了需要权衡的量级。
	mu sync.Mutex
	// dir 凭证目录（`codearts*.json`）。
	dir string
	// load 注入的目录加载器，默认 codearts.LoadDir。
	// 抽成字段是为了单测能替换成"坏目录 / 特定返回"而不用真的造盘。
	load func(string) ([]*codearts.Auth, error)
	// byUID 是**唯一**的 uid → 对象映射。所有对外方法都由它派生，
	// 因此三处调用方看到的是同一批指针。
	byUID map[string]*codearts.Auth
}

// newCodeartsCredStore 建一个 store。dir 为空时 List 返回空（守卫见 list()）。
func newCodeartsCredStore(dir string) *codeartsCredStore {
	return &codeartsCredStore{
		dir:   dir,
		load:  codearts.LoadDir,
		byUID: make(map[string]*codearts.Auth),
	}
}

// List 返回当前目录对应的凭证列表（**副本切片**，元素是 store 持有的指针）。
//
// 每次调用都重新枚举磁盘 —— 这是回调路径（SetAccounts）的语义要求：
// 用户刚跑完 cmd/login 或刚删掉一份凭证，下一次调用就要看到。
//
// 读目录失败时返回 nil 并记一条日志：这里没有 err 出口（调用方签名是
// `func() []*codearts.Auth`），需要错误信息的调用方走 list / Resolve。
func (s *codeartsCredStore) List() []*codearts.Auth {
	list, err := s.list()
	if err != nil {
		log.Printf("codearts: 读取凭证目录失败: %v", err)
		return nil
	}
	return list
}

// UIDs 返回当前目录里的 uid 列表（顺序与 List 一致）。
//
// 先 List 再投影：这样管理端点看到的账号集合与池子/后台任务**同源**，
// 不会出现"管理端点里有一个池子不认识的号"。
func (s *codeartsCredStore) UIDs() []string {
	list, err := s.list()
	if err != nil {
		log.Printf("codearts: 读取凭证目录失败: %v", err)
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.UID)
	}
	return out
}

// Resolve 按 uid **或 AK** 找账号（改造前的 Resolve 就同时认这两个）。
//
// 返回的是 store 持有的那个指针：管理端点在它上面看到/改到的东西，
// 与请求路径真正拿去发请求的是同一份，不会出现"界面上改了但发请求的还是旧的"。
//
// 错误语义：目录读不了 → 把该错误原样返回（与改造前一致，不是"账号不存在"）；
// 目录读到了但没有这个 uid → 账号不存在。
func (s *codeartsCredStore) Resolve(uid string) (*codearts.Auth, error) {
	list, err := s.list()
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if a.UID == uid || a.AccessKey == uid {
			return a, nil
		}
	}
	return nil, fmt.Errorf("codearts: 账号不存在: %s", uid)
}

// list 是**唯一**重新枚举磁盘并做 uid 合并的地方。返回副本切片。
//
// # 合并规则（按设计，不要另造判据）
//
//   - 胜出者的判据复用 codeartsWinners（即 betterCodeartsCred，与并池同一套）——
//     判据若有两份实现，store 的对象与池 secret 迟早分叉。
//   - uid 首次出现          → 用解析结果建新对象
//   - uid 已存在、**同路径** → 通常保留原指针（内存权威）；仅当磁盘那份
//     **不比内存旧**且字段确有差异时才原地采用（外部重新登录 / 手工修好）
//   - uid 已存在、不同路径  → 取 LockRefresh() 原地写字段（指针不变）
//   - 磁盘上消失的 uid      → 从 store 移除
//
// # ⚠ 为什么"同路径默认不回抄"是必须的（这条最容易被"顺手优化"掉）
//
// 那个文件是对象**自己**（或它的续期）写出去的镜像。续期是"先改内存、
// 再原子落盘"，两个时刻之间存在窗口；即磁盘内容**恰好落后于内存**。
// 若 List 在这里无条件回抄磁盘，就等于**用旧值覆盖刚用一次性 token 换回来的新凭证**：
//
//	旧 refresh_token 已作废、新 refresh_token 被覆盖丢失 → 该账号永久报废，只能重新登录。
//
// 所以"磁盘不比内存旧"是采用的**前置条件**，而不是"来自另一个路径"。
// （原先的判据是"不同路径才采用"，但同 uid 必然同路径 —— 见下面的分支注释。）
func (s *codeartsCredStore) list() ([]*codearts.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 空目录守卫。`codearts.LoadDir("")` 内部是
	// filepath.Glob(filepath.Join("", "codearts*.json")) == glob("codearts*.json")，
	// 而那是**相对进程 CWD** 的匹配 —— 会把当前目录下任意 codearts*.json 当凭证读进来。
	//
	// 生产装配不会传空（config.go 保证至少是 "<auth_dir>/codearts"），
	// 但"注释宣称的守卫其实不存在 + 静默回落"正是最难查的一类缺陷，所以显式挡住。
	if s.dir == "" {
		return nil, nil
	}

	raw, err := s.load(s.dir)
	if err != nil {
		return nil, err
	}
	// 先按 uid 裁决胜出者（同一 uid 可能有多份文件，判据与并池完全一致）。
	// picked 与 order 一一对应：order[i] 是 picked[i] 的 uid。
	picked, order := codeartsWinners(raw)

	out := make([]*codearts.Auth, 0, len(order))
	seen := make(map[string]bool, len(order))
	for i, uid := range order {
		w := picked[i]
		seen[uid] = true

		cur, exists := s.byUID[uid]
		if !exists {
			s.byUID[uid] = w
			out = append(out, w)
			continue
		}
		if cur.FilePath == w.FilePath {
			// 同一个文件：**通常**是它自己的镜像 → 内存权威。
			//
			// # 为什么不能无条件"一个字都不抄"（评审 F2）
			//
			// cmd/login 的文件名规则是 `codearts-<uid>.json`（见 login.go），
			// 于是**同 uid 必然同路径** —— "重新登录同一个账号"与"用户手工修好
			// 这份文件"都落在这一条分支上。无条件不抄 = 这两种情况永远不生效，
			// 账号会一直拿着已作废的凭证，只能靠重启网关恢复。
			//
			// # 判据：磁盘那份**不比内存旧**才采用
			//
			//   · 自己写的镜像（正常）：SaveAtomic 成功后磁盘 == 内存 → 采用是 no-op
			//   · 自己写的镜像（落盘失败）：磁盘是**更旧**的已作废凭证 → **不采用**
			//     （这条最关键：回抄它等于把刚用一次性 token 换来的新凭证丢掉，
			//      该账号永久报废，只能重新走浏览器登录）
			//   · 别人重写的（重新登录 / 手工修）：新凭证必然更晚过期 → 采用
			// ⚠ 读-判-写必须在**同一个 refreshMu 临界区**里（对抗评审 R1）。
			//
			// 原先这三步只在 s.mu 下进行，而写这些字段的是
			// client.RefreshToken 与 client.adoptDiskRefreshToken —— 它们持的是
			// **Auth.refreshMu / mu**，与 s.mu 没有任何同步关系。无同步读 = data race，
			// 而且有具体交错能让账号**永久报废**：
			//
			//	1. 本函数 LoadDir 读到旧盘值 w（RT_old / E_old）
			//	2. 此刻 RefreshToken 正在同一对象上跑：内存已写新值，尚未 SaveAtomic
			//	3. 无锁读 cur.ExpiresAt 允许返回尚未发布的 E_old
			//	   → `w.ExpiresAt >= cur.ExpiresAt` 成立；caCredFieldsDiffer 又读到
			//	     新 RT → 判"有差异"
			//	4. 进入原地写时**才**取 refreshMu（阻塞到续期落盘完成）
			//	5. → 用第 1 步那份**旧盘值**覆盖刚用一次性 token 换回的新凭证
			//
			// 第 5 步正是本文件开头声称要防住的那件事（"只能靠重启恢复"）。
			// 触发面：后台任务默认每 60s 一次 store.List()，请求路径随时会续期。
			//
			// 这里用 LockRefresh 而不是 mu：写字段的两条路径**都**持 refreshMu
			// （一条自己持、一条由 RefreshToken 持），所以 refreshMu 是覆盖全部
			// 写者的那一把。加锁顺序 refreshMu → mu 与 SaveAtomic 一致，无反向加锁。
			cur.LockRefresh()
			changed := w.ExpiresAt >= cur.ExpiresAt && caCredFieldsDiffer(cur, w)
			if changed {
				adoptCodeartsCredLocked(cur, w)
			}
			cur.UnlockRefresh()
			if changed {
				log.Printf("codearts: 账号 %s 的凭证文件被外部改写，已在原对象上更新字段（指针不变）", shortUID(uid))
			}
			out = append(out, cur)
			continue
		}
		// 不同文件：磁盘上换了一份（例如用户重新登录多出一份、旧的还没删）。
		// 原地更新字段，指针不变 —— 池 secret / 后台任务 / 管理端点共享的
		// 就是这一个指针，换成新对象会让它们全部变成孤儿。
		adoptCodeartsCredInPlace(cur, w)
		log.Printf("codearts: 账号 %s 的凭证来源文件已变更，已在原对象上更新字段（指针不变）", shortUID(uid))
		out = append(out, cur)
	}

	// 磁盘上消失的 uid：从 store 移除（下一次出现时是全新对象）。
	// 池子的对齐由调用方负责（syncCodeartsAccounts 会用本次列表做 upsert+剔除）。
	for uid := range s.byUID {
		if !seen[uid] {
			delete(s.byUID, uid)
		}
	}
	return out, nil
}

// adoptCodeartsCredInPlace 取 refreshMu 后把 src 的凭证字段**原地**写进 dst。
//
// 这是给**没有自己持锁**的调用方用的入口。调用方若已经持有 dst 的 refreshMu
// （例如 store.list() 把"读-判-写"整段放进同一个临界区），必须改用
// adoptCodeartsCredLocked —— 本函数会再取一次，而 refreshMu 不可重入 → 自死锁。
func adoptCodeartsCredInPlace(dst, src *codearts.Auth) {
	dst.LockRefresh()
	defer dst.UnlockRefresh()
	adoptCodeartsCredLocked(dst, src)
}

// adoptCodeartsCredLocked 原地改写凭证字段，**要求调用方已持有 dst.refreshMu**。
//
// # 为什么是 refreshMu（而不是 mu）
//
// 续期全过程（client.RefreshToken：从读 refresh_token 到请求、写字段、落盘）
// 都在**同一把 refreshMu** 里，所以拿它就与续期互斥：
//
//	续期在途   → 这里等它写完，随后本次写入的是"另一个路径"的凭证，语义正确
//	续期已结束 → 不存在半更新的中间态
//
// 注意 refreshMu 也覆盖了 adoptDiskRefreshToken（它只在 RefreshToken 内部被调用），
// 所以它是**唯一**能覆盖全部凭证字段写者的锁。
//
// # 为什么里面还要取 mu
//
// 只持 refreshMu 能挡住"另一个写者"，但挡不住**只读**的调用方（签名、Cred() 等
// 走的是 mu）。写字段时一并取 mu，读者才不会看到半更新的中间态。
// 顺序是 refreshMu → mu，与 SaveAtomic 一致；反过来（先 mu 再 refreshMu）
// 会与 SaveAtomic 构成反向加锁 → 可能死锁，所以这里绝不能调换。
//
// # 为什么字段集比设计里列的六个多一些
//
// 设计列出的是「凭证字段」的最小集（AK/SK/ST/ExpiresAt/RefreshToken/FilePath）。
// 这里把 DPoP 私钥、ClientID、Nickname 一并写上，因为它们是**同一份凭证**的
// 组成部分：只写一半会让对象处于"新 AK + 旧 DPoP 私钥"的状态，
// 而 DPoP 私钥与 refresh_token 是**配对**的 —— 续期会因签名/绑定不匹配而失败。
// UID 不写：它就是这个对象的键，按定义相等。
func adoptCodeartsCredLocked(dst, src *codearts.Auth) {
	dst.Lock()
	defer dst.Unlock()

	dst.AccessKey = src.AccessKey
	dst.SecretKey = src.SecretKey
	dst.SecurityToken = src.SecurityToken
	dst.ExpiresAt = src.ExpiresAt
	dst.RefreshToken = src.RefreshToken
	dst.FilePath = src.FilePath

	// 钥匙只在**带了一把可用的**时候才写：磁盘上丢了 dpop 段（或写成 null）时
	// 不得清空内存里那把 —— 清空会让对象连发请求的资格都没有，
	// 而"磁盘没钥匙"本身不是能被修好的差异。
	if codearts.HasUsableDPoPKey(src.DPoPPrivateKeyJWK) {
		dst.DPoPPrivateKeyJWK = src.DPoPPrivateKeyJWK
	}
	dst.ClientID = src.ClientID
	dst.Nickname = src.Nickname
}

// caCredFieldsDiffer 报告两份凭证的**凭证材料**是否不同。
//
// 只比"会影响能不能发出请求"的字段，**不比** Nickname：
// 昵称变了不代表凭证要重装。
//
// 用途是**避免无谓的原地改写**：镜像文件在绝大多数 List 调用里与内存完全一致，
// 那时不必写字段、也不必打日志（否则每 60 秒的后台扫描都会刷一行）。
//
// # 为什么必须比 DPoP 私钥与 client_id（评审发现的不对称）
//
// 判据的字段集必须与 adoptCodeartsCredInPlace 的**写入**字段集一致。
// 原先这里只比 AK/SK/ST/ExpiresAt/RT，而写入方却连 DPoP 私钥一起写 ——
// 检测能力小于修复能力，就会漏掉一整类"只有钥匙变了"的凭证。
//
// 这个不对称是有真实后果的（不是洁癖）：client.adoptDiskRefreshToken 曾
// 只把 RT/AK/SK/ST/ExpiresAt 抄进内存，**落下钥匙**。那一半采纳之后，
// 本函数会因为"其余字段全部相等"而judged false → adoptCodeartsCredInPlace
// 再也不跑 → 对象永远停在"新 token + 旧钥匙"上，每次续期都因绑定不匹配失败，
// **只能重启进程恢复**。而这正是本次交付要根除的那类故障。
//
// 所以两侧现在**同进同退**：检测什么，就必须能修好什么。
func caCredFieldsDiffer(a, b *codearts.Auth) bool {
	return a.AccessKey != b.AccessKey ||
		a.SecretKey != b.SecretKey ||
		a.SecurityToken != b.SecurityToken ||
		a.ExpiresAt != b.ExpiresAt ||
		a.RefreshToken != b.RefreshToken ||
		a.ClientID != b.ClientID ||
		// DPoP 私钥走 codearts 包里的统一口径：nil / 空白 / JSON null 一律视为
		// "没有钥匙"，互相等价 —— 否则"内存有钥匙、磁盘丢了"会被每 60 秒判成
		// 一次差异，刷日志却修不好任何东西（我们没法凭空造出钥匙）。
		codearts.DPoPKeysDiffer(a.DPoPPrivateKeyJWK, b.DPoPPrivateKeyJWK)
}

// wireCodeartsCreds 把 store 接到 Provider 的**三条读路径**上。
//
// # 为什么把它抽成一个函数（评审 F1）
//
// 007 的根因就在装配层这一句：`cb.SetAccounts` 原先用的是一个"每次 LoadDir
// 造一批新对象"的闭包。而 007 的测试全都自己 `newCodeartsCredStore` 再调
// `syncCodeartsAccounts` —— **从没经过 main.go 这段装配**。
// 实测：把这里退回旧闭包，`go build` + `go test ./cmd/server/` 全绿。
// 也就是说那 4 条断言守的是 store/sync 这一对函数，不是"生产装配线"。
//
// 抽成函数之后，测试可以调用**同一段代码**（见 TestBackgroundRefreshLandsOnPoolObject），
// 把不变量钉在真正会出问题的那个位置。
//
// 四条读路径必须同源，缺一条就会出事：
//
//	SetAccounts —— 后台续期任务（jobs.go → localAccounts）
//	AdminEnv.Accounts / Resolve —— 管理端点
//	池 secret —— 由 syncCodeartsAccounts 用同一个 store 装载
//	「重载 auths」—— admin 走 gateway.CredentialSecretLoader → 本上游的
//	    LoadCredentialsWithSecrets，而它优先取 AdminEnv.Accounts（= 本 store）
//
// ⚠ 第四条缺了的后果**不是**"造出第二个对象"，而是更隐蔽的"一个对象都没有"：
// 池里多出该 uid 却没有 secret，被选中时必然失败；而按 store 同步的路径
// 只在启动时跑一次 → 用户得重启网关。这正是评审 R2 修的缺口。
func wireCodeartsCreds(cb *codearts.Provider, dir string, hist *checkinlog.Log) *codeartsCredStore {
	creds := newCodeartsCredStore(dir)
	cb.SetAccounts(creds.List)
	cb.SetAdminEnv(codearts.AdminEnv{
		Accounts: creds.UIDs,
		Resolve:  creds.Resolve,
		// 福利领取的历史记录（账号池的「福利」列据此回答"今天领过没有"）。
		// 与 workbuddy 拿的是**同一个** checkinLog 实例 —— 两个上游的任务历史
		// 必须落在同一份日志里，否则「任务历史」面板只看得到一半。
		Log: hist,
	})
	return creds
}

// syncCodeartsAccounts 把 codearts 凭证目录里的账号并入核心账号池。
//
// # 为什么需要 secret 通道
//
// 池子存的是 *auth.Auth（workbuddy 的凭证类型）—— 那是核心唯一认识的凭证。
// codearts 的凭证是 *codearts.Auth（AK/SK/DPoP 私钥），核心**不得**认识它
// （判据 3：pool 不得依赖任何上游包）。
//
// 于是走 pool 提供的**不透明 any 通道**：
//
//	池子保管：SyncToDirWithSecrets 的 secrets 参数
//	取回使用：pool.SecretOf(uid) → 调用方断言回 *codearts.Auth
//
// 池子全程只搬不读，因此它对 codearts 一无所知，耦合为零。
//
// # ⚠ 为什么必须从 store 取对象（007 修的就是这里）
//
// 改造前这里自己 `codearts.LoadDir(authDir)` 一次，于是池 secret 是
// **另一批**对象（与后台任务 LoadDir 出来的那批不同）。凭证目录里一份
// refresh_token 对应进程里 N 个对象 → 对象级 refreshMu 形同虚设 →
// 续期写不回池子 → 503 no_healthy_account。
//
// 现在唯一的来源是 store：放进 secrets 的指针**就是**后台任务拿到并原地
// 续期的那一个，续期结果因此立刻对请求路径可见。
//
// # 投影规则
//
// 核心只需要 uid（主键）与 nickname（展示），其余字段一律留在 secret 里。
// 这就是"核心不解释上游凭证"的具体体现。
//
// 返回值是**实际生效的账号数**（按 uid 去重后）。
func syncCodeartsAccounts(p *pool.Pool, store *codeartsCredStore) int {
	if store == nil {
		// 装配层缺陷（cb 非 nil 却没有 store）：宁可不动池子，也不要 panic。
		log.Printf("codearts: 凭证 store 未装配，账号池未并入")
		return 0
	}
	list, err := store.list()
	if err != nil {
		log.Printf("codearts: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		// 目录里没有可用凭证不是错误：用户可能刚起网关、还没跑 cmd/login。
		// 这里仍然对齐一次（把已删除的账号剔掉），但不报错、不刷日志。
		p.SyncToDirWithSecrets(codearts.ProviderID, nil, nil)
		return 0
	}

	auths, secrets := dedupeCodeartsByUID(list)
	p.SyncToDirWithSecrets(codearts.ProviderID, auths, secrets)
	return len(auths)
}
