package tier2

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// ── 维度2 跨域证据 ─────────────────────────────────────────────

type tier2CrossResult struct {
	HitSelector string
	URLBefore   string
	URLAfter    string
	URLChanged  bool
	CrossOrigin bool
}

func crossDomainAction(ctx context.Context, s Session) (tier2CrossResult, error) {
	expr := "(function(){ var el=document.getElementById('btn-cross'); if(!el) return JSON.stringify({hit:{},url_before:location.href});" +
		"var before=location.href; var path=__tier2_path(el); var text=(el.textContent||'').trim(); var tag=(el.tagName||'').toLowerCase();" +
		"el.click(); return JSON.stringify({hit:{selector:path,text:text,tag:tag},url_before:before}); })()"
	raw, err := s.EvalResult(ctx, tier2CandidateJS+"\n"+expr)
	if err != nil {
		return tier2CrossResult{}, err
	}
	var ev struct {
		Hit struct {
			Selector string `json:"selector"`
		} `json:"hit"`
		URLBefore string `json:"url_before"`
	}
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return tier2CrossResult{}, err
	}
	after := ""
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		after = s.PageURL(ctx)
		if after != "" && after != ev.URLBefore {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	return tier2CrossResult{
		HitSelector: ev.Hit.Selector,
		URLBefore:   ev.URLBefore,
		URLAfter:    after,
		URLChanged:  after != "" && after != ev.URLBefore,
		CrossOrigin: tier2CrossOrigin(ev.URLBefore, after),
	}, nil
}

func tier2CrossOrigin(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return ua.Host != ub.Host
}

// ── 维度3 κ ─────────────────────────────────────────────────

func tier2Kappa(a, b []string) (kappa float64, defined bool, agreement float64) {
	if len(a) != len(b) {
		return 0, false, 0
	}
	n := len(a)
	if n == 0 {
		return 0, false, 0
	}
	agree := 0
	pa, pb := 0.0, 0.0
	for i := range a {
		av := 0
		if a[i] == "success" {
			av = 1
		}
		bv := 0
		if b[i] == "success" {
			bv = 1
		}
		if av == bv {
			agree++
		}
		pa += float64(av)
		pb += float64(bv)
	}
	pa /= float64(n)
	pb /= float64(n)
	po := float64(agree) / float64(n)
	pe := pa*pb + (1-pa)*(1-pb)
	agreement = po
	if pe == 1 {
		return 0, false, po
	}
	return (po - pe) / (1 - pe), true, po
}

func tier2SuccessRate(labels []string) float64 {
	if len(labels) == 0 {
		return 0
	}
	n := 0
	for _, l := range labels {
		if l == "success" {
			n++
		}
	}
	return roundTier2(float64(n)/float64(len(labels)), 4)
}

// ── 工具函数 ────────────────────────────────────────────────

func setTier2Throttle(ctx context.Context, s Session, cpu, latency, downloadKbps, uploadKbps float64) error {
	if err := s.CallSession(ctx, "Emulation.setCPUThrottlingRate",
		map[string]any{"rate": cpu}, nil); err != nil {
		return err
	}
	params := map[string]any{
		"offline":            false,
		"latency":            latency,
		"downloadThroughput": downloadKbps,
		"uploadThroughput":   uploadKbps,
	}
	return s.CallSession(ctx, "Network.emulateNetworkConditions", params, nil)
}

func waitTier2Selector(ctx context.Context, s Session, selector string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	expr := "!!document.querySelector(" + strconv.Quote(selector) + ")"
	for time.Now().Before(deadline) {
		ok, err := s.EvalBool(ctx, expr)
		if err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("tier2: selector %s not ready", selector)
}

func nearestRankP99(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	idx := int(math.Ceil(0.99*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func roundTier2(v float64, decimals int) float64 {
	shift := math.Pow10(decimals)
	return math.Round(v*shift) / shift
}

// ── HTML 常量 ────────────────────────────────────────────────

const tier2StableHTML = `<!doctype html><html><head><meta charset="utf-8"><title>stable</title></head><body>
<nav id="nav" role="navigation"><div class="ghost"><button id="btn-login" data-testid="login" aria-label="用户登录">登录</button></div></nav>
<main><section id="checkout" role="region"><div><button id="btn-buy" class="buy" data-testid="buy" aria-label="立即购买">立即购买</button></div></section></main>
</body></html>`

const tier2DriftingHTML = `<!doctype html><html><head><meta charset="utf-8"><title>drifting</title></head><body>
<nav id="nav" role="navigation"><div class="ghost"><button id="btn-login" data-testid="login" aria-label="用户登录">登录</button></div></nav>
<main><section id="checkout" role="region"><div><button id="btn-buy" class="buy" data-testid="buy">立即购买</button></div></section></main>
<script>window.addEventListener('load',function(){var btn=document.getElementById('btn-login');var wrap=document.createElement('div');wrap.setAttribute('role','groups');var parent=btn.parentNode;parent.insertBefore(wrap,btn);wrap.appendChild(btn);});</script>
</body></html>`

const tier2CrossHTML = `<!doctype html><html><head><meta charset="utf-8"><title>cross</title></head><body>
<button id="btn-cross" data-testid="replace">跳转跨域</button>
<script>document.getElementById('btn-cross').addEventListener('click',function(){window.location.replace('__LANDING_ORIGIN__/landing.html');});</script>
</body></html>`

const tier2LandingHTML = `<!doctype html><html><head><meta charset="utf-8"><title>landing</title></head><body><h1 id="landing-title">已到达落地页</h1></body></html>`

func tier2MutatedHTML(semantic, mutation string) string {
	target := tier2TargetForSemantic(semantic)
	if target == nil {
		return "<!doctype html><html><body></body></html>"
	}
	loginText := "登录"
	buyText := "立即购买"
	if target.Text == "登录" && mutation == "rename" {
		loginText = tier2MutationRename[target.Text]
	}
	if target.Text == "立即购买" && mutation == "rename" {
		buyText = tier2MutationRename[target.Text]
	}
	loginHTML := tier2MutatedButton("btn-login", loginText, "decoy", target.Text == "登录", mutation, target.Text == "登录")
	buyHTML := tier2MutatedButton("btn-buy", buyText, "target", target.Text == "立即购买", mutation, target.Text == "立即购买")
	if target.Text == "立即购买" {
		loginHTML = tier2MutatedButton("btn-login", "登录", "decoy", false, "", false)
		buyHTML = tier2MutatedButton("btn-buy", buyText, "target", true, mutation, true)
	}
	return `<!doctype html><html><head><meta charset="utf-8"><title>` + semantic + "/" + mutation + `</title></head><body>
<nav id="nav" role="navigation"><div class="ghost">` + loginHTML + `</div></nav>
<main><section id="checkout" role="region"><div>` + buyHTML + `</div></section></main>
</body></html>`
}

func tier2MutatedButton(id, text, dataRole string, truth bool, mutation string, target bool) string {
	truthAttr := ""
	if truth {
		truthAttr = ` data-t2truth="1"`
	}
	el := `<button id="` + id + `" data-testid="` + id + `" data-role="` + dataRole + `" aria-label="` + text + `"` + truthAttr + `>` + text + `</button>`
	if mutation == "wrap" && target {
		el = `<div class="ghost-wrap">` + el + `</div>`
	}
	return el
}

func tier2TargetForSemantic(semantic string) *tier2Target {
	for i := range tier2Targets {
		if tier2Targets[i].Text == semantic {
			return &tier2Targets[i]
		}
	}
	return nil
}
