// profile_root_test.go profileRoot 必须是绝对路径。
//
// # 2026-11-14 用户报障（T-457）
//
// 现象：每次点「添加账号」都弹 Edge 的对话框
//
//	无法读取和写入其数据目录：.browser-profiles\wb2api-login-<纳秒>
//
// 原因：`--user-data-dir` 拿到的是**相对路径**，而浏览器按**它自己的 CWD**
// 解析（Edge 装在 `C:\Program Files (x86)\Microsoft\Edge\Application\`，不可写）
// ⇒ 建 profile 目录失败 ⇒ 授权页打不开 ⇒ 弹窗每次必现。
//
// 这条判据把"必须是绝对路径"钉住：任何一条分支返回相对路径都会红。
package monkeycode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProfileRootIsAbsolute(t *testing.T) {
	cases := []struct {
		name    string
		authDir string
	}{
		{"authDir 为空（落到工作目录）", ""},
		{"authDir 相对路径", filepath.Join(".", "auths", "monkeycode")},
		{"authDir 绝对路径", filepath.Join(os.TempDir(), "auths", "monkeycode")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Provider{authDir: c.authDir}
			got := p.profileRoot()

			if got == "" {
				t.Fatal("profileRoot 不能返回空串（那会把决定权交给下游，见 T-456）")
			}
			if !filepath.IsAbs(got) {
				t.Errorf("必须是绝对路径，实得 %q\n"+
					"  相对路径会被浏览器按它自己的 CWD 解析 ⇒ 建目录失败 ⇒ "+
					"用户看到「无法读取和写入其数据目录」弹窗", got)
			}
			if filepath.Base(got) != ".browser-profiles" {
				t.Errorf("末段应为 .browser-profiles，实得 %q", filepath.Base(got))
			}
		})
	}
}

// TestProfileRootAvoidsSystemTempWhenUnconfigured T-456 的语义仍在：
// 没配 authDir 时**不要**往 %TEMP% 里堆（那里曾攒了 24 个 525MB 的 profile）。
func TestProfileRootAvoidsSystemTempWhenUnconfigured(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		t.Skip("拿不到工作目录，跳过")
	}
	p := &Provider{}
	got := p.profileRoot()

	temp := filepath.Clean(os.TempDir())
	if filepath.IsAbs(got) {
		parent := filepath.Clean(filepath.Dir(got))
		if parent == temp {
			t.Errorf("没配 authDir 时不该落在系统临时目录（%s），实得 %q", temp, got)
		}
	}
	if want := filepath.Join(wd, ".browser-profiles"); filepath.Clean(got) != filepath.Clean(want) {
		t.Errorf("应落在工作目录下的 .browser-profiles，期望 %q，实得 %q", want, got)
	}
}
