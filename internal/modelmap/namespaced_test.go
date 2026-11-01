package modelmap

import "testing"

// TestNamespaced 渠道名 + 上游名 → 公开名。
//
// 这条规则是用户报的 bug（目录里出现没有上游前缀的模型）的修法本体：
// 渠道声明了模型但没写 model_mapping 时，公开名必须是 <渠道名>/<上游名>，
// 不能把上游原名原样暴露出去。
func TestNamespaced(t *testing.T) {
	cases := []struct{ channel, upstream, want string }{
		{"csg", "deepseek-v4.1-flash-1017", "csg/deepseek-v4.1-flash-1017"},
		// 上游名自己带厂商前缀 ⇒ 只拼一层命名空间，别把厂商前缀当第二层。
		{"七牛云", "deepseek/deepseek-v4.1-flash", "七牛云/deepseek/deepseek-v4.1-flash"},
		// 已经是本渠道命名空间下的名字 ⇒ 不重复拼（否则会出 a/a/model）。
		{"csg", "csg/deepseek-v4.1-flash-1017", "csg/deepseek-v4.1-flash-1017"},
		// 渠道名缺失 ⇒ 原样返回（不能造出 "/model" 这种坏名字）。
		{"", "glm-5.2", "glm-5.2"},
		{"csg", "", ""},
	}
	for _, c := range cases {
		if got := Namespaced(c.channel, c.upstream); got != c.want {
			t.Errorf("Namespaced(%q, %q) = %q, want %q", c.channel, c.upstream, got, c.want)
		}
	}
}

// TestSplitNamespaced 反解：只认**自己**的命名空间，且只剥一层。
//
// 它与 Namespaced 必须互逆：目录侧拼出来、路由侧解回去，两侧不同规则就会
// "目录里看得见、发请求却 400"（channelroute 文件头写着"与目录侧对称，这是关键"）。
func TestSplitNamespaced(t *testing.T) {
	// 命中：剥掉自己的一层
	if got, ok := SplitNamespaced("csg", "csg/deepseek-v4.1-flash-1017"); !ok || got != "deepseek-v4.1-flash-1017" {
		t.Errorf("SplitNamespaced 命中失败: (%q, %v)", got, ok)
	}
	// 含厂商前缀的上游名：只剥一层（厂商前缀属于上游名）
	if got, ok := SplitNamespaced("七牛云", "七牛云/deepseek/deepseek-v4.1-flash"); !ok || got != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("SplitNamespaced 多剥了一层: (%q, %v)", got, ok)
	}
	// 别人的命名空间 ⇒ 不命中（否则 csg 会去认领 七牛云 的模型）
	if got, ok := SplitNamespaced("csg", "七牛云/deepseek-v4.1-flash"); ok || got != "" {
		t.Errorf("SplitNamespaced 不该认领别的渠道: (%q, %v)", got, ok)
	}
	// 裸名 ⇒ 不命中（裸名走 legacy 默认上游那条路，不由渠道认领）
	if got, ok := SplitNamespaced("csg", "deepseek-v4.1-flash-1017"); ok || got != "" {
		t.Errorf("SplitNamespaced 不该认领裸名: (%q, %v)", got, ok)
	}
	// 边界：空渠道名 / 空模型 / 只有前缀没有模型
	for _, c := range []struct{ channel, name string }{{"", "csg/x"}, {"csg", ""}, {"csg", "csg/"}} {
		if got, ok := SplitNamespaced(c.channel, c.name); ok || got != "" {
			t.Errorf("SplitNamespaced(%q,%q) 应当不命中，得到 (%q,%v)", c.channel, c.name, got, ok)
		}
	}
}

// TestNamespacedSplitRoundTrip 互逆性（拼出来的一定能解回去）。
func TestNamespacedSplitRoundTrip(t *testing.T) {
	for _, c := range []struct{ channel, upstream string }{
		{"csg", "deepseek-v4.1-flash-1017"},
		{"七牛云", "deepseek/deepseek-v4.1-flash"},
		{"seed渠道", "ch-only-model"},
	} {
		pub := Namespaced(c.channel, c.upstream)
		got, ok := SplitNamespaced(c.channel, pub)
		if !ok || got != c.upstream {
			t.Errorf("往返不一致: channel=%q upstream=%q pub=%q 回解=(%q,%v)", c.channel, c.upstream, pub, got, ok)
		}
	}
}
