// visibility_test.go 守卫 R1 的六条规则。每条都对应一句产品承诺，删掉实现里的对应分支就会红。
package modelvis

import (
	"reflect"
	"testing"
)

// 规则 1：没有任何启用渠道能服务 ⇒ 对外空（这是"默认收紧"的本体，不是"放行全部"）。
func TestNoAbilityMeansNothingPublic(t *testing.T) {
	got := PublicModels(nil, nil, false, nil)
	if len(got) != 0 {
		t.Fatalf("没有能力记录时对外应为空，实际 %v", got)
	}
	// 哪怕白名单非空，也不能凭空变出模型
	got = PublicModels([]string{}, []string{"m1", "m2"}, true, nil)
	if len(got) != 0 {
		t.Fatalf("白名单不能凭空产生模型，实际 %v", got)
	}
}

// 不限制的令牌：看到全部有能力的模型（升序去重）。
func TestUnrestrictedSeesAllAbilities(t *testing.T) {
	got := PublicModels([]string{"b", "a", "a"}, nil, false, nil)
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("应升序去重得 [a b]，实际 %v", got)
	}
}

// 受限令牌：只看到白名单里的（交集）。
func TestRestrictedSeesOnlyWhitelisted(t *testing.T) {
	got := PublicModels([]string{"a", "b", "c"}, []string{"b", "zzz"}, true, nil)
	if !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("应只剩 [b]（zzz 没有渠道能力），实际 %v", got)
	}
}

// 白名单里的名字必须**真的有渠道能力**才会出现 —— 这条是"渠道没能力就不对外"。
func TestWhitelistCannotExceedAbility(t *testing.T) {
	if got := PublicModels([]string{"a"}, []string{"a", "secret-model"}, true, nil); len(got) != 1 || got[0] != "a" {
		t.Fatalf("白名单里没有渠道能力的模型不该出现，实际 %v", got)
	}
}

// 反向映射：能力里存的是上游真实名，输出要用公开名；白名单也按公开名比对。
func TestReverseMappingDrivesPublicNames(t *testing.T) {
	rev := map[string]string{"real-x": "pub-y"}
	// 不限制：输出公开名
	if got := PublicModels([]string{"real-x", "real-z"}, nil, false, rev); !reflect.DeepEqual(got, []string{"pub-y", "real-z"}) {
		t.Fatalf("映射项应出公开名、未映射项原样，实际 %v", got)
	}
	// 受限：白名单写**公开名**能放行
	if got := PublicModels([]string{"real-x"}, []string{"pub-y"}, true, rev); !reflect.DeepEqual(got, []string{"pub-y"}) {
		t.Fatalf("白名单按公开名应放行，实际 %v", got)
	}
	// 受限：白名单写**上游名**不放行（口径必须唯一，不能两种都认）
	if got := PublicModels([]string{"real-x"}, []string{"real-x"}, true, rev); len(got) != 0 {
		t.Fatalf("白名单按上游名不该放行（口径应统一为公开名），实际 %v", got)
	}
}

// 两个上游模型映射到同一个公开名 ⇒ 只列一次。
func TestDuplicatePublicNamesCollapse(t *testing.T) {
	rev := map[string]string{"r1": "same", "r2": "same"}
	got := PublicModels([]string{"r1", "r2"}, nil, false, rev)
	if !reflect.DeepEqual(got, []string{"same"}) {
		t.Fatalf("重复公开名应只列一次，实际 %v", got)
	}
}

// restricted=true 但白名单为空是调用方传错：按最保守处理（不放行任何东西），不是"放行全部"。
func TestRestrictedWithEmptyWhitelistIsClosed(t *testing.T) {
	got := PublicModels([]string{"a", "b"}, nil, true, nil)
	if len(got) != 0 {
		t.Fatalf("restricted 且白名单为空应不放行任何模型，实际 %v", got)
	}
}

// VisibleForRequest 与列表同一套规则。
func TestVisibleForRequestMatchesList(t *testing.T) {
	rev := map[string]string{"real": "pub"}
	if !VisibleForRequest([]string{"real"}, nil, false, rev, "pub") {
		t.Fatal("不限制时应允许 pub")
	}
	if VisibleForRequest([]string{"real"}, nil, false, rev, "real") {
		t.Fatal("对外只认公开名，上游名不该被接受")
	}
	if VisibleForRequest([]string{"real"}, []string{"pub"}, true, rev, "other") {
		t.Fatal("白名单外的不该允许")
	}
	if VisibleForRequest(nil, nil, false, nil, "anything") {
		t.Fatal("没有能力时任何模型都不该允许")
	}
	if VisibleForRequest([]string{"real"}, nil, false, nil, "") {
		t.Fatal("空模型名不该允许")
	}
}
