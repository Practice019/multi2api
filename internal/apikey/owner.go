// owner.go —— key 的「归属账号」判据（登录系统多用户改造的第一步）。
//
// # 要解决的问题
//
// 登录体系上线后，控制台不再只有管理员一个人：成员也能建 key（给自己那边的
// 调用方用），但**只能看到、只能管自己的**。办法不是"每人一个文件"——那会让
// 用量统计、原子写、限额判据全部复制一份，而是让**读接口带身份**：
// apikeys.json 仍是一份、Store 仍是一个，谁看得见哪几行由这里决定。
//
// # 为什么判据住在存储层，而不是 HTTP 层
//
// 放在 internal/admin 里过滤也能编译、也能跑、界面看起来也对，唯一的症状是
// "某个成员偶尔看见别人的 key" —— 一条不会自我暴露的故障。判据与数据放在
// 一起，才能钉住 owner_test.go 里那几条守门测试（尤其
// TestListForBlankOwnerIsEmpty，那是本次改造最危险的坑）。
//
// # 本包不认识 account
//
// OwnerID 是**不透明字符串**（`account.User.ID`，形如 u-xxxx）：这里既不校验
// 格式也不查账号表。与 Models 的取舍同一条理由（见 apikey.go 里 ModelAllowed
// 的注释）：本包零内部依赖，认识账号的是 HTTP 层。
package apikey

import (
	"errors"
	"strings"
	"time"
)

// CreateOwned 新建一把**归属 owner** 的 key；owner 为空 = 管理员所有。
//
// 校验与落盘口径与 Create 逐字一致（Create 就是 owner="" 的那个特例）：
//
//	limit=0 不限额度；rpm=0 不限速；models 为空则不限制模型
//
// owner 只做去空白，不校验格式：账号 ID 的合法性由 account 包负责，这里再判
// 一次只会多出第二份可能分叉的判据。
func (s *Store) CreateOwned(owner, name string, limit int64, rpm int, models []string) (*Key, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("名称不能为空")
	}
	if limit < 0 || rpm < 0 {
		return nil, errors.New("额度/限速不能为负数")
	}
	owner = strings.TrimSpace(owner)
	s.mu.Lock()
	defer s.mu.Unlock()
	k := &Key{
		ID:        newID(),
		Name:      name,
		Enabled:   true,
		Limit:     limit,
		RPM:       rpm,
		OwnerID:   owner,
		Models:    normalizeModels(models),
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	s.keys[k.ID] = k
	if err := s.saveLocked(); err != nil {
		// 落盘失败就把内存里的新记录撤回：否则会出现"这个进程里能用、
		// 重启后 key 不存在"的幽灵 key（与改造前的 Create 同一做法）。
		delete(s.keys, k.ID)
		return nil, err
	}
	return k, nil
}

// ListFor 带身份的列表视图：管理员看全部，成员只看自己名下的。
//
// # ⚠ 空 owner + 非管理员 = **空列表**（本次改造最危险的坑）
//
// 最顺手的写法是 `if k.OwnerID != owner { continue }`。当 owner 与 k.OwnerID
// **两边都是空**时它成立 —— 而空恰恰是**管理员那些 key** 的标记（见
// Key.OwnerID 的注释：老数据全都没有这个字段）。于是"未登录""会话失效"
// "身份字段没接上"等**任何**让 owner 落到空串的情况，都会把管理员的 key
// 整列表端给一个成员。
//
// 所以这里不是"比相等"，而是先判身份：非管理员必须先有 owner 才谈匹配，
// owner 为空直接返回空表。宁严勿宽 —— 多挡一次只是"列表空了"（看得见、
// 能报障），漏一次是"别人的 key 泄露"（看不见）。
//
// 过滤走 List() 的结果，不另抄一份视图构造：View 将来加字段时只需要改一处。
// 单机规模（几十把 key）多构造几行视图不值一提。
func (s *Store) ListFor(owner string, isAdmin bool) []View {
	if s == nil {
		return []View{}
	}
	if isAdmin {
		return s.List()
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return []View{}
	}
	all := s.List()
	out := make([]View, 0, len(all))
	for _, v := range all {
		if v.OwnerID == owner {
			out = append(out, v)
		}
	}
	return out
}

// OwnerOf 取一把 key 的归属账号 ID；第二个返回值是"这把 key 是否存在"。
//
// 找不到时回空串 + false —— 调用方必须用第二个返回值区分"归属为空（管理员
// 所有）"与"根本没这把 key"，只看字符串会把两者混成一个。
func (s *Store) OwnerOf(id string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return "", false
	}
	return k.OwnerID, true
}

// CanManage 判断 (owner, isAdmin) 这个身份能不能管 id 这把 key。
//
// 口径：
//
//	key 不存在         → false（虚构的 id 谁也管不了；调用方随后会拿到
//	                     ErrUnknown，不必在这里先替管理员放行）
//	isAdmin            → true（管理员管全部，含成员建的）
//	owner 为空         → false（没有身份 = 不能认领，同上一条坑）
//	OwnerID == owner   → true
//	其余               → false，**包括**"老 key 的 OwnerID 为空"这种：
//	                     空是管理员所有的标记，成员不得据此认领。
//
// 与 ListFor 同一套判据的两种形态：能看见才谈得上管（改了别人的 key
// 额度、删掉别人的 key，都是同一个越权面）。
func (s *Store) CanManage(owner string, isAdmin bool, id string) bool {
	if s == nil {
		return false
	}
	owner = strings.TrimSpace(owner)
	if !isAdmin && owner == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return false
	}
	if isAdmin {
		return true
	}
	return k.OwnerID == owner
}
