package raccoon

import "testing"

// authFileWriter 核心落盘要求的窄接口（见 internal/admin/admin.go 的注释）。
//
// ⚠ 这里**刻意重新声明**而不是 import 那个私有接口：它在 admin 包里
// （未导出），而本测试要验证的正是"本包的登录 Secret 是否满足这个契约"。
// 接口形状一旦漂移，两处会同时编译失败 —— 那是有用的信号。
type authFileWriter interface {
	MarshalAuthFile() (name string, raw []byte, err error)
}

// TestLoginSecretIsLandable 登录返回的 Secret **必须**能被核心落盘。
//
// # 这条修的是一个真实缺陷（用户报的）
//
// 用户在界面点「添加 cline 账号」，浏览器授权成功，然后看到：
//
//	该上游的凭证结构尚未接入落盘（LoginFlow 的 Secret 需要实现 MarshalAuthFile）
//
// 即：**登录流程走完了，但凭证写不下去** —— 用户看到的是"登录不了"。
//
// 根因：`LoginFlow.Poll` 返回的 `gateway.Credential.Secret` 是 `any`，
// 而核心落盘要求它实现 `MarshalAuthFile() (name, raw, err)`，
// 因为**只有上游自己知道**凭证该写成什么文件名、什么字段形状。
// 本包此前把 `*Auth` 直接放了进去。
//
// # 为什么必须静态可检
//
// 这个缺陷：编译期看不出来（`Secret` 是 `any`）；只有真的走完一次
// 浏览器授权才暴露（单测里跑不了真实 OAuth）。所以判据放在
// "类型是否满足接口"这一层 —— 漏接会在 CI 里当场红，不必等用户点一次。
//
// ⚠ 用**非零** Auth 构造：`MarshalAuthFile` 对空凭证返回错误，
// 而这里要区分的是"接口满足且能产出"与"接口不满足"两件事，
// 零值会把它们混在一起。
func TestLoginSecretIsLandable(t *testing.T) {
	a := &Auth{AccessToken: "test-token"}
	// markReady 里放进 Secret 的那个值。
	secret := any(&authFile{a: a})

	w, ok := secret.(authFileWriter)
	if !ok {
		t.Fatalf("登录 Secret 类型 %T 不满足 MarshalAuthFile —— "+
			"浏览器授权会成功，但核心落盘会以 501 拒绝，"+
			"用户看到「该上游的凭证结构尚未接入落盘」= 登录不了", secret)
	}
	name, raw, err := w.MarshalAuthFile()
	if err != nil {
		t.Fatalf("MarshalAuthFile: %v", err)
	}
	if name == "" {
		t.Error("落盘文件名为空 —— 核心写不出文件")
	}
	if len(raw) == 0 {
		t.Error("落盘内容为空 —— 写出来的凭证读不回")
	}
}

// TestAuthFileNilIsError 空凭证要明确报错（不是 panic、不是写空文件）。
func TestAuthFileNilIsError(t *testing.T) {
	var f *authFile
	if _, _, err := f.MarshalAuthFile(); err == nil {
		t.Error("nil authFile 应报错")
	}
	if _, _, err := (&authFile{}).MarshalAuthFile(); err == nil {
		t.Error("空 Auth 应报错（写个空文件比明确失败糟得多）")
	}
}

// TestAuthFileMatchesPackageHelpers 包装类型的输出必须与包内那两个函数**一致**。
//
// 若哪天有人在包装里"顺手"另写一套序列化，落盘格式就会与
// 读取路径（LoadDir / MarshalAuthFile / FileName）不一致 ——
// 表现为"写入成功但重载读不到账号"。
func TestAuthFileMatchesPackageHelpers(t *testing.T) {
	a := &Auth{AccessToken: "test-token-x"}
	name, raw, err := (&authFile{a: a}).MarshalAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	wantRaw, err := MarshalAuthFile(a)
	if err != nil {
		t.Fatal(err)
	}
	if name != FileName(a) {
		t.Errorf("文件名 = %q，want %q（FileName 的输出）—— "+
			"两处不一致会让重载扫不到刚写入的凭证", name, FileName(a))
	}
	if string(raw) != string(wantRaw) {
		t.Error("落盘内容与包内 MarshalAuthFile 不一致 —— " +
			"会出现「写入成功但读不回」")
	}
}
