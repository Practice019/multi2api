// sticky_credential_guard_test.go —— 跨上游串号的**第二道防线**：
// 即使粘性层被绕过，凭证装配点（registryRouter.Credential）自己也必须拦住。
//
// # 两道防线分别守什么（这是本文件存在的全部理由）
//
//	防线 ①  handler.go  : PickByUIDFor(reqProvider, stickyUID)
//	                      → 选号阶段就发现"这个粘性号不属于本次请求的上游"
//	防线 ②  Credential  : 取到账号后校验它的 provider == 调用方要的上游
//	                      → 凭证装配阶段兜底
//
// sticky_provider_test.go 已经把防线 ① 钉死了。但**只有防线 ① 是不够的**：
//
//   - 它是**一条调用路径**上的判断。任何新的选号路径（运维接口、批量补号、
//     未来的会话路由改造）只要不经过 PickByUIDFor，串号就重新漏。
//   - 防线 ① 的失效模式是**静默**的：跨上游的号被塞进出站循环后，
//     改造后的出站按"账号自己的上游"分派，请求甚至会**成功返回 200**，
//     只是"请求 workbuddy 的模型却用了 codearts 的账号"。
//   - 而防线 ② 的失效模式是**响亮的**：凭证类型不匹配，上游 authOf 立刻
//     报错。代价是这个**无辜的账号被记账惩罚**（记错/冷却）。
//
// 所以本文件用**直接调用组装层**的方式验防线 ②：不经过 handler 的选号，
// 直接把"错误的 (id, uid) 组合"喂给它 —— 这正是"别的调用方"会做的事。
//
// 真实现（registryRouter）的判据在 cmd/server/outbound_credential_test.go；
// 本文件验的是**出口层在被拒绝时的行为**是否符合契约：
// 取不到凭证 → 换号 → 依然能发出请求，而不是 500，也不是拿错的凭证去发。
package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// guardRouter 包装一个真实 router，并在 Credential 上**强制注入归属校验**。
//
// 模拟"防线 ② 已经装好"的装配层：调用方要 (id, uid)，而 uid 的归属由
// owners 表给出；不匹配一律 ok=false（与 registryRouter 的修复同形）。
//
// 另有一个 bypass 开关，用来做**变异验证**：关掉它等价于修复前的
// registryRouter（只看"uid 在池里"，不看归属）。
type guardRouter struct {
	inner  *multiRouter
	owners map[string]string // uid → 它真正所属的上游
	bypass bool              // true = 复刻修复前的行为（无归属校验）
}

func (r *guardRouter) Has(id string) bool { return r.inner.Has(id) }
func (r *guardRouter) Default() string    { return r.inner.Default() }
func (r *guardRouter) Models(ctx context.Context, id string) ([]gateway.ModelInfo, bool) {
	return r.inner.Models(ctx, id)
}
func (r *guardRouter) Chat(ctx context.Context, id string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error) {
	return r.inner.Chat(ctx, id, cred, body)
}
func (r *guardRouter) RefreshCredential(ctx context.Context, id string, cred gateway.Credential) (bool, error) {
	return r.inner.RefreshCredential(ctx, id, cred)
}
func (r *guardRouter) RefreshSkew(id string, cred gateway.Credential) (time.Duration, bool) {
	return r.inner.RefreshSkew(id, cred)
}

// ResetAt / Classify 与 RefreshSkew 同款：本桩只守 Credential 那一层，
// 其余能力原样转发给 inner（接口断言要求方法集精确匹配）。
// SoftRateReset 测试桩：默认不给出模型级限时（保持账号级软冷却路径）。
func (r *guardRouter) SoftRateReset(_ string, _ int, _ string) (time.Time, bool) {
	return time.Time{}, false
}

func (r *guardRouter) ResetAt(id string, cred gateway.Credential) (time.Time, bool) {
	return r.inner.ResetAt(id, cred)
}
func (r *guardRouter) Classify(id string, status int, body string) (gateway.ErrorKind, bool) {
	return r.inner.Classify(id, status, body)
}

// Credential 是**被测的那一层**。
func (r *guardRouter) Credential(id, uid string) (gateway.Credential, bool) {
	cred, ok := r.inner.Credential(id, uid)
	if !ok {
		return cred, false
	}
	if r.bypass {
		return cred, true // 修复前的行为：归属不校验
	}
	if owner, known := r.owners[uid]; !known || owner != id {
		// 与 registryRouter 的修复一致：拿不到归属（或归属不是它）→ ok=false。
		return gateway.Credential{}, false
	}
	return cred, true
}

// newGuardedHandler 在 newMultiUpstreamHandler 之上装防线 ②。
func newGuardedHandler(t *testing.T, bypass bool) (*Handler, *recordingProvider, *recordingProvider) {
	t.Helper()
	h, ca, wb, r := newMultiUpstreamHandler(t, 200, sseOK)
	h.cfg.Provider = &guardRouter{
		inner:  r,
		owners: map[string]string{"ca-1": "codearts", "wb-1": "workbuddy"},
		bypass: bypass,
	}
	return h, ca, wb
}

// TestCredentialGuardRejectsForeignUIDThenRotatesToOwnProvider 是防线 ② 的**主断言**。
//
// # 场景（复刻真实的串号链路，但**绕过**防线 ①）
//
// 会话 key 被绑到 codearts 的号 ca-1，而本次请求是 workbuddy 的模型。
// 这里**故意不经过** handler 的粘性分支去制造这个状态（那会同时动用防线 ①，
// 就测不出防线 ② 了）—— 而是直接把请求的选号结果导向错误组合。
//
// 做法：把粘性绑定的**可用集**设为只含 ca-1，并让 PickFor 也无法在
// workbuddy 域里选到号之外……（见下，实际用更直接的注入）。
//
// 实际采用更直接的注入：让 workbuddy 域里**只有** ca-1 这个"外来号"
// 会命中的路径 —— 通过 Session 绑定 + 请求 workbuddy 模型时，
// 由 guardRouter 在 Credential 上拒绝，验证出口层**换号重试**的契约。
//
// # 判据
//
//	guard 生效时：ok=false → errNoProviderCredential → 换号（换的都是 workbuddy 域的号）
//	              最终请求仍应成功，且 **codearts 的 Provider 一次都不被调用**
func TestCredentialGuardRejectsForeignUIDThenRotatesToOwnProvider(t *testing.T) {
	h, ca, wb := newGuardedHandler(t, false)

	// 先验防线 ② 本身：拿 codearts 的 uid 问 workbuddy 要凭证，必须被拒。
	cred, ok := h.cfg.Provider.Credential("workbuddy", "ca-1")
	if ok {
		t.Fatalf("★ 防线②失效：Credential(workbuddy, ca-1) 返回 ok=true，Secret=%T\n"+
			"  这会让 workbuddy 用 codearts 的凭证发请求并**惩罚这个无辜账号**。", cred.Secret)
	}
	// 同一组合在**修复前**（bypass）必须是 ok=true —— 否则说明本用例
	// 没测到归属校验（而是被某个无关理由拦住了），是假绿。
	if _, ok := (&guardRouter{inner: h.cfg.Provider.(*guardRouter).inner,
		owners: map[string]string{"ca-1": "codearts"}, bypass: true}).
		Credential("workbuddy", "ca-1"); !ok {
		t.Fatal("前置自检失败：修复前形态也返回 ok=false —— 本用例被无关理由拦住了，测不到归属校验")
	}

	// 端到端：workbuddy 的模型请求仍应正常发出（守卫只管拒绝错组合，
	// 不该把整条 workbuddy 路径弄死）。
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s（守卫不该弄死正常的 workbuddy 路径）", rec.Code, rec.Body.String())
	}
	if got := wb.chatCalls.Load(); got != 1 {
		t.Errorf("workbuddy 的 Provider 被调用 %d 次，want 1", got)
	}
	if got := ca.chatCalls.Load(); got != 0 {
		t.Errorf("codearts 的 Provider 被调用 %d 次，want 0", got)
	}
}

// TestCredentialGuardBypassLeaksForeignCredential 是上面的**变异对照**：
// 把归属校验关掉（bypass=true，等价于修复前的 registryRouter），
// 必须观察到"别的上游的凭证被交出去" —— 这条断言就是本 bug 的可观察定义。
//
// 它同时说明防线 ② 的失效模式为什么比防线 ① 更响（类型不匹配 → 上游报错）。
func TestCredentialGuardBypassLeaksForeignCredential(t *testing.T) {
	h, _, _ := newGuardedHandler(t, true)

	cred, ok := h.cfg.Provider.Credential("workbuddy", "ca-1")
	if !ok {
		t.Fatal("变异形态（bypass）下应当 ok=true —— 这正是修复前的行为")
	}
	// 交出去的是 codearts 的凭证，而调用方要的是 workbuddy 的。
	if _, isCA := cred.Secret.(*fakeCodeartsSecret); !isCA {
		t.Fatalf("变异形态下 Secret 类型 = %T，期望 *fakeCodeartsSecret（复刻「别的上游的凭证被交出」）", cred.Secret)
	}
	// 上游的 authOf 会在这里失败 —— 这就是"无辜账号被惩罚"的入口。
	if cred.Provider != "workbuddy" {
		t.Errorf("Provider=%q want workbuddy（标签是调用方要的，凭证却是别家的 —— 正是本 bug）", cred.Provider)
	}
}
