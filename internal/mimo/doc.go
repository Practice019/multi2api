// Package mimo 把小米 MiMo 开放平台（api.xiaomimimo.com，MiMo Code 同源协议）
// 适配成 gateway.Provider —— 本网关的第五个上游。
//
// # 两条通道（一包双轨，主走 paid）
//
//	paid（主目标）：OpenAI 兼容端点 POST {base}/chat/completions，
//	               长期 API key（sk- 按量 / tp- Token Plan 套餐），
//	               设计上**不会过期** —— 死法只有人为维度（删 key/欠费/风控）。
//	free（留位，默认关）：CLI 免费通道 bootstrap→1h JWT → /api/free-ai/openai/chat。
//	               2026-07-26 官方 sunset，实测 chat 全 403 illegal_access；
//	               代码与假上游测试备好，mimo.free_enabled=true 即复活。
//
// # 与 TRAE 事故的对账（实现红线，评审报告 §4.7/§6）
//
//  1. 账号池投影只带 {UID, Nickname} ⇒ auth.Auth.NeedsRefresh 恒真；
//     `RefreshSkew` 返回 ok=false **不等于**关预检（会回落核心 10m 兜底），
//     **唯一有效的关闭是 (0, true)**。本包恒返回 (0,true)。
//  2. Chat 的 401 自愈（oauth/free 轨）必须**真的写出来**并带钉测试，
//     不是注释里存在。
//  3. 凭证文件必须保存签发上下文（baseUrl/clientId），ExchangeToken 类
//     刷新永远用凭证自带的 id，不吃全局硬编码 —— TRAE ClientID 事故同款。
//
// # 协议方言（两大硬点）
//
//   - thinking 模式下带 tool_calls 的 assistant 历史**必须回传 reasoning_content**，
//     否则 400 Param Incorrect → dialect.go 做回注/降级（出站）与聚合回写（入站）。
//   - 非标错误藏在 HTTP 400 的**字符串** error.code 里（"421"=审查、"441"=风控）
//     → errorclassifier.go 先 body 后状态码。
//
// 外部参考（实现依据）：reports/mimo-upstream-implementation-report.md。
package mimo
