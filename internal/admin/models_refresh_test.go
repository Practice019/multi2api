// models_refresh_test.go —— 「刷新模型目录」这条端点的**行为**判据（含防连点）。
//
// # 为什么要防连点
//
// 用户报障是「目录不是实时的，本地缓存需要有办法更新」⇒ 修法是给一个能**穿透**
// 缓存的显式刷新。而穿透的代价是「这一下真的会去打上游」：
//
//	· 用户手快连点两下 ⇒ 两次全量上游目录拉取（实测单上游 0.2~23.7s，十二个
//	  上游逐个拉，画像见 internal/server/models_flight.go 的注释）；
//	· 面板里的探针 / 其他按钮若复用同一条端点，会变成「点一次打一轮上游」。
//
// 所以这一层必须设一个最小间隔：间隔内**不再清第二次缓存**（第二次清缓存会让
// 下一次取数再穿透一次上游 —— 那正是要被挡住的东西），并把「你现在不该再点」
// 如实告诉调用方（throttled + retry_after_ms），而不是静默成功。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// resetModelsRefreshGate 把闸门归零（每次用例从「没刷过」开始）。
//
// 它是包级状态：不清就会跨用例串（前一个用例刚刷过，后一个用例的第一次点击
// 会被判成连点，表现为莫名其妙的假红）。
func resetModelsRefreshGate() {
	modelsRefreshGate.Lock()
	modelsRefreshGate.last = time.Time{}
	modelsRefreshGate.Unlock()
}

// expireModelsRefreshGate 把「上次刷新时刻」拨到冷却窗口之外，模拟「过了一会儿又点」。
func expireModelsRefreshGate() {
	modelsRefreshGate.Lock()
	modelsRefreshGate.last = time.Now().Add(-modelsRefreshMinInterval - time.Second)
	modelsRefreshGate.Unlock()
}

func postModelsRefresh(t *testing.T, h *Handler) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localPost("/admin/models/refresh"))
	var out map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// TestModelsRefreshRouteThrottlesRapidClicks 断言「刷新穿透一次，连点不叠加」。
//
// 反例形态（这条用例存在的理由）：把 ResetModelsCache 直接挂在路由上、不设间隔 ⇒
// 两个请求 = 两次清缓存 = 两次穿透。那时 resets 会是 2，本用例当场红。
func TestModelsRefreshRouteThrottlesRapidClicks(t *testing.T) {
	resetModelsRefreshGate()
	t.Cleanup(resetModelsRefreshGate)

	resets := 0
	h := New(Config{
		AdminToken:       testAdminToken,
		ListensLoopback:  true,
		ResetModelsCache: func() { resets++ },
	})

	code, resp := postModelsRefresh(t, h)
	if code != http.StatusOK {
		t.Fatalf("第一次刷新应 200，实际 %d（body=%v）", code, resp)
	}
	if resp["reset"] != true {
		t.Fatalf("第一次刷新 reset=%v want true（正常路径必须真的清缓存）", resp["reset"])
	}
	if resets != 1 {
		t.Fatalf("第一次刷新调用 ResetModelsCache %d 次 want 1", resets)
	}

	// 立刻再点一次（连点 / 双击）：不许再清一次缓存。
	code, resp = postModelsRefresh(t, h)
	if code != http.StatusOK {
		t.Fatalf("连点应 200 + throttled（不是错误码），实际 %d body=%v", code, resp)
	}
	if resp["reset"] == true {
		t.Errorf("★ 冷却窗口内又清了一次缓存 —— 连点会反复穿透上游（上游被打爆的形态）")
	}
	if resp["throttled"] != true {
		t.Errorf("★ 被挡住时 throttled=%v want true —— 静默成功会让界面以为这次刷新生效了，"+
			"而实际什么都没做", resp["throttled"])
	}
	if resets != 1 {
		t.Errorf("★ 连点穿透到上游：ResetModelsCache 被调 %d 次 want 1", resets)
	}
	if ms, ok := resp["retry_after_ms"].(float64); !ok || ms <= 0 || ms > float64(modelsRefreshMinInterval.Milliseconds()) {
		t.Errorf("retry_after_ms=%v want (0, %d]（要给调用方一个还能等多久的确定数字）",
			resp["retry_after_ms"], modelsRefreshMinInterval.Milliseconds())
	}

	// 过了冷却窗口 ⇒ 恢复成「真的刷」
	expireModelsRefreshGate()
	code, resp = postModelsRefresh(t, h)
	if code != http.StatusOK || resp["reset"] != true {
		t.Fatalf("冷却过后应恢复刷新，实际 code=%d resp=%v", code, resp)
	}
	if resets != 2 {
		t.Errorf("冷却过后 ResetModelsCache 被调 %d 次 want 2", resets)
	}
}

// TestModelsRefreshThrottleControl 反向验证：**没有**闸门时同样的两个请求会穿透两次。
//
// # 为什么要有这条（它不是换个写法重复上面那条）
//
// 上面那条断言「两次点击只清一次缓存」。若把 ResetModelsCache 换成一个**只生效一次**
// 的实现（或路由上加了别的去重），那条断言照样绿 —— 但它证明不了「被挡住的是第二次点击」。
// 这条把冷却窗口拨过去（= 模拟没有闸门），同样的动作序列必须穿透**两次** ⇒
// 两条合起来才说明：每一次被接受的请求都会真的清缓存，而连点是被闸门挡下的。
func TestModelsRefreshThrottleControl(t *testing.T) {
	resetModelsRefreshGate()
	t.Cleanup(resetModelsRefreshGate)

	resets := 0
	h := New(Config{
		AdminToken:       testAdminToken,
		ListensLoopback:  true,
		ResetModelsCache: func() { resets++ },
	})
	if _, resp := postModelsRefresh(t, h); resp["reset"] != true {
		t.Fatalf("第一次应真的清缓存，实际 %v", resp)
	}
	expireModelsRefreshGate() // ⚠ 拨过去 = 假装没有闸门
	if _, resp := postModelsRefresh(t, h); resp["reset"] != true {
		t.Fatalf("冷却过后的第二次点击应真的清缓存，实际 %v", resp)
	}
	if resets != 2 {
		t.Errorf("穿透次数=%d want 2 —— 说明 ResetModelsCache 并不是「每次被接受就调一次」，"+
			"那上面那条「连点只 1 次」就无法归因到闸门上", resets)
	}
}

// TestModelsRefreshRouteWithoutWiring 未接线时保持既有降级（501 + 中文原因），
// 且**不得**因为限速闸门先判而后退化成看起来很成功。
func TestModelsRefreshRouteWithoutWiring(t *testing.T) {
	resetModelsRefreshGate()
	t.Cleanup(resetModelsRefreshGate)
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true})
	code, _ := postModelsRefresh(t, h)
	if code != http.StatusNotImplemented {
		t.Errorf("未接线时应 501，实际 %d", code)
	}
}
