package tier2

import (
	"math"
	"strings"
	"testing"

	"uws/pkg/matcher"
)

func TestTier2StructuralHashMatchesMatcher(t *testing.T) {
	feat := tier2RawFeature{
		Tag:  "button",
		Role: "",
		Ancestors: []tier2RawAncestor{
			{Tag: "div", Role: ""},
			{Tag: "nav", Role: "navigation"},
		},
	}
	want := matcher.StructuralSimHash64(matcher.StructFeature{
		Tag: "button",
		Ancestors: []matcher.StructAncestor{
			{Tag: "div", Role: ""},
			{Tag: "nav", Role: "navigation"},
		},
	})
	if got := tier2StructuralHash(feat); got != want {
		t.Fatalf("tier2StructuralHash = %#x, want %#x", got, want)
	}
}

func TestTier2LocateRenameRescuesByStruct(t *testing.T) {
	target := tier2Candidate{
		Text: "立即登录", Tag: "button", Role: "", Truth: true, Path: "html > body > nav > div > button#btn-login",
		StructHash: tier2StructuralHash(tier2RawFeature{Tag: "button", Ancestors: []tier2RawAncestor{{Tag: "nav", Role: "navigation"}}}),
		TextHash:   matcher.SimHash64("立即登录"),
	}
	decoy := tier2Candidate{
		Text: "立即购买", Tag: "button", Role: "", Truth: false, Path: "html > body > main > section > div > button#btn-buy",
		StructHash: tier2StructuralHash(tier2RawFeature{Tag: "button", Ancestors: []tier2RawAncestor{{Tag: "section", Role: "region"}}}),
		TextHash:   matcher.SimHash64("立即购买"),
	}
	rule := tier2Rule{
		Text:       "登录",
		StructHash: tier2StructuralHash(tier2RawFeature{Tag: "button", Ancestors: []tier2RawAncestor{{Tag: "nav", Role: "navigation"}}}),
		Tag:        "button",
	}
	sel := locateTier2([]tier2Candidate{decoy, target}, rule)
	if sel == nil || !sel.Truth || sel.Text != "立即登录" {
		t.Fatalf("rename 应由结构通道救回，got %+v", sel)
	}
	if sel.TextDist <= tier2D {
		t.Fatalf("rename 文本距离应 > D_h，got %d", sel.TextDist)
	}
	if sel.StructDist != 0 {
		t.Fatalf("rename 结构距离应为 0，got %d", sel.StructDist)
	}
}

func TestTier2KappaAllSuccessUndefined(t *testing.T) {
	labels := []string{"success", "success", "success", "success"}
	k, defined, agreement := tier2Kappa(labels, labels)
	if defined {
		t.Fatalf("全 success 的 κ 应标记 undefined，got k=%v", k)
	}
	if agreement != 1 {
		t.Fatalf("agreement = %v, want 1", agreement)
	}
}

func TestTier2KappaMixed(t *testing.T) {
	a := []string{"success", "success", "miss", "miss"}
	b := []string{"success", "miss", "success", "miss"}
	k, defined, agreement := tier2Kappa(a, b)
	if !defined {
		t.Fatal("非退化 kappa 应 defined")
	}
	if math.Abs(k) > 1e-9 {
		t.Fatalf("kappa = %v, want 0", k)
	}
	if math.Abs(agreement-0.5) > 1e-9 {
		t.Fatalf("agreement = %v, want 0.5", agreement)
	}
}

func TestTier2NearestRankP99(t *testing.T) {
	values := make([]float64, 100)
	for i := range values {
		values[i] = float64(i)
	}
	if got := nearestRankP99(values); got != 98 {
		t.Fatalf("nearest-rank P99 = %v, want 98（不应取 max=99）", got)
	}
}

func TestTier2MutatedHTMLRenameTarget(t *testing.T) {
	html := tier2MutatedHTML("登录", "rename")
	if !strings.Contains(html, ">立即登录<") {
		t.Fatalf("rename 登录页应包含目标文本「立即登录」: %s", html)
	}
	if !strings.Contains(html, `id="btn-login"`) || !strings.Contains(html, `data-t2truth="1"`) {
		t.Fatalf("rename 登录页应标记 btn-login 为真身: %s", html)
	}
}
