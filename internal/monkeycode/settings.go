// settings.go 本上游的**设置页字段**：出口（代理）配置。
//
// # 为什么必须有它（用户报障）
//
// 用户原话：「这个代理的配置是写死的吧？设置里面都设置不了。有些东西不能写死了，
// 要放在设置里面让用户自己去设置，因为每个人都不一样。」
//
// 事实是：代理**有**配置键（config.json 的 monkeycode.proxy / monkeycode.direct_proxy），
// 但设置页没有入口 —— 用户只能去手改 config.json 再重启。而出口地址天然是
// 每台机器各不相同的（本地代理端口、公司网关、直连……），把它锁在文件里等于把
// 「改一个只有本机才知道的值」变成了运维动作。
//
// 实现的是 admin 的 SettingsExt 契约（上游自报字段，核心不认识具体键名）。
// 与 workbuddy 的 growth/travel 开关同一模式，区别只有一处：
// **它们是热生效的**（RestartRequired=false）——因为出口只是一个 http.Client 的
// transport，换掉它不需要重启；而「改完要重启」在这种场景下最容易让人以为没生效
// （改完立刻能用「出口自检」验证，才是这个旋钮该有的体验）。
package monkeycode

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// 设置页的键名。
//
// ⚠ 它们同时是 config.json 里 monkeycode 段的键名（见 cmd/server 的 Config.MonkeyCode），
//
//	所以**不许改名**：改了等于让老部署里手写的值失效。
const (
	// KeyProxy 国际站出口（monkeycode-ai.net）。**空 = 直连（常态）**；填了才走代理。
	//
	// ⚠ 2026-11-01 更正：「国际站必须走它」是错误前提 —— 国际站多数环境可直连
	// （见 egress.go 头注的实测数据）。代理只在本机直连不通时才需要。
	KeyProxy = "monkeycode_proxy"
	// KeyDirectProxy 国内站出口（默认空 = 直连；只有「本机无法直连国内站」时才该设）。
	KeyDirectProxy = "monkeycode_direct_proxy"
)

// SettingField 一个设置项的对外形状（与 admin.SettingField 同形）。
//
// 本包**不 import admin**：上游只提供一份形状，宿主那边有适配器转成 admin 的类型
// （见 cmd/server 的 upstream adapter）。这样上游包的依赖面里没有管理面。
type SettingField struct {
	Key             string
	Value           any
	RestartRequired bool
}

// SettingsFields 当前出口值（顺序稳定：代理在前、强制国内站代理在后）。
func (p *Provider) SettingsFields() []SettingField {
	proxy, direct := "", ""
	if p != nil && p.client != nil {
		proxy, direct = p.client.ProxyConfig()
	}
	return []SettingField{
		{Key: KeyProxy, Value: proxy, RestartRequired: false},
		{Key: KeyDirectProxy, Value: direct, RestartRequired: false},
	}
}

// ApplySettings 应用补丁里属于本上游的键（不认得的忽略）。
//
// # 为什么整份补丁给我，而不是预先切好的
//
// 与其余上游同一条理由：切分需要核心知道键的归属，那就等于核心认识上游字段了。
//
// # 校验（用户填错要让他在保存那一刻就知道）
//
// 空串 = 直连（合法）。非空必须是 http/https 且带主机 —— 允许省略 scheme（补 http://）。
// 不校验的话，写错一个字符的表现是「所有国际站请求都超时」，而错误文案里那句
// 「代理 http://…」看起来完全正常，用户会去查上游而不是查自己刚填的那个值。
func (p *Provider) ApplySettings(patch json.RawMessage) ([]string, error) {
	if p == nil || p.client == nil {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(patch, &m); err != nil {
		return nil, fmt.Errorf("设置解析失败: %w", err)
	}
	proxy, direct := p.client.ProxyConfig()
	applied := make([]string, 0, 2)
	if raw, ok := m[KeyProxy]; ok {
		s, err := settingString(KeyProxy, raw)
		if err != nil {
			return nil, err
		}
		proxy = s
		applied = append(applied, KeyProxy)
	}
	if raw, ok := m[KeyDirectProxy]; ok {
		s, err := settingString(KeyDirectProxy, raw)
		if err != nil {
			return nil, err
		}
		direct = s
		applied = append(applied, KeyDirectProxy)
	}
	if len(applied) == 0 {
		return nil, nil // 补丁里没有我的键：不是错误，也不算我应用了什么
	}
	proxy, err := normalizeProxyURL(KeyProxy, proxy)
	if err != nil {
		return nil, err
	}
	direct, err = normalizeProxyURL(KeyDirectProxy, direct)
	if err != nil {
		return nil, err
	}
	// 热生效：换 transport，不重启。
	p.client.SetProxy(proxy, direct)
	return applied, nil
}

// settingString 取一个字符串型设置值（JSON 里给 null 视为空串）。
func settingString(key string, raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s 需要是字符串（如 http://127.0.0.1:10808）", key)
	}
	return strings.TrimSpace(s), nil
}

// normalizeProxyURL 校验并归一代理地址（空 = 直连，合法）。
func normalizeProxyURL(key, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	// 允许用户只写 host:port（很多人从别处复制粘贴的就是这个形态）——补默认 scheme。
	if !strings.Contains(v, "://") {
		v = "http://" + v
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("%s 不是合法的地址: %v", key, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return "", fmt.Errorf("%s 的协议只支持 http / https / socks5（当前 %q）", key, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s 缺少主机名（应形如 http://127.0.0.1:10808）", key)
	}
	return u.String(), nil
}
