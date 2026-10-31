package gateway

import (
	"crypto/x509"
	"errors"
	"net"
	"syscall"
	"testing"
)

// timeoutErr 一个只实现 net.Error 的超时错误（模拟 http.Client 的超时）。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

// TestClassifyNetErr 每一类网络错误都要翻成「能指导动作」的类别 + 一句人话。
//
// # 这条判据守的是什么
//
// 现场原文只有一句 `net/http: TLS handshake timeout`，而它至少有三种成因
// （代理没起来 / 上游挂了 / 中间设备拦了），要用户做的事完全不同。
// 分类器把「看原文猜」变成「读类别动手」—— 所以每一类都必须有独立用例
// （少一类就会出现「其它网络错误」兜底，等于没分类）。
func TestClassifyNetErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"域名解析失败", &net.DNSError{Err: "no such host", Name: "monkeycode-ai.net"}, EgressKindDNS},
		{"端口没人听（代理没起来）", syscall.ECONNREFUSED, EgressKindRefused},
		{"包装过的拒绝连接", errors.Join(errors.New("dial tcp"), syscall.ECONNREFUSED), EgressKindRefused},
		{"TLS 握手超时（现场原文）", errors.New("Get \"https://monkeycode-ai.net/api/v1/users/wallet/checkin\": net/http: TLS handshake timeout"), EgressKindTLS},
		{"连接被重置", errors.Join(errors.New("read tcp"), syscall.ECONNRESET), EgressKindTLS},
		{"代理连接失败", errors.New("proxyconnect tcp: dial tcp 127.0.0.1:10808: connect: connection refused"), EgressKindRefused},
		{"超时", timeoutErr{}, EgressKindTimeout},
		{"证书不受信", x509.UnknownAuthorityError{}, EgressKindCert},
		{"没见过的错误（兜底不假装知道）", errors.New("something weird happened"), EgressKindNet},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, hint := ClassifyNetErr(c.err)
			if kind != c.want {
				t.Errorf("类别 = %q，want %q（错误原文：%v）", kind, c.want, c.err)
			}
			if hint == "" {
				t.Error("提示为空 —— 分类器必须给出下一步动作，否则用户还是要猜")
			}
		})
	}
	// nil 不该造出假类别（调用方按空串判「没失败」）。
	if kind, hint := ClassifyNetErr(nil); kind != "" || hint != "" {
		t.Errorf("nil 错误应回空串，实际 kind=%q hint=%q", kind, hint)
	}
}
