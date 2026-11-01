// namespaced.go 渠道公开名的**命名空间规则**（唯一一份实现，目录侧与路由侧共用）。
//
// # 它解决什么（用户报的现象）
//
// 用户在请求日志里看到 `fake-refusal-test` 这种**没有上游前缀**的模型名。
// 根因：渠道声明了模型、但没写 model_mapping 时，目录直接把**上游原名**当公开名输出
// （internal/modelvis 的规则 3 原样输出；那是照 new-api 的行为抄的）。
// 而用户的约定是「模型目录里只应出现带上游/渠道前缀的模型」——
// 他实际在用的写法就是 `csg/deepseek-v4.1-flash-1017`。
//
// # 规则
//
//	公开名 = <渠道名>/<该渠道声明的上游名>
//
// 显式 model_mapping 仍然优先（用户手写的映射不许被这条规则盖掉）。
//
// # 为什么要抽成共享函数
//
// 目录侧（把上游名换成公开名）与路由侧（把公开名换回上游名）**必须对称**：
// channelroute 的文件头就写着"匹配规则（与目录侧对称，这是关键）"。
// 两侧各写一份拼接/切分逻辑，迟早会出现"目录里看得见、发请求却 400" ——
// 那正是这一族问题的老形态（见 models_channels.go 文件头那两段用户反馈）。
package modelmap

import "strings"

// Namespaced 把渠道名与它声明的上游名拼成对外公开名。
//
// 渠道名为空时**原样返回**上游名：没有前缀可用的时候不该凭空造一个（会造出 "/model" 这种坏名字）。
func Namespaced(channelName, upstream string) string {
	if channelName == "" || upstream == "" {
		return upstream
	}
	if strings.HasPrefix(upstream, channelName+"/") {
		return upstream // 已经是这个名字空间下的，别拼成 a/a/model
	}
	return channelName + "/" + upstream
}

// SplitNamespaced 是 Namespaced 的反操作：从公开名里取出该渠道名下的上游名。
//
// 命中返回 (上游名, true)；不属于这个渠道的命名空间则返回 ("", false)。
//
// ⚠ 只剥**一层**：`七牛云/deepseek/xxx` → `deepseek/xxx`（上游名本身可能含斜杠，
//
//	而厂商前缀是上游名的一部分，不能被当成第二层命名空间）。
func SplitNamespaced(channelName, publicModel string) (string, bool) {
	if channelName == "" || publicModel == "" {
		return "", false
	}
	prefix := channelName + "/"
	if !strings.HasPrefix(publicModel, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(publicModel, prefix)
	if rest == "" {
		return "", false
	}
	return rest, true
}
