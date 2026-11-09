// codearts-doctor —— 凭证目录体检（★ T-295）
//
// **只读**：不修改、不删除任何文件，只报告。
//
// 用法:
//
//	go run ./cmd/codearts-doctor                        # 默认扫 ./auths/codearts
//	go run ./cmd/codearts-doctor -dir ./auths/codearts
//	go run ./cmd/codearts-doctor -now 2026-11-08T00:00:00+08:00   # 固定"现在"，便于复现报告
//
// # 为什么需要它
//
// 同一个账号**重复登录会留下多份凭证**（AK 每次登录都新生成，而文件名按 AK 命名）。
// 旧的、已过期的那份如果没删，任何"遍历凭证目录"的路径都会踩到它：
//
//	上游对过期 STS 返回 **403** APIGW.0301（"Incorrect IAM authentication information"），
//	而**不是** 401 —— 界面上看起来就像签名 bug，其实是脏数据。
//
// 本命令把它显式化：哪些 UID 有重复、哪份该删、每份还剩多少寿命。
package main

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"time"

	"workbuddy2api/internal/codearts"
)

func main() {
	dir := flag.String("dir", "./auths/codearts", "凭证目录（codearts-*.json）")
	nowStr := flag.String("now", "", "把\"现在\"固定成该时刻（RFC3339）")
	flag.Parse()

	now := time.Now()
	if *nowStr != "" {
		t, err := time.Parse(time.RFC3339, *nowStr)
		if err != nil {
			log.Fatalf("-now 解析失败: %v", err)
		}
		now = t
	}

	creds, err := codearts.LoadDir(*dir)
	if err != nil {
		log.Fatalf("读凭证目录 %s: %v", *dir, err)
	}
	fmt.Printf("═══ CodeArts 凭证体检 ═══\n  目录: %s\n  共 %d 份凭证\n\n", *dir, len(creds))

	byUID := map[string][]*codearts.Auth{}
	var noUID []*codearts.Auth
	for _, a := range creds {
		if a == nil {
			continue
		}
		if a.UID == "" {
			noUID = append(noUID, a)
			continue
		}
		byUID[a.UID] = append(byUID[a.UID], a)
	}
	uids := make([]string, 0, len(byUID))
	for u := range byUID {
		uids = append(uids, u)
	}
	sort.Strings(uids)

	dupN, expiredN := 0, 0
	var suggest []string
	for _, uid := range uids {
		group := byUID[uid]
		sort.SliceStable(group, func(i, j int) bool { return group[i].ExpiresAt > group[j].ExpiresAt })
		dup := len(group) > 1
		if dup {
			dupN++
		}
		head := ""
		if dup {
			head = fmt.Sprintf("   ★ 有 %d 份凭证（该留的是第 1 行）", len(group))
		}
		fmt.Printf("  UID %s%s\n", uid, head)
		for i, a := range group {
			life, expired := lifetime(a, now)
			if expired {
				expiredN++
			}
			mark := "✓"
			tail := ""
			if expired {
				mark = "✗"
				tail = "   ← 建议删除"
			} else if dup && i > 0 {
				tail = "   ← 重复（同 UID，更晚到期的那份在用它）"
			}
			name := "codearts-" + a.AccessKey + ".json"
			fmt.Printf("    %s %-40s AK=%-24s %s%s\n", mark, name, a.AccessKey, life, tail)
			if expired {
				suggest = append(suggest, filepath.Join(*dir, name))
			}
		}
		fmt.Println()
	}
	if len(noUID) > 0 {
		fmt.Printf("  ⚠ %d 份凭证没有 uid（无法按账号归并，请人工核对）\n\n", len(noUID))
	}

	fmt.Println("═══ 结论 ═══")
	fmt.Printf("  重复账号: %d 个 UID 有多份凭证\n", dupN)
	fmt.Printf("  已过期  : %d 份\n", expiredN)
	if len(suggest) == 0 {
		fmt.Println("  ✓ 没有需要删除的文件")
		return
	}
	fmt.Println("  建议删除（**本命令只报告，不删除**）：")
	for _, p := range suggest {
		fmt.Println("    " + p)
	}
	fmt.Println("\n  删除前请确认该 UID 的另一份凭证仍有效（上面 ✓ 那行）；")
	fmt.Println("  删完无需重启：凭证目录是每请求现场扫描的（Provider.localAccounts → LoadDir）。")
}

// lifetime 把"还剩多久/过期多久"说成人能一眼读懂的形式。
func lifetime(a *codearts.Auth, now time.Time) (string, bool) {
	if a.ExpiresAt <= 0 {
		return "（无过期时间）", false
	}
	left := time.Unix(a.ExpiresAt, 0).Sub(now)
	if left > 0 {
		return "剩 " + humanDur(left), false
	}
	return "★ 已过期 " + humanDur(-left), true
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return itoa(int(d.Seconds())) + " 秒"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + " 分"
	case d < 48*time.Hour:
		return itoa(int(d.Hours())) + " 小时"
	default:
		return itoa(int(d.Hours()/24)) + " 天 " + itoa(int(d.Hours())%24) + " 小时"
	}
}

// itoa 小整数字符串化（本仓 codearts 包内既有同款做法，不引 strconv 保持一致）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
