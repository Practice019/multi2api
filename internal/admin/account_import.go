// account_import.go 通用的「批量粘贴导入」端点。
//
// # 为什么是**一个**端点而不是每个上游一个（用户要求）
//
// 用户原话：「那个批量导入全部上游都可以制作这个功能，所以就统一一下。」
//
// 改造前只有 workbuddy 与 loomy 各写了一份 `handleImport` + 自己的
// `/admin/<id>/import` 路由，其余 7 个上游界面上根本没有这个按钮。
// 每个上游再抄一遍的话：**判据会抄多份，只有一份被改对**（本项目反复吃
// 这个亏 —— 见 `runRefresh` 那几处的注释）。
//
// 现在核心只提供一个端点，按请求体里的 `provider` 分派：
//
//	POST /admin/accounts/import
//	{"provider":"cline","data":"{…}"}        ← data 是用户粘的**原文**
//
// 落盘与池对齐都在这里做；而"那段 JSON 是什么意思"完全交给上游
// （`gateway.AccountImportExt`）—— 核心不解析、不理解凭证内容。
//
// # 为什么路由参数是 `provider` 而不是"从路径里取"
//
// 与 `/admin/accounts/quota/refresh`、`/admin/login/start` 同一条既有约定：
// 指定了上游就必须有它自己的实现，否则**明确失败**（501），不回落默认上游。
// 路由写成 `/admin/<id>/import` 会让"上游没实现"变成一个 404 ——
// 而 404 在本项目里已经被用于"账号不存在"，两种含义混在一起无法区分。
package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"workbuddy2api/internal/gateway"
)

// importMaxBytes 请求体上限。
//
// 8 MB 与既有两条导入路径一致（workbuddy / loomy 的 handleImport）——
// 用户粘几十条凭证也就几十 KB，8 MB 是给"一次导入几百个号"留的余量，
// 同时挡住"把整个 auths 目录的压缩包贴进来"这种误操作。
const importMaxBytes = 8 << 20

// accountsImport POST /admin/accounts/import —— 通用批量导入。
//
// 请求体：
//
//	{"provider":"<id>","data":"<用户粘贴的 JSON 原文>"}
//
// `data` 可以是单个对象或数组，**由上游自己决定认哪些形状**。
//
// 回执（与既有两个导入端点同形，前端不用区分）：
//
//	{ok, imported, failed, results:[{index, ok, uid?, nickname?, error?}]}
func (h *Handler) accountsImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		Data     string `json:"data"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, importMaxBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取请求体失败")
		return
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON（需要 {provider, data}）: "+err.Error())
		return
	}
	pid := strings.TrimSpace(body.Provider)
	if pid == "" {
		writeError(w, http.StatusBadRequest, "缺少 provider —— 导入必须指明是哪个上游")
		return
	}
	if strings.TrimSpace(body.Data) == "" {
		writeError(w, http.StatusBadRequest, "没有可导入的内容（data 为空）")
		return
	}

	p, ok := h.providerByID(pid)
	if !ok {
		writeError(w, http.StatusNotFound, "上游 "+pid+" 不存在（未注册或未启用）")
		return
	}
	// ⚠ 与其余分派端点同一条：**没实现就明确 501**，不回落。
	// 回落会让"导入了但进了别的上游"变成一个静默的成功。
	ext, ok := gateway.ExtOf[gateway.AccountImportExt](p)
	if !ok {
		writeError(w, http.StatusNotImplemented,
			"上游 "+pid+" 不支持批量导入（它没有声明 AccountImportExt）")
		return
	}

	creds, err := ext.ImportCredentials(body.Data)
	if err != nil {
		// 解析失败是**用户输入问题**（粘错了内容），不是服务端故障 ——
		// 400 + 原因，让界面能照实说清哪不对。
		writeError(w, http.StatusBadRequest, "导入内容无法解析："+err.Error())
		return
	}

	dir := h.credentialDirFor(pid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "创建凭证目录失败: "+err.Error())
		return
	}

	results := make([]map[string]any, 0, len(creds))
	okN := 0
	for i, c := range creds {
		res := map[string]any{"index": i + 1}
		if err := h.importOne(dir, c); err != nil {
			res["ok"] = false
			res["error"] = err.Error()
		} else {
			okN++
			res["ok"] = true
			res["uid"] = c.UID
			if c.Nickname != "" {
				res["nickname"] = c.Nickname
			}
		}
		results = append(results, res)
	}
	// ⚠ 落盘之后**必须灌池** —— 否则用户看到「导入成功 3 个」，
	// 而账号列表里一个新行都没有。
	//
	// # 这是用户报的断点（链路的最后一段）
	//
	// 本函数此前只写文件、完全不碰池。于是「批量导入」的完整体验是：
	//
	//	点导入 → 200 {imported:3} → 表格里一个新行都没有
	//
	// 用户只能再去点一次「重载 auths」。那一步可以省掉：池与文件的同步
	// 逻辑本来就在 reloadProviderInto 里，与启动时、与重载端点**共用同一份
	// 实现**（见 pool_sync.go 的注释 —— 抄第二份的代价是将来只改对一处）。
	//
	// # 为什么失败不回滚、也不 500
	//
	// 文件已经落盘了，那是既成事实。同步失败不该删文件 —— 那会让
	// 「重试导入」变成「先去目录里手工清理」。所以只把结果**如实回执**。
	sync := map[string]any{}
	if okN > 0 {
		scanned, before, after, dir, serr := h.reloadProviderInto(pid)
		if serr != nil {
			sync["error"] = serr.Error()
		} else {
			sync["scanned"] = scanned
			sync["before"] = before
			sync["after"] = after
			sync["dir"] = dir
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       okN == len(creds),
		"imported": okN,
		"failed":   len(creds) - okN,
		"results":  results,
		// sync 是"落盘后有没有真的进池"的回执（字段同名于重载端点）。
		"sync":     sync,
	})
}

// importOne 把一条上游产出的凭证落盘。
//
// # 为什么核心只做"写文件"
//
// 文件名与内容都是**上游给的**（`ImportedCredential`）—— 核心不拼、不改。
// 这样导入产出的文件与页内登录产出的**逐字节同形**，下游（LoadDir /
// 账号池对齐）不需要区分"这条是导入的还是登录来的"。
//
// ⚠ 文件名必须**不含路径分隔符**：它会与 dir 拼在一起，而 `FileName`
// 来自上游（可能用了 uid 拼接）。这里做最后一道校验 —— 上游若有 bug，
// 表现必须是"这一条导入失败"，而不是"写到了别的目录"。
func (h *Handler) importOne(dir string, c gateway.ImportedCredential) error {
	name := strings.TrimSpace(c.FileName)
	if name == "" {
		return errEmptyFileName
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return errBadFileName
	}
	if len(c.Raw) == 0 {
		return errEmptyContent
	}
	_, err := writeAuthFile(dir, name, c.Raw)
	return err
}

// （该上游的凭证目录由 `credentialDirFor` 给出 —— 见 credentialdir.go 的注释：
// 导入 / 重载 / 删除三条路径共用同一个判据，各自写一遍的代价是"将来只改对一处"。）

// 导入落盘时的三种"这条不可落盘"。分成三个值而不是一个笼统的 error：
// 用户看到的原因不同（文件名空 = 上游的 bug；内容空 = 解析出了个空凭证）。
var (
	errEmptyFileName = errors.New("落盘文件名为空（上游未给出文件名）")
	errBadFileName   = errors.New("落盘文件名含路径分隔符（拒绝写入 —— 防越出凭证目录）")
	errEmptyContent  = errors.New("落盘内容为空（解析出的凭证没有可写字段）")
)
