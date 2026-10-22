// region_live_test.go 真机验证：国内站与国际站**各自走对的网络出口**。
//
// # 这条测试守的是什么
//
// 两站的网络前提**相反**，而配错的表现极具误导性：
//
//	国内站走了代理  → POST 直接 EOF → 核心报「账号不健康」
//	国际站不走代理  → 连不上（同 EOF）
//
// 两者都表现为"账号坏了"，而实际是**出口选错**。所以这条测试
// 不只看"有没有返回"，而是**分别验证两个区域都能打通** ——
// 只测一个区域时，把另一个也配上代理（或全直连）都能让测试绿。
package monkeycode

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestLiveBothRegions 两个区域的账号都拉一次额度 + 目录。
//
// 需要 auths/monkeycode/ 里同时有 cn 与 intl 的凭证，否则各自 Skip。
func TestLiveBothRegions(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	dir := liveAuthDir()
	list, err := LoadDir(dir)
	if err != nil || len(list) == 0 {
		t.Skipf("无真实凭证（%s），跳过", dir)
	}

	byRegion := map[string]*Auth{}
	for _, a := range list {
		if !a.ChatUsable() {
			continue
		}
		if _, exists := byRegion[a.RegionOrDefault()]; !exists {
			byRegion[a.RegionOrDefault()] = a
		}
	}
	if len(byRegion) == 0 {
		t.Skip("没有同时具备 session 与 api_key 的凭证")
	}

	// ⚠ 国际站出口取自环境变量（本机代理），国内站恒直连 ——
	// 这正是生产配置的形态（config 里 monkeycode.proxy 只影响国际站）。
	proxy := os.Getenv("MONKEYCODE_TEST_PROXY")
	p := New(Config{AuthDir: dir, Proxy: proxy})

	for region, a := range byRegion {
		a := a
		t.Run(region, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			// ① 模型目录（走 host，Cookie 认证）
			models, err := p.client.FetchModels(ctx, a)
			if err != nil {
				t.Fatalf("[%s] 拉模型目录失败: %v", region, err)
			}
			if len(models) == 0 {
				t.Fatalf("[%s] 模型目录为空", region)
			}
			t.Logf("[%s] 模型 %d 条（host=%s）", region, len(models), a.Host())

			// ② 额度（走 host）
			wallet, err := p.client.FetchWallet(ctx, a)
			if err != nil {
				t.Fatalf("[%s] 拉额度失败: %v", region, err)
			}
			t.Logf("[%s] balance=%d daily=%d/%d", region, wallet.Balance,
				wallet.DailyTokenBalance, wallet.DailyTokenLimit)

			// ③ 对话（走 proxyBase，Bearer + 签名）—— 这一步才真正验证网关侧
			model := pickModel(models)
			body := []byte(`{"model":"` + model + `","stream":true,
				"messages":[{"role":"user","content":"只回答两个字：收到"}]}`)
			stream, err := p.Chat(ctx, gateway.Credential{Secret: a}, body)
			if err != nil {
				t.Fatalf("[%s] Chat 失败: %v", region, err)
			}
			defer stream.Body.Close()
			if stream.Status != 200 {
				raw, _ := io.ReadAll(io.LimitReader(stream.Body, 400))
				t.Fatalf("[%s] Chat HTTP %d: %s", region, stream.Status, raw)
			}
			got, _ := io.ReadAll(io.LimitReader(stream.Body, 32*1024))
			if !bytes.Contains(got, []byte("[DONE]")) {
				t.Errorf("[%s] 流未以 [DONE] 收尾", region)
			}
			t.Logf("[%s] Chat OK，模型 %s，%d 字节 SSE", region, model, len(got))
		})
	}
}

// pickModel 从目录里挑一个**能对话的**模型。
//
// 优先 basic 档：pro/ultra 对 basic 号一律 403，拿它们测会把
// "档位不够"误报成"上游不通"。
func pickModel(models []ModelEntry) string {
	for _, want := range []string{
		"monkeycode-basic/qwen3.8-flash",
		"monkeycode-basic/deepseek-flash",
		"monkeycode-basic/qwen3.5-plus",
	} {
		for _, m := range models {
			if m.Model == want {
				return want
			}
		}
	}
	for _, m := range models {
		if m.AccessLevel == "basic" && !m.IsHidden {
			return m.Model
		}
	}
	_ = filepath.Join
	return models[0].Model
}
