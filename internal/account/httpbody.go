package account

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// httpbody.go —— 本包所有端点共用的 JSON 请求体解码（T9 抽出来的共享层）。
//
// # 为什么必须共用一份
//
// 解码纪律有四处细节，漏任何一处都是真实故障：
//
//	MaxBytesReader      匿名端点不限大小 ⇒ 一条 1GB 的请求体就能把内存打满
//	DisallowUnknownFields 收下多余字段 ⇒ 前端写错字段名时**静默**按默认值走
//	单个 JSON 对象        允许 `{...}{...}` ⇒ 后面的字节被静默忽略
//	区分"太大"与"格式错"  两者给人的信息不同（一个是攻击/误用，一个是调用方写错了）
//
// 原先只有注册/登录一处调用（`decodeCreds`），逐字复制第二份的后果是
// 两处**慢慢分叉** —— 而分叉点恰恰是"新加的那个端点忘了 DisallowUnknownFields"
// 这类只看代码看不出来的问题。
//
// ⚠ 解析错误原文不回吐：它可能含请求内容片段（例如把口令打进了错误信息里）。
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体过大")
			return false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法的 JSON")
		return false
	}
	// 只读一次：多给的字节意味着请求体里有别的东西，不接受。
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体应当是单个 JSON 对象")
		return false
	}
	return true
}
