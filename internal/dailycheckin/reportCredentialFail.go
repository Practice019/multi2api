// reportCredentialFail.go 凭据类失败的上报出口（T-431）。
//
// 单独一个文件放这个小函数：它把"判定"（CredentialError）与"上报"（回调）粘起来，
// 而两者的理由各自写在各处（判定在 credential_error.go，回调在 Options 的注释里）。
package dailycheckin

import "log"

// reportCredentialFail 判定 + 上报。**网络类与模糊措辞一律不上报**（不误杀）。
//
// ⚠ 没接回调（上游没实现）时只打一条日志 —— 用户至少能从日志看出"这个号要重登"，
//
//	而不是只看到一条"签到失败"。
func (d *Driver) reportCredentialFail(uid string, err error) {
	if !CredentialError(err) {
		return
	}
	log.Printf("%s: 签到凭据失效 uid=%s：%v（按设计上报，由上游侧决定是否自动禁用）",
		d.desc.ID, shortUID(uid), err)
	if d.onCredFail != nil {
		d.onCredFail(uid, err)
	}
}
