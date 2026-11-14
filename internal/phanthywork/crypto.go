// crypto.go PhanthyWork 的**纯函数层**：三条 canonical 的拼装、sha256/base64url、
// ed25519 密钥的生成与加载、installationId 的派生、时钟偏移的规整。
//
// # 为什么单独一个文件（这条是报告 §7.2 明确推崇的分层）
//
//	「proof 的纯函数分层 —— 三条 canonical + sha256 + ed25519 原语，
//	  **零 IO、不发请求、不认识端点**。换厂商时只改 canonical 拼装。」
//
// 本文件的硬约束（有测试钉住）：
//
//	不 import net/http、不 import os、不做任何网络与磁盘操作
//	不认识任何端点路径 —— 路径是**调用方传进来的字符串**
//	不使用时间以外的全局状态
//
// 这样做的好处是"签名算错"这件事可以被**离线**逐字节断言：
// 段序、分隔符、哪一段是哪把公钥，全部能在单测里用已知向量钉死，
// 不需要连上游（那正是本仓"契约测试不碰网络"的同一条要求）。
//
// # 三条 canonical 的段序（逐字来自报告 §2.4 / §2.5 / §2.6 与附录 C.1）
//
//	device     7 段：desktop-device-v1 / accountScope / installationId /
//	                  identityPub / inferencePub / ts / nonce
//	v1         7 段：v1 / method(**原值**) / path / ts / nonce / sha256(body) / idempotencyKey
//	inference 12 段：desktop-inference-v2 / METHOD(**大写**) / path / ts / nonce /
//	                  sha256(body) / sha256(Authorization) / sha256(推理令牌) /
//	                  requestId / sessionId / expertId / skillNames
//
// ⚠ 三条 canonical 的**分隔符不是同一个**（2026-11-14 读官方源码才发现）：
//
//	device      → 单个空格 0x20
//	v1          → 单个空格 0x20
//	inference   → LF 0x0A
//
// 证据（两处来源不同，互相独立）：
//   - Tauri 前端 bundle：`_up_/dist/assets/App-dSHMvpu3.js` @111223 附近
//     [`desktop-device-v1`, scope, instId, pub, inferPub, t, n].join(` `)
//     [`v1`, method, upstreamPath, t, n, sha256(body), idem].join(` `)
//     两处都是 `.join(\` \`)`。
//   - claude-sidecar.exe @99904757 的 jO8()：[...].join(`\n`)
//     （ASCII 字节 5D 2E 6A 6F 69 6E 28 60 0A 60 29）。
//
// 早期文档把三条写成同一个分隔符，据此实现必失败：device/v1 用 LF 会
// `invalid_desktop_signature`，inference 用空格会 500 空体。
// 这里保留两个具名常量 + 逐字节断言测试，禁止再合并成一个。
package phanthywork

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// ── 常量（协议事实，全部来自报告）────────────────────────────────────────

const (
	// deviceV1Separator device / v1 两条 canonical 的段分隔符 = **单个空格**。
	//
	// ⚠ 不是 LF、不是冒号、不是竖线。证据见包注释（前端 bundle 的 .join(` `)）。
	deviceV1Separator = " "

	// inferenceSeparator inference canonical 的段分隔符 = **LF（0x0A）**。
	//
	// ⚠ 与上面那个**不同**。证据见包注释（sidecar 的 .join(`\n`)，
	//   二进制里是 5D 2E 6A 6F 69 6E 28 60 0A 60 29）。
	inferenceSeparator = "\n"

	// deviceCanonicalPrefix device canonical 的版本域。
	deviceCanonicalPrefix = "desktop-device-v1"

	// v1CanonicalPrefix v1 canonical 的版本域。
	v1CanonicalPrefix = "v1"

	// inferenceCanonicalPrefix inference canonical 的版本域（**v2**）。
	inferenceCanonicalPrefix = "desktop-inference-v2"

	// ed25519SPKIHeaderHex ed25519 公钥 SPKI/DER 的 12 字节固定头。
	//
	// 实测：DER 共 44 字节 = 12 字节头 + 32 字节裸公钥；公钥串 base64url 后 59 字符。
	ed25519SPKIHeaderHex = "302a300506032b6570032100"

	// rawPublicKeyLen 裸公钥长度（32 字节）。
	rawPublicKeyLen = 32

	// ed25519SeedLen ed25519 私钥种子长度（32 字节）。
	ed25519SeedLen = 32

	// installationIDPrefix installationId 的前缀（**不是** di:、不是空串）。
	installationIDPrefix = "di_"

	// deviceV1NonceLen device / v1 的 nonce 字节数 = **18**。
	//
	// 证据：前端 `Ao()` = `Oo(crypto.getRandomValues(new Uint8Array(18)))`
	//   → base64url 24 字符。device 绑定与 v1 请求头用的是同一个 `n`。
	deviceV1NonceLen = 18

	// inferenceNonceLen inference 的 nonce 字节数 = **24**。
	//
	// 证据：sidecar 的 jO8() 里 `Vd3(24).toString("base64url")` → 32 字符。
	//   ⚠ 与 device/v1 的 18 **不同**，别混。
	inferenceNonceLen = 24
)

// ed25519SPKIHeader 12 字节头（解析一次，避免每处都 hex 解码）。
var ed25519SPKIHeader = func() []byte {
	b, err := hex.DecodeString(ed25519SPKIHeaderHex)
	if err != nil {
		panic("phanthywork: SPKI 头常量写错了: " + err.Error())
	}
	return b
}()

// ── base64url / sha256 / 随机 ─────────────────────────────────────────────

// b64url 无 padding 的 base64url 编码（协议里所有二进制字段都用它）。
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// b64urlDecode 无 padding 的 base64url 解码，容忍带 padding 的写法。
func b64urlDecode(s string) ([]byte, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, errEmptyB64
	}
	if b, err := base64.RawURLEncoding.DecodeString(t); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(t)
}

// sha256B64URL sha256 摘要的 base64url（无 padding）。
//
// ⚠ 输入必须是**最终发出的原始字节**：请求体那条不能拿"重新序列化"的结果
// 去算（坑⑧）—— 见 RequestBodyForHash 的注释。
func sha256B64URL(b []byte) string {
	sum := sha256.Sum256(b)
	return b64url(sum[:])
}

// newNonceN 生成 n 字节随机 nonce 的 base64url。
//
// 随机源出错（不可恢复）时返回空串而不是 panic：nonce 为空会让服务端
// 拒绝这次请求（可观测），而 panic 会带崩整个出站循环（不可观测）。
func newNonceN(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return b64url(buf)
}

// newDeviceNonce 生成 device / v1 用的 nonce（**18 字节** → 24 字符）。
//
// 两条串共用同一个 nonce（前端也是同一个 `n` 同时喂给 device 与 v1）。
func newDeviceNonce() string { return newNonceN(deviceV1NonceLen) }

// newInferenceNonce 生成 inference 用的 nonce（**24 字节** → 32 字符）。
func newInferenceNonce() string { return newNonceN(inferenceNonceLen) }

// newNonce 24 字节的通用随机串。
//
// ⚠ 它**只**给与协议 nonce 无关的本地用途（PKCE verifier / OAuth state）。
// 出站请求的 nonce 一律用 newDeviceNonce / newInferenceNonce —— 那两处
// 的长度是协议事实（18 / 24），不是随便挑的。
func newNonce() string { return newNonceN(inferenceNonceLen) }

// ── ed25519 ──────────────────────────────────────────────────────────────

// generateEd25519 造一对新密钥，返回 (裸种子 base64url, SPKI 公钥 base64url)。
//
// 公钥用 SPKI/DER 编码（44 字节）后 base64url —— 与上游认的形态一致
// （报告 §2.2「公钥格式 SPKI/DER/base64url 无 padding」）。
//
// 私钥**只留 32 字节种子**（PKCS#8 的中间那段），不落 PKCS#8 外壳：
// 本包只需要用它重建 ed25519.PrivateKey（ed25519.NewKeyFromSeed），
// 多存一层外壳只是多一个会写错的地方。
func generateEd25519() (seedB64, pubB64 string, err error) {
	pub, priv, gerr := ed25519.GenerateKey(rand.Reader)
	if gerr != nil {
		return "", "", gerr
	}
	return b64url(priv.Seed()), b64url(marshalSPKI(pub)), nil
}

// marshalSPKI 把裸 ed25519 公钥编成 SPKI/DER（12 字节固定头 + 32 字节裸公钥）。
func marshalSPKI(pub ed25519.PublicKey) []byte {
	out := make([]byte, 0, len(ed25519SPKIHeader)+len(pub))
	out = append(out, ed25519SPKIHeader...)
	out = append(out, pub...)
	return out
}

// keyPairFromSeed 由裸种子重建密钥对（公钥与私钥可用）。
func keyPairFromSeed(seedB64 string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	seed, err := b64urlDecode(seedB64)
	if err != nil {
		return nil, nil, err
	}
	if len(seed) != ed25519SeedLen {
		return nil, nil, errBadSeedLen
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return pub, priv, nil
}

// publicKeyB64FromSeed 由种子算出其 SPKI/base64url 公钥。
func publicKeyB64FromSeed(seedB64 string) (string, error) {
	pub, _, err := keyPairFromSeed(seedB64)
	if err != nil {
		return "", err
	}
	return b64url(marshalSPKI(pub)), nil
}

// publicKeyB64ToDER 把 SPKI/base64url 公钥解回 DER 原始字节，并校验它是 ed25519。
func publicKeyB64ToDER(pubB64 string) ([]byte, error) {
	der, err := b64urlDecode(pubB64)
	if err != nil {
		return nil, err
	}
	if len(der) != len(ed25519SPKIHeader)+rawPublicKeyLen {
		return nil, errBadPublicKey
	}
	for i, b := range ed25519SPKIHeader {
		if der[i] != b {
			return nil, errBadPublicKey
		}
	}
	return der, nil
}

// signEd25519 用裸种子对 data 做 ed25519 签名，返回 base64url。
//
// ⚠ 算法参数是 ed25519 本身（源码里的 sign(null, …)）——
// 报告 §2.2：「ed25519 的算法参数必须是 null」，即"不做预哈希"。
func signEd25519(seedB64 string, data []byte) (string, error) {
	_, priv, err := keyPairFromSeed(seedB64)
	if err != nil {
		return "", err
	}
	return b64url(ed25519.Sign(priv, data)), nil
}

// verifyEd25519 校验签名（**只给测试用**，生产路径不发验签）。
//
// 放在这里而不是测试文件里，是为了让"canonical 段序对了"这件事
// 能用一条离线用例自证（签名 → 验签 → 篡改一段 → 验签必须失败）。
func verifyEd25519(pubB64 string, data []byte, sigB64 string) bool {
	der, err := publicKeyB64ToDER(pubB64)
	if err != nil {
		return false
	}
	sig, err := b64urlDecode(sigB64)
	if err != nil {
		return false
	}
	raw := der[len(ed25519SPKIHeader):]
	if len(raw) != rawPublicKeyLen {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(raw), data, sig)
}

// deriveDeviceSeed 由 identity 种子**确定性**派生 device 私钥的种子。
//
// # 为什么要"派生"而不是"存第二把种子"
//
// 三把密钥里 identity 与 device 都**跨重启不变**（报告 §2.2 的生命周期表），
// 而 device 私钥只在"设备注册"那一次用得上。既然它必须是不变的，
// 那"不变的来源"就已经是 identity 种子 —— 再存一份只是多一处会写错、
// 会分叉、会被两个写者各自覆盖的状态（本仓反复吃过这一类）。
//
// 关键事实（报告 §2.2 实测钉死的 IDENTITY_POLICY）：
//
//	deviceKeyDistinct: true  —— device 公钥**必须与** identity 公钥不同
//	headerSigner:      identity（v1 串用 identity 私钥）
//	bindingSigner:     device  （device 串用 device 私钥）
//
// 所以派生必须**换域**（加一个用途前缀），否则两把公钥会相等
// —— 而失败表现是 desktop_binding_invalid，一个看不出原因的错误。
func deriveDeviceSeed(identitySeedB64 string) (string, error) {
	seed, err := b64urlDecode(identitySeedB64)
	if err != nil {
		return "", err
	}
	if len(seed) != ed25519SeedLen {
		return "", errBadSeedLen
	}
	h := sha256.New()
	h.Write([]byte("phanthywork/device-key/v1"))
	h.Write([]byte{0})
	h.Write(seed)
	return b64url(h.Sum(nil)), nil
}

// DeriveInstallationID 由 identity 公钥（SPKI/base64url）派生 installationId。
//
// 唯一算法（报告 §2.3 附录 C.2，逐字）：
//
//	installationId = "di_" + base64url( sha256( SPKI_DER[12:] ) )
//
// 即：丢掉 12 字节 SPKI 头，对剩下的**裸 32 字节**做 sha256。
//
// 三条被实测否证的错写法（每条都回 409 desktop_installation_conflict）：
//
//	① 哈希 accountScope（账号 uuid）—— 与公钥无关
//	② 哈希整段 SPKI（44 字节）—— 官方哈希的是**裸公钥**
//	③ 不加前缀 / 用 "di:" —— 前缀就是 "di_"
//
// 导出是因为装配层与测试都需要它（"同一个公钥必然同一个 id"是运维事实）。
func DeriveInstallationID(identityPublicKeyB64 string) (string, error) {
	der, err := publicKeyB64ToDER(identityPublicKeyB64)
	if err != nil {
		return "", err
	}
	raw := der[len(ed25519SPKIHeader):]
	return installationIDPrefix + sha256B64URL(raw), nil
}

// ── 三条 canonical 的拼装 ────────────────────────────────────────────────

// DeviceCanonical 拼 device canonical（7 段，**单空格**连接）。
//
//	desktop-device-v1
//	<accountScope>        账号 uuid（GET /api/oauth/profile 的 account.uuid 原字符串）
//	<installationId>      di_…
//	<identityPublicKey>   identity 公钥
//	<sessionPublicKey>    ⚠ 第 5 段是**推理公钥**（坑⑩），不是 identity 再写一遍
//	<timestamp>           毫秒数字的字符串
//	<nonce>               18 字节 → 24 字符
func DeviceCanonical(accountScope, installationID, identityPublicKey, inferencePublicKey, timestamp, nonce string) string {
	return strings.Join([]string{
		deviceCanonicalPrefix,
		accountScope,
		installationID,
		identityPublicKey,
		inferencePublicKey,
		timestamp,
		nonce,
	}, deviceV1Separator)
}

// V1Canonical 拼 v1 canonical（7 段，**单空格**连接）—— 请求头 x-desktop-signature 用它。
//
//	v1
//	<method>            ⚠ **保持原值**，不做 toUpperCase（坑⑫）
//	<upstreamPath>
//	<timestamp>
//	<nonce>             ⚠ 18 字节 → 24 字符（与 device 串共用同一个 nonce）
//	<sha256(body) base64url>
//	<idempotencyKey>    ⚠ 没有幂等键时是**空串**，但**分隔符不能省**（坑⑪）
func V1Canonical(method, upstreamPath, timestamp, nonce string, body []byte, idempotencyKey string) string {
	return strings.Join([]string{
		v1CanonicalPrefix,
		method,
		upstreamPath,
		timestamp,
		nonce,
		sha256B64URL(body),
		idempotencyKey,
	}, deviceV1Separator)
}

// InferenceCanonical 拼 inference canonical（12 段，**LF** 连接）。
//
//	desktop-inference-v2
//	<METHOD 大写>            ⚠ 这条串里**要** toUpperCase（坑⑫）
//	<pathname>
//	<timestamp>
//	<nonce>                  ⚠ 24 字节 → 32 字符（**不是** device/v1 那个 18）
//	<sha256(body)>
//	<sha256(Authorization)>  ⚠ 含 "Bearer " 前缀的**完整值**
//	<sha256(推理令牌原值)>
//	<x-request-id 或 "">
//	<x-claude-code-session-id 或 "">
//	<x-phanthy-expert-id 或 "">
//	<x-phanthy-skill-names 或 "">
//
// 四条规则（缺一条签名就废，报告 §2.6）：
//
//	1 分隔符 **LF**（与 device/v1 的空格不同，见包注释）
//	2 第 6 段哈希的是**最终发出的 body 原始字节**
//	3 后 4 段缺项写**空串**，但分隔符不能省 —— 少一段就是另一个串
//	4 method 在这条串里**大写**（与 v1 串相反）
//
// ⚠ 第 10 段（x-claude-code-session-id）在**线上路径上不许为空**：
// 缺它上游回 500 空体（kong upstream-latency 9~10ms）。本函数的空串能力
// 只用于单测构造，线上由 buildProof 传入真实 session id。
func InferenceCanonical(method, pathname, timestamp, nonce string, body []byte, authorization, inferenceToken, requestID, sessionID, expertID, skillNames string) string {
	return strings.Join([]string{
		inferenceCanonicalPrefix,
		strings.ToUpper(method),
		pathname,
		timestamp,
		nonce,
		sha256B64URL(body),
		sha256B64URL([]byte(authorization)),
		sha256B64URL([]byte(inferenceToken)),
		requestID,
		sessionID,
		expertID,
		skillNames,
	}, inferenceSeparator)
}

// AuthorizationHeaderValue 拼 Authorization 头的**完整值**（含 "Bearer " 前缀）。
//
// ⚠ inference canonical 第 7 段哈希的是这个整串，不是裸 token ——
// 拿裸 token 去哈希会得到一个永远对不上的签名。
func AuthorizationHeaderValue(accessToken string) string {
	return "Bearer " + accessToken
}

// RequestBodyForHash 返回应当参与签名的请求体字节（**最终发出的那一份**）。
//
// # 为什么单独一个函数（坑⑧）
//
// canonical 第 6 段哈希的是**最终发出的 body 原始字节**。任何
// "顺手再 JSON.stringify 一下"都会破坏它 —— 序列化两次得到的字节
// 可能不同（键序、空格、转义），而服务端只认它自己收到的那一份。
//
// 所以本包全程只做**一次**序列化：PrepareBody 的产物同时用于
// ① 算签名 ② 原样发出。这个函数存在的意义是把这个约定写成代码，
// 而不是散在调用点上的注释（调用点上"少写一层包装"看不出来）。
func RequestBodyForHash(finalBody []byte) []byte { return finalBody }

// ── 时钟 ─────────────────────────────────────────────────────────────────

// NormalizeClockSkewMS 把时钟偏移规整到 (-1000, 1000] 毫秒区间。
//
// # 为什么必须规整（这是一条实测踩出来的）
//
// 同一个进程里两处报出的偏移差了约 1000ms：auth 报告 §8 记的是
// clockSkewMs: -66996（来自 /api/daily-activity/session），
// 2api 报告 §3① 实测的是 **-68,047 ms**（来自 activities/summary 的
// server_time）。报告说"不是矛盾，是本机时钟一直在漂"。
//
// ⇒ 偏移的**单位口径**必须在本包内定死为毫秒，并对"多了整秒"的来源做规整：
// 偏移落在 (-1000, 1000] 之外就取它对 1000 的余数（而不是原样累加一个
// -66,996 的错偏移 —— 那会让签名时间戳直接偏出服务端窗口，表现成
// 403 request_not_authorized，而文案完全指不到时钟）。
//
// 规整是**幂等**的：反复调用结果不变（有测试钉住）。
func NormalizeClockSkewMS(skewMS int64) int64 {
	if skewMS > -maxPlausibleSkewMS && skewMS < maxPlausibleSkewMS {
		return skewMS
	}
	// 明显超出「任何真实时钟偏移」的量级（> 1 小时）：
	// 只可能是单位/来源错了，取它对 1000 的余数把它拉回亚秒区间，
	// 而不是原样累加一个十八小时的偏移（那会让签名时间戳彻底偏出服务端窗口）。
	return skewMS % 1000
}

// maxPlausibleSkewMS 仍然算「真实时钟偏移」的上界（1 小时）。
//
// ⚠ 这个上界**不是容差**：报告明确说服务端的时间窗容差「未知」，
// 所以本包不猜容差，只判「这个数是时钟偏移还是单位错」。
// 实测过的两个真实偏移（-68,047ms 与 -66,996ms）都远在上界之内 ⇒ 原样保留。
const maxPlausibleSkewMS int64 = 60 * 60 * 1000

// serverNowMS 由本机时间与偏移算出**服务端口径**的毫秒时间戳。
//
// skewMS 的口径是 本机 - 服务端（实测 -68,047 = 本机**慢** 68 秒），
// 所以服务端时间 = 本机时间 - skew。
func serverNowMS(localMS, skewMS int64) int64 { return localMS - NormalizeClockSkewMS(skewMS) }

// msToString 毫秒时间戳的字符串形态（canonical 里就是这个）。
func msToString(ms int64) string { return strconv.FormatInt(ms, 10) }

// nowMS 本机毫秒时间（抽成变量便于测试注入固定时钟）。
var nowMS = func() int64 { return time.Now().UnixMilli() }

// ── 形态判定小工具 ───────────────────────────────────────────────────────

// isInstallationID 判断串是否符合 di_… 形态（含长度校验）。
func isInstallationID(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, installationIDPrefix) {
		return false
	}
	body := s[len(installationIDPrefix):]
	if len(body) != 43 { // 32 字节 → base64url 无 padding = 43 字符
		return false
	}
	_, err := b64urlDecode(body)
	return err == nil
}

// isPublicKeyB64 判断串是否是本包认的 ed25519 SPKI/base64url 公钥（44 字节）。
func isPublicKeyB64(s string) bool {
	_, err := publicKeyB64ToDER(strings.TrimSpace(s))
	return err == nil
}

// ── 纯函数层的哨兵（"输入不合法"，不是业务错误）───────────────────────────

var (
	errEmptyB64     = cryptoErr("输入为空（不是合法的 base64url）")
	errBadSeedLen   = cryptoErr("ed25519 种子长度不是 32 字节")
	errBadPublicKey = cryptoErr("不是合法的 ed25519 SPKI 公钥（期望 44 字节且头为 302a300506032b6570032100）")
)

// cryptoErr 纯函数层的小错误类型（避免为几个字符串 import errors）。
type cryptoErr string

func (e cryptoErr) Error() string { return "phanthywork: " + string(e) }
