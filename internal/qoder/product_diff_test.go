package qoder

import "testing"

// TestQoderCNDiffersFromIntlEndpoints 中国版**必须**有独立的端点与 clientId。
//
// # 这条修的是一个真实的移植错误（我自己造成的）
//
// 我第一版移植 QoderCN 时，看到参照注释说「CN 没有可用的公开端点」，
// 就顺手把国际版的四个端点与 clientId 全抄了一遍 —— 只改了 ID 与显示名，
// 使 QoderCN 变成"同一个服务的另一张模型表"。
//
// 参照的 CN 是**独立的一份配置**（qoder-product.ts:433-482），四个值全不同：
//
//	authBase            qoder.cn              vs qoder.com
//	openApiBase         openapi.qoder.com.cn  vs openapi.qoder.sh
//	encryptedInferBase  gateway.qoder.com.cn  vs api2.qoder.sh
//	clientId            732aef47-…            vs e883ade2-…
//
// # 为什么必须有测试（这个错误无法靠"跑一下"发现）
//
// 参照对 clientId 的注释原文：
//
//	用错的症状是「授权页 302 正常、点击授权后报参数无效」，
//	故**不能**靠探测入口验证，必须真实登录闭环
//
// 也就是说：写下错的 clientId 后，授权页照常打开、state 照常返回 ——
// 一切探测都是绿的，只有用户真的点了授权才失败。**这类错误只能靠
// 逐字对照配置来防**，所以这里把它逐字钉住。
func TestQoderCNDiffersFromIntlEndpoints(t *testing.T) {
	cases := []struct {
		name string
		intl string
		cn   string
	}{
		{"AuthBase", Qoder.AuthBase, QoderCN.AuthBase},
		{"OpenAPIBase", Qoder.OpenAPIBase, QoderCN.OpenAPIBase},
		{"EncryptedInferBase", Qoder.EncryptedInferBase, QoderCN.EncryptedInferBase},
		{"ClientID", Qoder.ClientID, QoderCN.ClientID},
	}
	for _, c := range cases {
		if c.intl == c.cn {
			t.Errorf("CN 的 %s 与国际版相同（都是 %q）—— "+
				"中国版是**另一个服务**，不是同一端点的另一张模型表；"+
				"照着国际版抄会让 qodercn 加不上号（授权页正常、点授权后参数无效）",
				c.name, c.intl)
		}
	}
}

// TestQoderCNExactValues 逐字对照参照 qoder-product.ts:433-482 的 CN 取值。
//
// 这些值是**协议事实**，不是我们自己定的 —— 参照从 CN asar 与
// endpoint-cache.json 里挖出来的（注释里的 E2/E4/E9/E10/E11 是设计文档编号）。
//
// ⚠ 改任何一个都必须有新的实测依据，并同步更新这里的期望值。
func TestQoderCNExactValues(t *testing.T) {
	want := map[string]struct{ got, want string }{
		"AuthBase":           {QoderCN.AuthBase, "https://qoder.cn"},
		"OpenAPIBase":        {QoderCN.OpenAPIBase, "https://openapi.qoder.com.cn"},
		"EncryptedInferBase": {QoderCN.EncryptedInferBase, "https://gateway.qoder.com.cn"},
		"ClientID":           {QoderCN.ClientID, "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa"},
		"TestClientID":       {QoderCN.TestClientID, "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa"},
		"SashClientType":     {QoderCN.SashClientType, "10"},
		"UserAgentPrefix":    {QoderCN.UserAgentPrefix, "qoder"},
	}
	for name, w := range want {
		if w.got != w.want {
			t.Errorf("%s = %q，want %q（参照 qoder-product.ts 的 CN 取值）", name, w.got, w.want)
		}
	}
}

// TestQoderCNTestClientIDSameAsProd CN 的 test 与 prod clientId 是**同一个值**。
//
// 参照注释：「CN 的 `authClientIds.test` 与 `prod` **同一个值**，因此不存在
// 国际版 `J_a` / `G_a` 被读反的那类风险」。
//
// 而国际版两个值**不同** —— 所以"两边都填一样"这个巧合只在 CN 成立，
// 不能反过来推导国际版也可以填一样。
func TestQoderCNTestClientIDSameAsProd(t *testing.T) {
	if QoderCN.TestClientID != QoderCN.ClientID {
		t.Errorf("CN 的 test 与 prod clientId 应相同：test=%q prod=%q",
			QoderCN.TestClientID, QoderCN.ClientID)
	}
	// 国际版则必须不同（否则说明有人"统一"了两个值，那是错的）。
	if Qoder.TestClientID == Qoder.ClientID {
		t.Error("国际版的 test 与 prod clientId 应不同 —— " +
			"把两者统一会掩盖参照记录的 J_a/G_a 读反风险")
	}
}

// TestQoderCNSashBaseIsCN CN 的 sash 基址随产品走（qoder.cn）。
func TestQoderCNSashBaseIsCN(t *testing.T) {
	if QoderCN.SashBase == Qoder.SashBase {
		t.Errorf("CN 的 SashBase 与国际版相同（%q）—— 它应随产品走", QoderCN.SashBase)
	}
	if QoderCN.SashBase != "https://qoder.cn" {
		t.Errorf("QoderCN.SashBase = %q，want https://qoder.cn", QoderCN.SashBase)
	}
}

// TestQoderProductsDistinguishable 两个产品在**标识层**可区分（供界面分组）。
//
// ID / 显示名 / 凭证 ref 都不同，所以界面能把它们分开渲染。
func TestQoderProductsDistinguishable(t *testing.T) {
	if Qoder.ID == QoderCN.ID {
		t.Fatal("两个产品的 ID 相同 —— 注册表会拒绝或互相覆盖")
	}
	if Qoder.DisplayName == QoderCN.DisplayName {
		t.Errorf("两个产品的显示名相同（%q）—— 用户在界面上分不出该点哪个",
			Qoder.DisplayName)
	}
	// 显示名要能让人看出"这是中国版"。
	if !containsCN(QoderCN.DisplayName) {
		t.Errorf("中国版显示名 %q 里没有「中国版」字样 —— "+
			"用户在国际版与中国版之间无法分辨", QoderCN.DisplayName)
	}
}

func containsCN(s string) bool {
	for i := 0; i+9 <= len(s); i++ {
		if s[i:i+9] == "中国版" {
			return true
		}
	}
	return false
}

// TestProviderDisplayNameDistinguishesInstances 两个实例的展示名要能区分。
//
// 界面按 display_name 渲染分组标题（gateway.DisplayNameExt）。
// 若两个实例报同一个名字，用户在国际版与中国版之间仍然分不出。
func TestProviderDisplayNameDistinguishesInstances(t *testing.T) {
	intl := NewWithConfig(Config{})
	cn := NewWithConfig(Config{Product: QoderCN})

	ni, nc := intl.DisplayName(), cn.DisplayName()
	if ni == "" || nc == "" {
		t.Fatalf("展示名不得为空：intl=%q cn=%q", ni, nc)
	}
	if ni == nc {
		t.Errorf("两个实例的展示名相同（都是 %q）—— "+
			"界面上就分不出该点哪个", ni)
	}
	if !containsCN(nc) {
		t.Errorf("中国版展示名 %q 里没有「中国版」字样", nc)
	}
}

// TestProviderDisplayNameFollowsProductTable 展示名取自产品表（同源，不另写一份）。
func TestProviderDisplayNameFollowsProductTable(t *testing.T) {
	cn := NewWithConfig(Config{Product: QoderCN})
	if got, want := cn.DisplayName(), QoderCN.DisplayName; got != want {
		t.Errorf("DisplayName = %q，want %q（产品表的 DisplayName）—— "+
			"另写一份会让两处漂移", got, want)
	}
}
