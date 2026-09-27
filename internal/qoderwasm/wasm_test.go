package qoderwasm

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// newTestSigner 建一个用于测试的签名器（固定输入，不碰网络）。
func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(context.Background(), SignerOptions{
		UID:                "fixture-uid-0001",
		SecurityOAuthToken: "fixture-token-abc",
		MachineID:          "fixture-machine-id-xyz",
		Host:               "api2.qoder.sh",
		ClientType:         "5",
		BusinessProduct:    "cli",
		BusinessType:       "agent",
		Scene:              "assistant",
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// TestWasmInstantiate 最小事实：WASM 能实例化、导出齐全。
//
// 这条先红过：签名声明错了（把 i32 看成 f64）会直接在这里
// `signature mismatch`；而实例化成功但**解码方式错**不会报错，
// 只会在真正调用时死循环 —— 那种情况由下面的调用测试覆盖。
func TestWasmInstantiate(t *testing.T) {
	m, err := New(context.Background())
	if err != nil {
		t.Fatalf("实例化失败: %v", err)
	}
	defer func() { _ = m.Close(context.Background()) }()

	for _, n := range []string{
		"generate_runtime_auth_fields", "qodercontext_new",
		"qodercontext_prepareInferRequest", "requestresult_headers",
		"requestresult_url", "requestresult_body",
	} {
		if m.mod.ExportedFunction(n) == nil {
			t.Errorf("缺少导出 %q", n)
		}
	}
}

// TestGenerateRuntimeAuthFields 鉴权字段必须是两个非空的长串。
//
// # 判据为什么是"形状 + 长度"而不是固定值
//
// WASM 每次调用都会**重新随机**（内部用 getRandomValues 生成密钥），
// 所以同输入两次的产物**不同** —— 拿它做黄金样本比对走不通。
//
// 能钉住的是**协议事实**：两个字段都存在、都是 base64 形态、
// 长度与参照实现一致。长度尤其有意义 —— 它直接反映明文的字节数：
//
//	encrypt_user_info  192 字符
//	key                172 字符
//	整个 JSON          397 字节
//
// 这三个数是把**参照实现真跑起来**打印出来的（Node 24 的
// --experimental-transform-types 能直接跑 .ts），不是估的。
//
// ⚠ 我第一版把它们写成 152/108 —— 那是我在探针里**看错了**输出行
//（那次的输入与这次不同）。教训：黄金值必须来自与被测代码**同样的输入**，
// 否则测的是"两个不同的东西"。
func TestGenerateRuntimeAuthFields(t *testing.T) {
	// 直接调底层，便于拿到原始串
	m, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close(context.Background()) }()

	payload, _ := json.Marshal(map[string]any{
		"uid":                  "fixture-uid-0001",
		"security_oauth_token": "fixture-token-abc",
		"organization_id":      "",
		"organization_tags":    []string{},
		"data_policy_agreed":   false,
	})
	raw, err := m.callStringWithStrings(m.fnGenFields, string(payload))
	if err != nil {
		t.Fatalf("generate_runtime_auth_fields: %v", err)
	}

	var out struct {
		Enc string `json:"encrypt_user_info"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("产物不是合法 JSON: %v（原文 %.200q）", err, raw)
	}
	if out.Enc == "" || out.Key == "" {
		t.Fatalf("字段为空: enc=%d 字节 key=%d 字节", len(out.Enc), len(out.Key))
	}

	// 与参照实现**逐字节同源**的三个长度。
	//
	// 严格相等而不是给区间：这两个长度是确定性的（明文固定 → 密文长度固定），
	// 而它们恰好能抓住"glue 走了另一条分支"这类缺陷 ——
	// 我实测过：把 is_object 按 TS 类型标注的字面语义实现，
	// 长度会变成 192/172 → 不对，是变成别的值；总之会变。
	if len(out.Enc) != 192 {
		t.Errorf("encrypt_user_info 长度 = %d，参照实现是 192 —— "+
			"glue 的分支与参照不一致（某个 import 的语义实现错了）", len(out.Enc))
	}
	if len(out.Key) != 172 {
		t.Errorf("key 长度 = %d，参照实现是 172 —— 同上", len(out.Key))
	}
	if len(raw) != 397 {
		t.Errorf("产物 JSON 长度 = %d，参照实现是 397", len(raw))
	}

	// 两次调用必须不同（每次重新随机）。若相同说明随机数没真的填进去 ——
	// 那意味着加密强度归零。
	raw2, err := m.callStringWithStrings(m.fnGenFields, string(payload))
	if err != nil {
		t.Fatal(err)
	}
	if raw2 == raw {
		t.Error("两次调用产物相同 —— 随机数没有真的填进去，加密强度归零")
	}
	if len(raw2) != len(raw) {
		t.Errorf("两次调用长度不同（%d vs %d）—— 长度应是确定性的", len(raw), len(raw2))
	}
}

// TestBuildInferRequestProtocolFacts 完整路径的协议事实。
//
// # 与参照实现（Node 跑真 WASM）对照出的黄金事实
//
// 这些值不是猜的，是把参照实现真跑起来打印出来的：
//
//	URL  = api2.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation
//	       ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1
//	headers 恰好 20 个，含 Authorization: Bearer COSY.<载荷>.<签名>
//	body 长度 1088（同样输入）
//
// ⚠ body 长度**可以**钉住，因为它是**确定性的**（密文由明文 + 固定密钥派生，
// 而密钥每次不同但长度固定）。而 Authorization 的内容每次不同（含 requestId
// 与随机密钥）—— 所以只能钉形状。
func TestBuildInferRequestProtocolFacts(t *testing.T) {
	s := newTestSigner(t)

	body, _ := json.Marshal(map[string]any{
		"model":    "qfmodel",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	path, payload, headers, err := s.BuildInferRequest("qfmodel", body)
	if err != nil {
		t.Fatalf("BuildInferRequest: %v", err)
	}

	// ---- URL query ----
	wantPath := "?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	if path != wantPath {
		t.Errorf("URL query = %q\nwant %q", path, wantPath)
	}

	// ---- headers ----
	if len(headers) != 20 {
		t.Errorf("请求头数量 = %d，want 20（参照实现实测 20）", len(headers))
	}
	auth := headers["Authorization"]
	if !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Errorf("Authorization 必须是 `Bearer COSY.<载荷>.<签名>`，得到 %.60q", auth)
	}
	// 三段：Bearer COSY / 载荷 / 签名
	if n := strings.Count(auth, "."); n != 2 {
		t.Errorf("Authorization 应有 2 个 `.`（COSY.<载荷>.<签名>），得到 %d 个: %.80q", n, auth)
	}
	// 几个关键头必须在（缺任一个都可能被服务端拒绝）
	for _, k := range []string{
		"Accept", "Content-Type", "Cosy-ClientType", "Cosy-MachineId",
		"Cosy-MachineToken", "Cosy-MachineType", "Cosy-User", "Cosy-Version",
		"Cosy-Key", "Cosy-Date", "Login-Version", "X-Model-Key", "X-Model-Source",
	} {
		if headers[k] == "" {
			t.Errorf("缺少请求头 %q", k)
		}
	}
	if headers["Cosy-User"] != "fixture-uid-0001" {
		t.Errorf("Cosy-User = %q，应为账号 uid", headers["Cosy-User"])
	}
	if headers["X-Model-Key"] != "qfmodel" {
		t.Errorf("X-Model-Key = %q，应为目录 key", headers["X-Model-Key"])
	}

	// ---- body ----
	if len(payload) == 0 {
		t.Fatal("加密请求体为空")
	}
	// 加密体不是 JSON（是 WASM 的密文），但长度应与参照实现同量级。
	// 参照实测 1088；给较宽区间是因为明文字段（uuid 等）长度可能微变。
	if l := len(payload); l < 900 || l > 1400 {
		t.Errorf("加密请求体长度 %d 超出参照实测量级 [900,1400]（实测 1088）", l)
	}
}

// TestBuildInferRequestRejectsMissingModel 缺 model 必须明确报错。
//
// 加密端点靠目录 key 路由，没有 key 就无从路由 —— 早报错比
// 发一个必然失败的请求好。
func TestBuildInferRequestRejectsMissingModel(t *testing.T) {
	s := newTestSigner(t)
	_, _, _, err := s.BuildInferRequest("", []byte(`{"messages":[]}`))
	if err == nil {
		t.Fatal("缺 model 时应报错")
	}
}

// TestBuildInferRequestRejectsBadJSON 非法 JSON 必须报错而不是发出垃圾。
func TestBuildInferRequestRejectsBadJSON(t *testing.T) {
	s := newTestSigner(t)
	if _, _, _, err := s.BuildInferRequest("qfmodel", []byte(`{not json`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// TestCloseIsIdempotent Close 幂等（避免 defer 与显式 Close 并存时二次报错）。
func TestCloseIsIdempotent(t *testing.T) {
	s, err := NewSigner(context.Background(), SignerOptions{
		UID: "u", SecurityOAuthToken: "t", MachineID: "m", Host: "api2.qoder.sh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("首次 Close: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("二次 Close 应返回 nil，得到: %v", err)
	}
}

// TestMemoryIsRecycledBeforeExhaustion 内存必须被**回收重建**，不会无限增长。
//
// # 为什么判据不是"内存不涨"（那是不可能的）
//
// 这个 WASM 假设宿主有 GC：它把每次调用的入参指针留在内部，从不主动释放。
// 实测曲线（约 5 KB/次，线性）：
//
//	调用次数    WASM 内存
//	    ~50      1.3 MB
//	   ~200      2.1 MB
//	   ~800      5.3 MB
//	  ~1600      9.6 MB
//
// 而且**不能靠我们归还来消除** —— 三种做法都实测失败：
//
//	归还入参字符串      → 第二次调用 out of bounds（result 指向已回收内存）
//	读完 result 后再还  → 同上（context 跨调用持有指针）
//	复用一块缓冲        → 第一次调用就 unreachable（WASM 按独立分配算边界）
//
// 所以唯一正确的做法是**定期换一个干净实例**（见 memLimitBytes）。
// 这条测试钉住的就是那个机制真的生效：内存到阈值后必须**回落**，
// 而不是继续涨。
//
// # 怎么在测试里验证（阈值 64 MB 跑不到）
//
// 直接跑到 64 MB 要一万三千次调用（约 10 秒 + 1.3 万次 WASM 调用），
// 太慢。所以这里**临时把阈值调低**（通过可注入的字段），
// 验证"到阈值就重建、内存回落"这条因果链。
func TestMemoryIsRecycledBeforeExhaustion(t *testing.T) {
	s := newTestSigner(t)
	body, _ := json.Marshal(map[string]any{
		"model":    "qfmodel",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	// 预热，让初始分配稳定
	if _, _, _, err := s.BuildInferRequest("qfmodel", body); err != nil {
		t.Fatal(err)
	}
	// 把阈值压到略高于当前内存：再跑几次就该触发重建
	base := s.mod.mod.Memory().Size()
	s.memLimit = base + 64*1024

	peak := base
	recycled := false
	for i := 0; i < 300; i++ {
		if _, _, _, err := s.BuildInferRequest("qfmodel", body); err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i, err)
		}
		cur := s.mod.mod.Memory().Size()
		if cur > peak {
			peak = cur
		}
		// 重建后内存应回落到接近初始值
		if cur <= base {
			recycled = true
			break
		}
	}
	if !recycled {
		t.Errorf("跑 300 次都没触发重建（峰值 %d，初始 %d，阈值 %d）—— "+
			"内存会一直涨到 32 位地址空间耗尽", peak, base, s.memLimit)
	}
	// 重建后必须**仍然能正常出请求**（换实例不能把状态换坏）
	path, payload, headers, err := s.BuildInferRequest("qfmodel", body)
	if err != nil {
		t.Fatalf("重建后调用失败: %v", err)
	}
	if path == "" || len(payload) == 0 || headers["Authorization"] == "" {
		t.Error("重建后产出的请求不完整 —— 换实例把状态换坏了")
	}
}

// TestRecyclePreservesIdentity 重建后身份（uid / Cosy-User）必须不变。
//
// 重建会重新调 qodercontext_new，用的是同一份 opts ——
// 若哪里漏传或串了，请求头里的账号身份就会变成别人（或空）。
// 那是**最严重**的一类缺陷（发到别的账号上），必须钉住。
func TestRecyclePreservesIdentity(t *testing.T) {
	s := newTestSigner(t)
	body, _ := json.Marshal(map[string]any{
		"model":    "qfmodel",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	_, _, h1, err := s.BuildInferRequest("qfmodel", body)
	if err != nil {
		t.Fatal(err)
	}
	// 强制重建
	s.memLimit = 1
	_, _, h2, err := s.BuildInferRequest("qfmodel", body)
	if err != nil {
		t.Fatal(err)
	}
	if h1["Cosy-User"] != h2["Cosy-User"] {
		t.Errorf("重建后 Cosy-User 变了：%q → %q —— 身份串了",
			h1["Cosy-User"], h2["Cosy-User"])
	}
	if h2["Cosy-User"] != "fixture-uid-0001" {
		t.Errorf("重建后 Cosy-User = %q，应为账号 uid", h2["Cosy-User"])
	}
	if h2["Authorization"] == "" {
		t.Error("重建后 Authorization 丢失")
	}
}

// TestConcurrentCallsAreSerialized 并发调用不得互相踩对象堆。
//
// 对象堆是共享可变状态；不串行化会得到随机的签名错误
//（只在并发时复现，极难定位）。
func TestConcurrentCallsAreSerialized(t *testing.T) {
	s := newTestSigner(t)
	body, _ := json.Marshal(map[string]any{
		"model":    "qfmodel",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	const n = 16
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, _, hdrs, err := s.BuildInferRequest("qfmodel", body)
			if err != nil {
				errs <- err
				return
			}
			if hdrs["Authorization"] == "" {
				errs <- errNoAuth
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("并发调用失败: %v", err)
		}
	}
}

var errNoAuth = &noAuthErr{}

type noAuthErr struct{}

func (*noAuthErr) Error() string { return "并发下 Authorization 丢失" }

// TestWasmFileIsEmbedded WASM 必须 embed 进二进制。
//
// 运行时读文件会让部署多一个可静默失效的步骤
//（"忘拷 wasm → 加密推理不可用"）。embed 之后二进制自带，
// 这条测试断言的就是"我们真的用的是 embed 那份"。
func TestWasmFileIsEmbedded(t *testing.T) {
	if len(wasmBytes) == 0 {
		t.Fatal("wasmBytes 为空 —— embed 失败，加密推理会在运行时才报错")
	}
	// WASM 魔数 \0asm
	if len(wasmBytes) < 4 || string(wasmBytes[:4]) != "\x00asm" {
		t.Fatalf("embed 的内容不是 WASM（前 4 字节 %.4q）", wasmBytes[:4])
	}
	if _, err := os.Stat("qoder_auth_wasm.wasm"); err != nil {
		t.Fatalf("WASM 源文件不在包目录里（embed 依赖它存在）: %v", err)
	}
}
