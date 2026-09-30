// qrlogin.go Raccoon 自报「我的登录链接要扫码」。
//
// # 为什么需要它（用户报的缺陷）
//
// Raccoon 的登录是**微信扫码**：`Start()` 返回的
// `https://xiaohuanxiong.com/login/mp?code=<32位hex>` 是**要扫的内容**，
// 不是"点开就能授权"的网页。而前端此前对所有上游只渲染
// 「打开授权页 / 复制链接」—— 链接显示在屏幕上，手机扫不了同一块屏幕，
// 于是「＋ 添加账号」等于不可用。
//
// 参照项目在自己的登录页里把该 URL 渲染成二维码（见 raccoon-qr.ts）。
// 本网关没有那个页面，所以这里产出**内联 SVG**，
// 由核心随登录回执下发、前端插进 DOM。
//
// # 与参照项目的一处差异（刻意）
//
//	参照   本机 HTTP 登录页里内联 SVG（页面自己画）
//	本实现 产出 SVG 字符串交给前端（后端画、前端插）
//
// 两者产物是同一个东西（一段 SVG），只是谁负责插进 DOM 不同。
// 这样做的好处与本包 login.go 里那条同源：**不绑端口、不注入 HTML**。
package raccoon

import (
	"workbuddy2api/internal/qrcode"
)

// qrSVGPixels 二维码 SVG 的边长（像素）。
//
// 158 是参照项目的默认值（raccoon-qr.ts 的 renderQrSvg）。
// 取它而不是自己定一个：微信扫一扫对模块尺寸敏感，
// 而这个值已在参照项目里实测可用。
const qrSVGPixels = 158

// QRLoginSVG 实现 gateway.QRLoginExt：把登录 URL 渲染成二维码 SVG。
//
// authURL 为空、或内容长到编不出来时返回空串 ——
// 由调用方回落到"显示链接"，而不是给出一个扫不出来的坏码。
// （本包的登录 URL 约 80 字节，远低于 213 字节上限，
// 所以正常路径不会走到空串那支。）
func (p *Provider) QRLoginSVG(authURL string) string {
	if authURL == "" {
		return ""
	}
	svg, err := qrcode.RenderSVG(authURL, qrSVGPixels)
	if err != nil {
		// 编不出来就如实回落（不产出坏码）。
		// 这里不记日志：核心会在回执里体现"没有二维码"，
		// 而日志会被每次点「添加账号」刷一遍。
		return ""
	}
	return svg
}
