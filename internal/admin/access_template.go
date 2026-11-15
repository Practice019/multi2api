// access_template.go 接入模板（U-11 任务闭环的最后一步）。
//
// # 它解决什么
//
// 计划 §5 U-11 的「任务闭环」最后一步是「**复制无密钥接入模板**」，
// 验收补充写着「复制不含秘密」。
//
// 改造前用户要自己拼：从页面读出 baseURL、自己找模型名、自己写 curl，
// 而"顺手把页面上的 key 复制进去再发给同事"是这类模板最常见的泄漏方式。
//
// ⇒ 服务端生成一份**只有占位符、没有真值**的模板。密钥由使用者从环境变量注入，
// 于是"复制粘贴"这个动作本身不可能带出密钥。
//
// # 为什么模板在服务端生成而不是前端拼
//
//	① baseURL 要知道请求方看到的 host（反代/端口映射之后前端猜不对）
//	② "不含秘密"这条约束要能被**判据**钉住 —— 服务端返回值是最好钉的位置
//	③ 端点清单与 v2 允许清单是同一份事实，不该在前端复制一遍
package admin

import (
	"net"
	"net/http"
	"strings"
)

// accessTemplateKeyPlaceholder 模板里代表密钥的**唯一**占位符。
//
// ⚠ 它绝不能是任何真实密钥的前缀形态（不以 `sk-` 开头）——
// 判据要靠它区分"占位符"与"泄漏的密钥"。
const accessTemplateKeyPlaceholder = "<YOUR_API_KEY>"

// accessTemplate 一份接入模板（**不含任何真值**）。
type accessTemplate struct {
	BaseURL   string            `json:"base_url"`
	Endpoints []string          `json:"endpoints"`
	ModelHint string            `json:"model_hint"`
	Templates map[string]string `json:"templates"`
	Note      string            `json:"note"`
}

// accessBaseURL 从请求推出"这个客户端看到的"基址。
//
// # 为什么不用配置里的固定值
//
// 同一个网关常被反代到不同域名（内网 IP、公网域名、端口映射）。
// 用请求的 Host 拼出来的才是**用户手上那个**地址 —— 配置里的值只对管理员本机成立。
//
// 只信 `r.Host`（由传输层给出），**不读 X-Forwarded-* **：
// 那些头是调用方随便填的，用它拼 URL 等于让模板指向攻击者指定的域名。
func accessBaseURL(r *http.Request) string {
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return ""
	}
	scheme := "https"
	// 回环 / 私网按 http（本机部署的常态），其余按 https。
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.HasPrefix(host, "10.") || strings.HasPrefix(host, "192.168.") ||
		strings.HasPrefix(host, "172.") {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

// buildAccessTemplate 生成模板。
//
// `model` 是建议填进示例的模型名（可空 ⇒ 用占位符）。
func buildAccessTemplate(r *http.Request, model string) accessTemplate {
	base := accessBaseURL(r)
	modelHint := strings.TrimSpace(model)
	if modelHint == "" {
		modelHint = "<YOUR_MODEL>"
	}
	v1 := base + "/v1"
	const envName = "WORKBUDDY_API_KEY"

	return accessTemplate{
		BaseURL:   v1,
		Endpoints: []string{"/chat/completions", "/models", "/images/generations"},
		ModelHint: modelHint,
		Templates: map[string]string{
			// 每个模板都用环境变量取密钥，**没有一处写死值**。
			"curl": "curl -sS " + v1 + "/chat/completions \\\n" +
				"  -H \"Authorization: Bearer $" + envName + "\" \\\n" +
				"  -H \"Content-Type: application/json\" \\\n" +
				"  -d '{\"model\":\"" + modelHint + "\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'",
			"python": "import os\n" +
				"from openai import OpenAI\n\n" +
				"client = OpenAI(\n" +
				"    base_url=\"" + v1 + "\",\n" +
				"    api_key=os.environ[\"" + envName + "\"],  # 不要写死密钥\n" +
				")\n" +
				"print(client.chat.completions.create(\n" +
				"    model=\"" + modelHint + "\",\n" +
				"    messages=[{\"role\": \"user\", \"content\": \"hi\"}],\n" +
				").choices[0].message.content)",
			"node": "import OpenAI from \"openai\";\n\n" +
				"const client = new OpenAI({\n" +
				"  baseURL: \"" + v1 + "\",\n" +
				"  apiKey: process.env." + envName + ",  // 不要写死密钥\n" +
				"});\n" +
				"const r = await client.chat.completions.create({\n" +
				"  model: \"" + modelHint + "\",\n" +
				"  messages: [{ role: \"user\", content: \"hi\" }],\n" +
				"});\n" +
				"console.log(r.choices[0].message.content);",
			"env": "export " + envName + "=" + accessTemplateKeyPlaceholder + "\n" +
				"export OPENAI_BASE_URL=" + v1 + "\n" +
				"export OPENAI_API_KEY=$" + envName,
			"models": "curl -sS " + v1 + "/models -H \"Authorization: Bearer $" + envName + "\"",
		},
		Note: "模板里只有占位符 " + accessTemplateKeyPlaceholder +
			"，密钥由环境变量 " + envName + " 注入 —— 复制这段文本不会带出任何凭据。",
	}
}

// accessTemplateHandler GET /admin/access-template —— 生成一份无密钥接入模板。
//
// 只读、管理员限定、禁缓存。
func (h *Handler) accessTemplateHandler(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !h.principalFrom(r).IsAdmin() {
		writeError(w, http.StatusForbidden, "admin required")
		return
	}
	// ?model= 可指定示例模型；空则用占位符。
	writeJSON(w, http.StatusOK, buildAccessTemplate(r, r.URL.Query().Get("model")))
}
