package raccoon

import (
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestQRLoginSVGIsValidSVG raccoon 必须自报"我的登录链接要扫码"。
//
// # 为什么这条是本轮的核心判据
//
// raccoon 的 auth_url 是**要扫的内容**（`.../login/mp?code=…`），
// 不是"点开就能授权"的网页。前端此前对所有上游只渲染「打开链接」，
// 于是它的「＋ 添加账号」给出一个扫不了的链接 —— 用户报的缺陷。
//
// 修法是让上游自报"要扫码"，核心据此下发二维码 SVG。
// 少了这个扩展点，界面就退回"给一个扫不了的链接"。
func TestQRLoginSVGIsValidSVG(t *testing.T) {
	p := NewWithConfig(Config{})
	ext, ok := gateway.ExtOf[gateway.QRLoginExt](p)
	if !ok {
		t.Fatal("raccoon 必须实现 gateway.QRLoginExt —— 否则「添加账号」给出扫不了的链接")
	}

	url := QRLoginURL("0123456789abcdef0123456789abcdef")
	svg := ext.QRLoginSVG(url)
	if svg == "" {
		t.Fatal("真实登录 URL 编不出二维码 —— 「添加账号」会退回给一个扫不了的链接")
	}
	for _, want := range []string{"<svg", "viewBox=", "<path", "</svg>"} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG 缺 %q", want)
		}
	}
}

// TestQRLoginSVGEmptyOnEmptyURL 空 URL 返回空串（不产出坏码）。
//
// 空串是"本次不展示二维码"的合法表达 —— 调用方回落到"显示链接"。
// 返回一个编码了空串的二维码（扫出来什么都没有）比不显示更糟：
// 用户会以为码坏了。
func TestQRLoginSVGEmptyOnEmptyURL(t *testing.T) {
	p := NewWithConfig(Config{})
	ext, _ := gateway.ExtOf[gateway.QRLoginExt](p)
	if got := ext.QRLoginSVG(""); got != "" {
		t.Errorf("空 URL 应返回空串，实际返回了 %d 字节", len(got))
	}
}

// TestQRLoginSVGEncodesTheURL 二维码内容必须**就是**登录 URL。
//
// ⚠ 这条不能靠"看 SVG 里有没有字符串"验证 —— 二维码是编码过的，
// URL 不会以明文出现在 SVG 里。所以判据是**结构性的**：
// 不同 URL 必须产出不同的 SVG（相同则说明编码时丢了输入）。
//
// 真正的"扫出来是什么"由 internal/qrcode 的交叉验证测试守住
// （它与独立的 Python 实现逐位比对）。
func TestQRLoginSVGEncodesTheURL(t *testing.T) {
	p := NewWithConfig(Config{})
	ext, _ := gateway.ExtOf[gateway.QRLoginExt](p)

	a := ext.QRLoginSVG(QRLoginURL("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	b := ext.QRLoginSVG(QRLoginURL("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if a == b {
		t.Error("两个不同的登录 URL 产出了相同的二维码 —— 编码时丢了输入")
	}
	// 同一个 URL 两次必须相同（确定性）
	if ext.QRLoginSVG(QRLoginURL("cccccccccccccccccccccccccccccccc")) !=
		ext.QRLoginSVG(QRLoginURL("cccccccccccccccccccccccccccccccc")) {
		t.Error("同一 URL 两次产出了不同的二维码 —— 不可复现")
	}
}

// TestProviderSatisfiesQRLoginExtOnMethodSet 扩展点必须挂在 Provider 自己身上。
//
// ⚠ 与 login.go 里 Start/Poll/Configured 那三个转发方法同一个理由：
// 核心用 `ExtOf[T](p)`（**类型断言**）发现扩展点，而"返回接口的访问器"
// 不参与方法集匹配。少了它，本包测试全绿但界面拿不到二维码。
func TestProviderSatisfiesQRLoginExtOnMethodSet(t *testing.T) {
	var p gateway.Provider = NewWithConfig(Config{})
	if _, ok := p.(gateway.QRLoginExt); !ok {
		t.Error("*Provider 必须**直接**满足 gateway.QRLoginExt（方法集匹配），" +
			"否则 ExtOf 断言看不见它 → 界面拿不到二维码")
	}
}
