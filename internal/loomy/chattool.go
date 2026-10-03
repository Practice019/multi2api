// chattool.go Loomy 实现 gateway.ChatToolExt ——
// "用我的模型对话时，可注入这些工具 / 它们怎么执行"。
//
// # 为什么工具是**上游私有**的（用户的要求）
//
// 用户原话：「我希望只能在使用 Loomy 上游任意一个模型时，启用该上游对应的工具。」
//
// 这条作用域靠扩展点**自动**成立，不需要核心写任何 `if provider == "loomy"`：
//
//	请求路由到 loomy     → 出口层拿到 loomy 的 Provider → 问它 ChatTools()
//	请求路由到 workbuddy → 那个 Provider 没实现本扩展点 → 一个工具都不注入
//
// 因为工具背后是**本上游的端点**（下面两个都实打实打在 loomyad.xunfei.cn 上），
// 换个上游这些路径根本不存在。做成"核心内置工具"就等于让核心假装
// 知道所有上游的端点。
//
// # 两个工具（都是实测跑通的，不是照文档猜的）
//
//	generate_image  文生图   → POST {base}/images/generations   实测 200 + 图片
//	web_search      联网搜索 → POST {base}/search/tencent       实测 200 + Pages
//
// 两者的鉴权都是账号的 session（与对话同一份凭证），所以"配了 loomy 账号
// 就等于配了这两个工具"。
package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy2api/internal/gateway"
)

// 工具名常量。
//
// ⚠ 必须**跨请求稳定** —— 模型会在后续轮次里引用这个名字，
// 改名等于让会话历史里的旧调用对不上。
const (
	ToolGenerateImage = "generate_image"
	ToolWebSearch     = "web_search"
)

// chatTools 是注入给模型的工具定义。
//
// # 为什么 Description 写得这么细
//
// Description 是**唯一**影响模型"要不要用"的东西。写得含糊，模型就会在
// 用户明确要图时写一篇散文（甚至输出 SVG 代码）—— 实测：
// 不给工具时，请对话模型"画一只橘猫"，它返回的是一段 SVG 源码。
//
// 所以这里把"什么时候该用"和"什么时候**不该**用"都写清楚：
// 生图工具只负责出**位图**，矢量图/图表/流程图应当用文本能力回答。
var chatTools = []gateway.ChatTool{
	{
		Name: ToolGenerateImage,
		// 措辞参照 Loomy 自己的图片工具说明（其 loomy_image.js 的 description），
		// 三条关键规则逐字保留：
		//   ① 用什么做（位图）
		//   ② 【不调用的场景】矢量图/图表用代码回答
		//   ③ prompt 默认中文
		// 第 ② 条是有实测依据的：不给工具时对话模型会直接吐 SVG 源码
		// （我实测过），所以必须明确排除"能用代码表达的图"。
		Description: "生成图片（文生图）。当用户想要一张**位图**时调用：插画、海报、封面、" +
			"壁纸、头像、写实照片、场景图等。生成需要约 20-30 秒，会自动扣积分。" +
			"【不要用于】Mermaid/PlantUML/ASCII 等文本语法画图、流程图/时序图/架构图等" +
			"可用代码表达的图表、SVG/HTML/CSS 图形 —— 这些用代码回答即可，不需要调用本工具。" +
			"【重要】必须用原生工具调用，不要把工具名或参数打印成文本。" +
			"prompt 默认使用中文，除非用户明确要求其他语言。",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": {
					"type": "string",
					"description": "图片的完整描述。要具体：主体、风格、构图、色调、光照。默认必须使用中文，除非用户明确要求其他语言。"
				},
				"size": {
					"type": "string",
					"description": "图片尺寸，如 1024x1024 或 2304x1728。不确定时省略。"
				}
			},
			"required": ["prompt"]
		}`),
	},
	{
		Name: ToolWebSearch,
		Description: "联网搜索实时信息。当问题涉及**最新动态、具体事实、你不确定或可能过时**的" +
			"内容时调用（新闻、价格、版本号、人物近况、赛事结果等）。" +
			"【不要用于】你自己就能回答的通识问题、纯计算、写作、代码 —— 那类直接回答更快更省。",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "搜索关键词，用自然语言描述要找什么。"
				},
				"count": {
					"type": "integer",
					"description": "返回结果条数，可选 10/20/30/40/50。不确定时省略。"
				}
			},
			"required": ["query"]
		}`),
	},
}

// ChatTools 返回注入对话的工具定义（gateway.ChatToolExt）。
//
// 返回副本：调用方（出口层）会把它们塞进请求体，改到包级切片会污染后续请求。
func (p *Provider) ChatTools() []gateway.ChatTool {
	out := make([]gateway.ChatTool, len(chatTools))
	copy(out, chatTools)
	return out
}

// ExecuteChatTool 执行一次工具调用（gateway.ChatToolExt）。
//
// # 为什么失败**不**返回 error
//
// 工具失败（上游 5xx、积分不足、提示词被拒）时**返回 error 会让整个请求 502**，
// 用户看到的是"网关挂了"—— 而事实是"这次图没生成成功"。
//
// 正确形态：把失败写进 Content（`OK=false`），让**模型**去向用户解释
// （"图片服务暂时不可用，稍后再试"）。模型收尾比网关的错误页友好得多，
// 而且它知道上下文（用户刚说了什么）。
//
// error 只用于**调用方用法错误**（未知工具名）—— 那说明核心传错了名字，
// 是本仓的 bug，必须显式暴露而不是伪装成"工具执行失败"。
func (p *Provider) ExecuteChatTool(ctx context.Context, uid, name string, args json.RawMessage) (gateway.ChatToolResult, error) {
	switch name {
	case ToolGenerateImage:
		return p.execGenerateImage(ctx, uid, args)
	case ToolWebSearch:
		return p.execWebSearch(ctx, uid, args)
	default:
		// 未知工具名 = 出口层注入了不该注入的东西。这是编程错误，不是用户错误。
		return gateway.ChatToolResult{}, fmt.Errorf("loomy: 未知的对话工具 %q", name)
	}
}

// execGenerateImage 执行 generate_image 工具。
func (p *Provider) execGenerateImage(ctx context.Context, uid string, args json.RawMessage) (gateway.ChatToolResult, error) {
	var in struct {
		Prompt string `json:"prompt"`
		Size   string `json:"size"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		// 参数畸形是**模型**给的，不是用户给的 —— 所以不报 error，
		// 而是把原因回给模型让它自己修正（它下一轮可以重试）。
		return gateway.ChatToolResult{
			OK:      false,
			Content: "工具调用失败：prompt 参数不是合法 JSON 对象（" + err.Error() + "）。请用 {\"prompt\":\"…\"} 重试。",
		}, nil
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return gateway.ChatToolResult{
			OK:      false,
			Content: "工具调用失败：prompt 为空。请提供图片的完整描述后重试。",
		}, nil
	}

	// 请求体：与 Loomy 官方客户端同形（见 imagegen_ext.go 的实测记录）。
	// size 缺省 2304x1728（客户端默认值）；模型给了就用它的。
	size := strings.TrimSpace(in.Size)
	if size == "" {
		size = "2304x1728"
	}
	body, err := json.Marshal(map[string]any{
		"model":           imageModelIDs[0].ID, // 默认豆包 Seedream
		"prompt":          prompt,
		"n":               1,
		"size":            size,
		"response_format": "url",
	})
	if err != nil {
		return gateway.ChatToolResult{}, fmt.Errorf("loomy: 构造生图请求失败: %w", err)
	}

	raw, status, err := p.GenerateImage(ctx, uid, body)
	if err != nil {
		return gateway.ChatToolResult{
			OK:      false,
			Content: "图片生成失败（网络或凭证问题）：" + err.Error() + "。请告知用户稍后重试。",
		}, nil
	}
	if status < 200 || status >= 300 {
		// 上游的业务错误：把**它自己的话**回给模型（比网关重写更准）。
		return gateway.ChatToolResult{
			OK: false,
			Content: fmt.Sprintf("图片生成被上游拒绝（HTTP %d）：%s。请检查提示词是否符合内容规范，或告知用户稍后重试。",
				status, clipForTool(string(raw), 500)),
		}, nil
	}

	var resp struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
		PointsConsumed any `json:"points_consumed"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Data) == 0 || resp.Data[0].URL == "" {
		return gateway.ChatToolResult{
			OK:      false,
			Content: "图片生成返回了无法解析的结果：" + clipForTool(string(raw), 300),
		}, nil
	}

	url := resp.Data[0].URL
	points := ""
	if resp.PointsConsumed != nil {
		points = fmt.Sprintf("（消耗 %v 积分）", resp.PointsConsumed)
	}

	return gateway.ChatToolResult{
		OK: true,
		// Content 给**模型**看：它需要知道"图出来了 + URL 是什么"才能收尾。
		Content: fmt.Sprintf("图片已生成成功%s。图片 URL：%s\n请用一句自然的话告诉用户图片已生成，"+
			"不要复述这个 URL（网关会在回复末尾自动附上图片）。", points, url),
		// Markdown 给**客户端**渲染 —— 由出口层追加到最终回复末尾。
		// 这样即使客户端不支持 markdown 图片，URL 也是可见可复制的。
		Markdown: fmt.Sprintf("![%s](%s)", markdownAltText(prompt), url),
		Artifacts: []gateway.Artifact{{
			"type":            "image",
			"url":             url,
			"prompt":          prompt,
			"size":            size,
			"model":           imageModelIDs[0].ID,
			"points_consumed": resp.PointsConsumed,
		}},
	}, nil
}

// Search 用某个账号向上游发起一次联网搜索。
//
// 与 GenerateImage 同形：按 uid 取凭证、交给 Client 发请求。
// 取不到凭证时**明确报错**而不是发空 session（后者会被上游当成
// "缺少 token"，把归因带偏）。
func (p *Provider) Search(ctx context.Context, uid, query string, count int) ([]byte, int, error) {
	if p == nil {
		return nil, 0, fmt.Errorf("loomy: provider 未初始化")
	}
	a := p.authByUID(uid)
	if a == nil {
		return nil, 0, fmt.Errorf("loomy: 找不到账号 %s 的凭证（无法搜索）", shortUID(uid))
	}
	return p.client.Search(ctx, a, query, count)
}

// execWebSearch 执行 web_search 工具。
func (p *Provider) execWebSearch(ctx context.Context, uid string, args json.RawMessage) (gateway.ChatToolResult, error) {
	var in struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return gateway.ChatToolResult{
			OK:      false,
			Content: "工具调用失败：query 参数不是合法 JSON 对象（" + err.Error() + "）。请用 {\"query\":\"…\"} 重试。",
		}, nil
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return gateway.ChatToolResult{
			OK:      false,
			Content: "工具调用失败：query 为空。请提供搜索关键词后重试。",
		}, nil
	}

	raw, status, err := p.Search(ctx, uid, query, in.Count)
	if err != nil {
		return gateway.ChatToolResult{
			OK:      false,
			Content: "联网搜索失败（网络或凭证问题）：" + err.Error() + "。请告知用户稍后重试，或直接用你的知识回答。",
		}, nil
	}
	if status < 200 || status >= 300 {
		return gateway.ChatToolResult{
			OK: false,
			Content: fmt.Sprintf("联网搜索被上游拒绝（HTTP %d）：%s。请直接用你的知识回答，并说明未能联网核实。",
				status, clipForTool(string(raw), 300)),
		}, nil
	}

	// 把上游的 Pages 摘成模型能读的条目列表。
	pages, points, perr := parseSearchPages(raw)
	if perr != nil {
		return gateway.ChatToolResult{
			OK:      false,
			Content: "搜索返回了无法解析的结果：" + clipForTool(string(raw), 300),
		}, nil
	}
	if len(pages) == 0 {
		return gateway.ChatToolResult{
			OK:      true,
			Content: "搜索没有找到结果。请直接用你的知识回答，并说明未能联网核实。",
		}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "搜索到 %d 条结果", len(pages))
	if points != "" {
		fmt.Fprintf(&b, "（消耗 %s 积分）", points)
	}
	b.WriteString("：\n\n")
	for i, pg := range pages {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, pg.Title)
		// 站点与日期各一短行：它们帮模型判断"这条可不可信、够不够新"，
		// 而这两个判断它必须做（否则会把三年前的帖子当今天的新闻）。
		if meta := joinNonEmpty(" · ", pg.Site, pg.Date); meta != "" {
			fmt.Fprintf(&b, "    来源：%s\n", meta)
		}
		if pg.URL != "" {
			fmt.Fprintf(&b, "    %s\n", pg.URL)
		}
		if pg.Content != "" {
			fmt.Fprintf(&b, "    %s\n", clipForTool(pg.Content, 800))
		}
		b.WriteString("\n")
	}
	b.WriteString("请基于以上结果回答用户，并在合适处引用来源编号。")

	return gateway.ChatToolResult{
		OK:      true,
		Content: b.String(),
		Artifacts: []gateway.Artifact{{
			"type":            "search",
			"query":           query,
			"hit_count":       len(pages),
			"points_consumed": points,
		}},
	}, nil
}

// searchPage 一条搜索结果的投影。
//
// 字段取自实测的 Pages 条目（见 parseSearchPages 的键名清单）。
type searchPage struct {
	Title   string
	URL     string
	Content string
	Site    string
	Date    string
}

// parseSearchPages 从上游响应里摘出条目。
//
// # 字段名**照抄 Loomy 客户端的实际响应**（实测确认，不是猜的）
//
// 实测 `Pages[0]` 的全部键：
//
//	authority_level, content, date, favicon, passage, pics, score, site, title, url
//
// 所以取 `title` / `url` / `content`，站点名是 **`site`**（不是 `site_name`）。
// 我第一版按"常见命名"猜了 site_name/summary/snippet/doc_url ——
// **全部不存在**，结果是每个条目的标题与站点都为空（内容还在，
// 所以不会报错，只是喂给模型的条目质量悄悄变差）。这正是"猜字段名"
// 的典型代价：它不失败，只是变差。
//
// 仍保留少量常见别名的尝试：那是为了上游将来改字段名时**不至于全空**，
// 而不是替代实测。别名只在主键缺失时生效。
//
// # 成功判据：`code` 存在且 ≠ "000000" 才算业务错误
//
// 照抄客户端 `executeToolAction` 的判据（search-service.js:202）：
//
//	if (data?.code && data.code !== '000000') throw …
//
// ⚠ 实测**成功响应里根本没有 `code` 字段**（只有 Pages/Query/RequestId/
// Version/points_consumed）。所以这个判据是"只在有 code 时才检查" ——
// 写成"code 必须等于 000000 才算成功"会让每一次成功调用都被判失败。
func parseSearchPages(raw []byte) ([]searchPage, string, error) {
	var doc struct {
		Pages []map[string]any `json:"Pages"`
		// 业务错误信封（实测成功时不存在这两个字段）。
		Code    string `json:"code"`
		Message string `json:"message"`
		Desc    string `json:"desc"`
		// points_consumed 在顶层（实测）。
		PointsConsumed any `json:"points_consumed"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", err
	}
	// 与客户端同一条判据：有 code 且不是 000000 → 业务错误。
	if doc.Code != "" && doc.Code != searchOKCode {
		msg := doc.Message
		if msg == "" {
			msg = doc.Desc
		}
		if msg == "" {
			msg = "未知错误"
		}
		return nil, "", fmt.Errorf("腾讯搜索服务错误 %s: %s", doc.Code, msg)
	}

	points := ""
	if doc.PointsConsumed != nil {
		points = fmt.Sprintf("%v", doc.PointsConsumed)
	}
	out := make([]searchPage, 0, len(doc.Pages))
	for _, m := range doc.Pages {
		out = append(out, searchPage{
			Title: firstStringField(m, "title", "name"),
			URL:   firstStringField(m, "url", "link"),
			// content 是网页正文摘要；passage 是命中片段（更短更贴题），
			// 两个都在时优先给 passage 再附 content 会让条目长一倍。
			// 选 content：它是实测里内容最完整的那个字段。
			Content: firstStringField(m, "content", "passage", "summary", "text"),
			Site:    firstStringField(m, "site", "site_name", "source"),
			Date:    firstStringField(m, "date"),
		})
	}
	return out, points, nil
}

// searchOKCode 是搜索服务的成功码（照抄客户端）。
//
// ⚠ 只在响应**带 code 字段**时才有意义 —— 实测成功响应不带它。
const searchOKCode = "000000"

// firstStringField 返回 map 里第一个非空字符串字段。
func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// joinNonEmpty 用 sep 连接非空项（空项跳过，不留多余分隔符）。
func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, strings.TrimSpace(p))
		}
	}
	return strings.Join(kept, sep)
}

// markdownAltText 从提示词生成图片的 alt 文本。
//
// 取提示词前若干个字符（去掉换行）：alt 是给"图没加载出来"或读屏软件用的，
// 完整提示词太长（可能几百字），那不是 alt 该干的事。
func markdownAltText(prompt string) string {
	s := strings.Join(strings.Fields(prompt), " ")
	if len([]rune(s)) <= 40 {
		return s
	}
	return string([]rune(s)[:40]) + "…"
}

// clipForTool 按字节裁剪文本（工具结果喂回模型时的长度上限）。
//
// 与 image-tool 的裁剪同一个理由：工具结果会进模型的上下文，
// 一条超长网页正文会把上下文撑爆（而它绝大部分是导航/广告噪声）。
func clipForTool(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// 按 rune 边界截，避免切出半个 UTF-8 字符。
	cut := s[:maxBytes]
	for len(cut) > 0 && !utf8ValidTail(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// utf8ValidTail 报告 b 是否以完整 UTF-8 字符结尾。
func utf8ValidTail(b string) bool {
	for i := len(b) - 1; i >= 0 && i > len(b)-4; i-- {
		c := b[i]
		if c&0xC0 == 0x80 {
			continue // 续字节，继续往前找
		}
		switch {
		case c&0x80 == 0:
			return i == len(b)-1 // 单字节字符必须是最后一个
		case c&0xE0 == 0xC0:
			return len(b)-i == 2
		case c&0xF0 == 0xE0:
			return len(b)-i == 3
		case c&0xF8 == 0xF0:
			return len(b)-i == 4
		}
		return false
	}
	return true
}

// 编译期断言：Provider 实现对话工具扩展点。
var _ gateway.ChatToolExt = (*Provider)(nil)
