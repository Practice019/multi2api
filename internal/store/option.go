// option.go options 表：读、写、以及从 config.json 的**一次性**导入。
//
// # options 与 config.json 的分工（PLAN-sql-unify.md 的 D3）
//
//	读路径  SQL 是唯一权威
//	写路径  SQL 事务
//	JSON    仅用于 ① 首次导入 ② 导出备份 ③ 人工排查
//
// 于是导入的语义**必须**是"键不存在才写"（见 ImportFromConfigJSON）。
// 反过来说：如果每次启动都用 config.json 覆盖 options，那么界面上改过的值
// 会在他下次重启时被悄悄冲回去，而他会以为"界面保存没生效"。
//
// # 为什么这些函数都直接返回错误而不是记日志就过
//
// 这一层没有"失败了还能继续"的场景：options 读不出来时上层只能回落默认值，
// 而**回落**这个决定该由上层做（它知道默认值是什么）。这里吞掉错误等于把
// "库坏了"变成"配置看起来是空的"，那是最难查的一种故障。
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// optionMaxKeyLen / optionMaxValueLen 与 models.go 的 Option 列宽一致。
//
// 超长**显式拒绝**而不是让 SQLite 静默截断：截断后的值仍是一个"能读到的值"，
// 于是"我配了 5000 字符的备注，结果只剩一半"会变成一个没人查得动的现象。
const (
	optionMaxKeyLen   = 64
	optionMaxValueLen = 4096
)

// ErrBadOption 键不是合法的 option key 形态，或长度超限。
var ErrBadOption = errors.New("store: 非法的 option 键值")

// LoadOptions 读出全部键值。
//
// 返回值是**副本**：调用方拿到 map 后可以随意改（比如传进模板），不会污染
// 任何人。空表返回空 map 而不是 nil —— nil map 与"没有配置"在调用方看来
// 应该是一回事，而 nil 在写入（m[k]=v）时直接 panic。
func LoadOptions() (map[string]string, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	var rows []Option
	if err := DB.Order("key").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: 读 options: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

// PutOption 写一条（存在则改值）。
//
// # 为什么用 upsert 而不是"先查再决定 Create/Update"
//
// 先查再写是两个语句、不是原子的：两个并发写者会双双查到"不存在"，然后一个
// 成功、一个撞主键报错。upsert（ON CONFLICT DO UPDATE）把这件事交给库自己，
// 一条语句、天然原子。与 setup.go 的 upsertOptionTx 共用同一段 clause，
// 区别只在这里传的是包级 DB（不在别人的事务里）。
func PutOption(key, value string) error {
	if DB == nil {
		return ErrNotOpen
	}
	if err := validateOptionKey(key); err != nil {
		return err
	}
	if len(value) > optionMaxValueLen {
		return fmt.Errorf("%w：值长度 %d 超过上限 %d", ErrBadOption, len(value), optionMaxValueLen)
	}
	return upsertOptionTx(DB, key, value)
}

// OptionPair 一条 option 的键与值。
type OptionPair struct {
	Key   string
	Value string
}

// PutOptions 在一个**事务**里写多条（存在则改值）。

// # 为什么不循环调 PutOption

// 设置页一次保存会写十几条（见 cmd/server 的受管清单）。逐条写的话，中途一条失败
// 会留下「一半新值、一半旧值」的 options —— 读侧看到的是一份自相矛盾的配置，
// 而且没有任何报错指向它。事务保证要么全上、要么全不上。
//
// # 校验放在开事务之前

// 键形态与值长度先**全部**校验完再进事务：非法输入不该先写进去一半再回滚
// （回滚是最后一道保险，不是第一道）。
func PutOptions(pairs []OptionPair) error {
	if DB == nil {
		return ErrNotOpen
	}
	if len(pairs) == 0 {
		return nil
	}
	for _, p := range pairs {
		if err := validateOptionKey(p.Key); err != nil {
			return err
		}
		if len(p.Value) > optionMaxValueLen {
			return fmt.Errorf("%w：值长度 %d 超过上限 %d", ErrBadOption, len(p.Value), optionMaxValueLen)
		}
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		for _, p := range pairs {
			if err := upsertOptionTx(tx, p.Key, p.Value); err != nil {
				return err
			}
		}
		return nil
	})
}

// OptionExists 报告某个键在库里有没有（导入时用它决定"跳过还是写"）。
//
// 单独暴露是因为"存在性"与"值"是两件事：导入要判断的是前者，而读值会得到
// 空串 —— 一个合法的空串配置（比如把公告清空）与"没有这个键"在
// LoadOptions 的返回值上无法区分。
func OptionExists(key string) (bool, error) {
	if DB == nil {
		return false, ErrNotOpen
	}
	var n int64
	if err := DB.Model(&Option{}).Where("key = ?", key).Count(&n).Error; err != nil {
		return false, fmt.Errorf("store: 查 option %q: %w", key, err)
	}
	return n > 0, nil
}

// validateOptionKey 键形态：非空、不超长、不含空白。
//
// 不含空白是刻意的：键会出现在日志、URL 查询串、前端表单里，一个带空格或
// 换行的键会让"复制这个键去搜日志"变成一件做不到的事。
func validateOptionKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w：键不能为空", ErrBadOption)
	case len(key) > optionMaxKeyLen:
		return fmt.Errorf("%w：键长度 %d 超过上限 %d", ErrBadOption, len(key), optionMaxKeyLen)
	case strings.IndexFunc(key, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) >= 0:
		return fmt.Errorf("%w：键不能含空白", ErrBadOption)
	}
	return nil
}

// ImportFromConfigJSON 把 config.json 里**可热改**的键首次导入 options。
//
// 返回**本次新写入的行数**（不是文件里的键数）。这个区别就是验收口径：
// 二次调用必须返回 0 —— 它比"行数不变"更能证明"一个键都没被重写"。
//
// # "键不存在才写"的具体做法
//
// 每个键一条 INSERT ... ON CONFLICT(key) DO NOTHING，RowsAffected 累加就是
// "新增了几条"。不用先 SELECT 全表再比对：那在文件很大时会读全表，而且两个
// 进程同时导入时仍会双双判"不存在"。让库来判冲突，一条语句就够。
//
// # 文件不存在不是错误
//
// 首次部署时 config.json 可能还没建（程序走全默认值）。这种情况返回 (0, nil)：
// 调用方不需要为"没配置"写一个分支，而且"文件不存在"与"文件里有 0 个可导入键"
// 对结果的影响完全相同。
//
// # 哪些键**不**导入
//
// 见 importSkipKeyNames / importSkipKeyPrefixes 的注释。一句话：凭据与服务地址
// 不进 options。options 会被整张表读出来渲染到设置页，也会被导出进备份 ——
// 把 api_key / admin_token 放进去等于给它们再造两份副本。
func ImportFromConfigJSON(path string) (int, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	if strings.TrimSpace(path) == "" {
		return 0, fmt.Errorf("%w：配置文件路径为空", ErrBadOption)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: 读配置 %s: %w", path, err)
	}
	flat, err := flattenConfigJSON(raw)
	if err != nil {
		return 0, fmt.Errorf("store: 解析配置 %s: %w", path, err)
	}

	// 排序后写入：同一份文件重复导入时的插入顺序稳定，排查时 diff 干净。
	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	imported := 0
	err = DB.Transaction(func(tx *gorm.DB) error {
		for _, k := range keys {
			v := flat[k]
			if len(k) > optionMaxKeyLen || len(v) > optionMaxValueLen {
				// 超长键值**跳过而不是整批失败**：一个写错的备注字段不该
				// 让整份配置导不进来（那是"一个字段毁掉一个部署"）。
				continue
			}
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).
				Create(&Option{Key: k, Value: v})
			if res.Error != nil {
				return fmt.Errorf("store: 导入 option %q: %w", k, res.Error)
			}
			imported += int(res.RowsAffected)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return imported, nil
}

// importSkipKeyNames 精确跳过的键（按**最后一段**比较，大小写不敏感）。
//
// 除了凭据，state_file / auth_dir 也在里面：它们是**部署参数**不是热改配置 ——
// 运行期改它们只会让"进程读的是旧路径、界面显示的是新路径"，而它们一旦指错，
// 表现是"账号全丢了"，属于最不该被远程改动的两类值。
var importSkipKeyNames = map[string]bool{
	"api_key":     true,
	"admin_token": true,
	"auth_dir":    true,
	"state_file":  true,
	"token":       true,
	"secret":      true,
	"password":    true,
	"passwd":      true,
	"key":         true,
}

// importSkipKeyPrefixes 这些前缀的键**永不**导入 options（安全取舍）。
//
// 判据刻意宽松（宁可多跳）：多跳一个键的代价是"设置页少显示一项"，而漏跳
// 一个的代价是"凭据被写进 options 表、又被导出进备份"。
var importSkipKeyPrefixes = []string{"api_key", "admin_token", "token", "secret", "password", "passwd"}

// flattenConfigJSON 把 JSON 摊平成 "a.b.c" -> 标量字符串。
//
// # 为什么是"摊平"而不是把整份 JSON 存成一个 option
//
// 因为 options 的用途是"按名读一个值"（设置页一项一项读、代码一处一处取）。
// 整份 JSON 存成一个值等于把 config.json 原样搬进库里，那样"改一项要读改写
// 整份文档"，并发下必然丢更新 —— 而这正是要搬到 SQL 的理由。
//
// # 嵌套对象怎么表示
//
// 用点号路径（"upstream.timeout_seconds"）。数组用**紧凑 JSON 字符串**表示
// （"schedule.activity_hours" -> "[9,21]"）：转成逗号串会丢掉"元素是数字还是
// 字符串"这条信息，而 []int 的解析要靠它。
//
// null 直接跳过：JSON 里 null 的语义是"没写"，写成字符串 "null" 会让
// "未配置"变成一个看起来像值的东西（config.json 的 schedule.checkin_hours
// 就是这样）。
func flattenConfigJSON(raw []byte) (map[string]string, error) {
	stripped := stripJSONComments(raw)
	dec := json.NewDecoder(bytes.NewReader(stripped))
	// UseNumber：默认解成 float64，于是大整数会被静默改值，
	// 而且 int 值回填时会变成 "1.2e+02" 这种样子，不再是原来的字面量。
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	out := make(map[string]string)
	flattenInto(out, "", root)
	return out, nil
}

// flattenInto 递归摊平。prefix 为空表示根。
func flattenInto(out map[string]string, prefix string, node any) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flattenInto(out, key, child)
		}
	case []any:
		// 空数组也写（"[]"）：它表达"我明确配了空列表"，与"没配"不同。
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		if key := importableKey(prefix); key != "" {
			out[key] = string(b)
		}
	case nil:
		// null = 没配，跳过（见 flattenConfigJSON 的注释）。
	case string:
		if key := importableKey(prefix); key != "" {
			out[key] = v
		}
	case json.Number:
		if key := importableKey(prefix); key != "" {
			out[key] = v.String()
		}
	case bool:
		if key := importableKey(prefix); key != "" {
			out[key] = strconv.FormatBool(v)
		}
	}
}

// importableKey 判定一个摊平后的路径该不该进 options；不该进返回空串。
//
// 判据看**最后一段**（"pool.breaker_threshold" 看 "breaker_threshold"）：
// 凭据味道是键名自己的性质，不是它所在分组的性质 —— 反过来按前缀判会把
// 整个 "upstream" 分组跳掉，那里面全是正常配置。
func importableKey(path string) string {
	if path == "" {
		return ""
	}
	last := path
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		last = path[i+1:]
	}
	low := strings.ToLower(last)
	if importSkipKeyNames[low] {
		return ""
	}
	for _, p := range importSkipKeyPrefixes {
		if strings.HasPrefix(low, p) {
			return ""
		}
	}
	return path
}

// stripJSONComments 去掉 // 与 /* */ 注释，**字符串内部不动**。
//
// # 为什么 store 需要自己实现一份（cmd/server 里已经有一份 JSONC 解析）
//
// 因为 cmd/server 是 package main，store import 不到它；而 store 也**不该**
// 反向依赖装配层（那是依赖倒置）。这里只需要"能把配置读成 map"这一件事，
// 不需要完整的 JSONC 能力。
//
// # 为什么不能"先按行剔注释再解析"
//
// 字符串里完全可能含双斜杠，例如 proxy 那一项的值就是
// "http://127.0.0.1:10808"（本仓的 config.json 里真的有）。按行剔注释会把
// 那个值整段切掉，而且解析仍然成功 —— 于是代理配置变成空串，现象是
// "某几个上游突然连不上"，与配置解析毫无关系。所以这里走的是状态机：
// 只有在字符串**之外**才认注释。
func stripJSONComments(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	inString := false
	escaped := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(raw) && (raw[i+1] == '/' || raw[i+1] == '*') {
			// 注释**前面**的空白是「给注释留的位置」：注释被删掉之后它就成了
			// 一段多余的空格（`{ // note` 会留下 `{ `，`{ /* a` 会留下 `{  `）。
			// 这里把它一并去掉 —— 但**只去空格与制表符，不碰换行**：
			// 换行是留给报错行号的（见下面「保留换行」那句），去掉它会让
			// 解析报错的行号与用户文件里的行号对不上。
			for len(out) > 0 && (out[len(out)-1] == ' ' || out[len(out)-1] == '\t') {
				out = out[:len(out)-1]
			}
			if raw[i+1] == '/' {
				for i < len(raw) && raw[i] != '\n' {
					i++
				}
				// 保留换行：解析报错时行号仍然有意义。
				if i < len(raw) {
					out = append(out, '\n')
				}
				continue
			}
			i += 2
			for i+1 < len(raw) && !(raw[i] == '*' && raw[i+1] == '/') {
				i++
			}
			i++ // 跳过结尾的 '/'
			continue
		}
		out = append(out, c)
	}
	return out
}
