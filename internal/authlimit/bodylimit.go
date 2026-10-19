package authlimit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxAnonymousBody 匿名端点（登录/注册/找回口令）的请求体上限：8 KiB。
//
// 这几个端点的请求体就是几个短字段（用户名 ≤32、口令 ≤128、题面 ID 与答案），
// 8 KiB 宽到把"多打几个空格、长口令、多带两个前端字段"全包进来，而比它大的
// 请求在这个端点上没有任何合法用途 —— 那只是拿我们的内存和 JSON 解析换血的
// 东西。new-api 给同类端点装的是同一个中间件：
// `middleware/request_body_limit.go` 的 `AnonymousRequestBodyLimit()`，挂在
// `router/api-router.go:79-81` 的 `/register` 与 `/login` 上。
const MaxAnonymousBody = 8 << 10

// bodyTooLargeCode 413 响应的错误码（与 internal/server 的 writeOpenAIError
// 同形的信封，见 writeJSONError）。
const bodyTooLargeCode = "body_too_large"

// EnforceBodyLimit 把匿名端点的请求体卡在 max 字节以内。
//
// 不超限返回 true，并把**读到的那份 body 原样放回**：r.Body 换成新的
// io.NopCloser(bytes.NewReader(...))、r.ContentLength 同步改成真实长度。这一步
// 不是洁癖 —— 不还回去就等于把请求体吃掉了，后面的 handler 拿到空 body，表现
// 成"用户名和口令都是空的"这种莫名其妙的 400。重装的做法照抄 new-api 的
// readAnonymousRequestBody + AnonymousRequestBodyLimit
// （middleware/request_body_limit.go）：它也是先整份读出来，再换回
// bytes.Reader。
//
// 超限返回 false，并且**已经写好** 413 + JSON 响应，调用方直接 return 即可，
// 不要再写第二个响应。
//
// # 为什么不看 Content-Length（这是本函数存在的理由）
//
//   - Content-Length 是**客户端自己写的**一个头。写 `Content-Length: 10`
//     再送 20 MiB 是允许的；用 chunked 时连这个头都没有。只看它等于没卡。
//   - 也不能"把 body 包一层就返回"：包上之后，超限是在**下一次 Read** 才被
//     发现的，谁读谁才知道 —— 而本包不可能替所有 handler 把 body 读完
//     （有些 handler 就指望自己按流式读）。所以这里主动读一遍，读到爆为止。
//   - 用 http.MaxBytesReader 而不是 io.LimitReader：前者超限时返回
//     *http.MaxBytesError（可判定），并且会通知 net/http 这条连接不可复用
//     （免得残留的未读字节污染同一 keep-alive 上的下一个请求）。同样的取舍
//     在本仓已经踩过一遍：internal/server/handler.go 的 chatCompletions 用了
//     MaxBytesReader，注释写着"LimitReader 只会静默截断，把『太大』伪装成
//     『JSON 畸形』"。
//
// max <= 0 退回 MaxAnonymousBody：0 在这里只会是"忘了传"，而不是"一个字节都
// 不许传"（不许传的正确表达是 max = 1）。r.Body 为 nil（GET/HEAD 之类）直接
// 放行，没什么可卡的。
func EnforceBodyLimit(w http.ResponseWriter, r *http.Request, max int64) bool {
	if max <= 0 {
		max = MaxAnonymousBody
	}
	if r == nil || r.Body == nil {
		return true
	}

	limited := http.MaxBytesReader(w, r.Body, max)
	// 原始 body 读完即关（new-api 那里同样是读完就 _ = originalBody.Close()）。
	// 之后 r.Body 指向的是内存里那份，与它无关。
	defer func() { _ = limited.Close() }()

	body, err := io.ReadAll(limited)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, bodyTooLargeCode,
				fmt.Sprintf("请求体过大（上限 %d 字节）", max))
			return false
		}
		// 读失败（客户端半路断开等）：不是"太大"，但请求同样已经没法处理，
		// 回 400 让它别把"读不出来"当成"服务端没反应"再重试一遍。
		writeJSONError(w, http.StatusBadRequest, "bad_request", "请求体读取失败")
		return false
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return true
}

// errEnvelope 对外错误信封：{"error":{"code":...,"message":...}}。
//
// 与 internal/server 的 writeOpenAIError 同形（那里多了个 "type" 字段）。本包
// 不认识那个类型体系，就不多写一个自己对不上的字段。
type errEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSONError 写一个 JSON 错误响应。code 是给机器看的稳定串（前端据此分支），
// message 是给人看的中文。
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	raw, err := json.Marshal(map[string]errEnvelope{
		"error": {Code: code, Message: msg},
	})
	if err != nil {
		// 只有 string 字段，Marshal 不会失败；真失败了也不能回一个半截 JSON。
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
