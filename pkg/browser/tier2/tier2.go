package tier2

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// Tier 2 动态沙盒 Go runner（真实 Chrome/CDP）。
//
// 把 benchmarks/tier2 的四个测量维度迁到 pkg/browser 内，使用与生产同一条
// CDP 会话链路，并把判分/定位算法迁移到 matcher 导出函数：
//   0 时间系综采样（结构不变量检测）
//   1 Hydration 抖动生还率 + 定位决策延迟
//   2 跨域内存屏障证据链
//   3 UWS 时间态 vs Malhotra 静态真值的 Cohen's Kappa
//
// 与 Python 版不同，本 runner 修正了两个口径问题：
//   - P99 使用 nearest-rank，而不是 int(0.99*n) 误取 max；
//   - kappa 在 p_e == 1（两个 rater 全标同一类）时标记 undefined，
//     不再把无区分度的全 success 直接当作 1.0。

// Options 是 Run 的可选参数。
type Options struct {
	ChromePath    string
	ProbeWasmPath string
	Timeout       time.Duration
}

// Report 是 Go runner 的完整测度结果，JSON 友好。
type Report struct {
	Dim0TemporalEnsemble Dim0 `json:"dim0_temporal_ensemble"`
	Dim1Hydration        Dim1 `json:"dim1_hydration_survival"`
	Dim2CrossDomain      Dim2 `json:"dim2_cross_domain_barrier"`
	Dim3Kappa            Dim3 `json:"dim3_kappa_calibration"`
}

type Dim0 struct {
	Records          int        `json:"records"`
	Stable           int        `json:"stable"`
	Drifted          int        `json:"drifted"`
	ReferenceEntries int        `json:"reference_entries"`
	DriftDetail      []DriftRow `json:"drift_detail"`
}

type DriftRow struct {
	Page     string `json:"page"`
	Semantic string `json:"semantic"`
	HashT0   string `json:"hash_t0"`
	HashT2   string `json:"hash_t2"`
}

type Dim1 struct {
	TotalCases        int       `json:"total_cases"`
	Success           int       `json:"success"`
	SurvivalRate      float64   `json:"survival_rate"`
	FalseHeal         int       `json:"false_heal"`
	Miss              int       `json:"miss"`
	LocateLatencyMean float64   `json:"locate_latency_mean_ms"`
	LocateLatencyP99  float64   `json:"locate_latency_p99_ms"`
	MaxCandidates     int       `json:"max_candidates"`
	Rows              []Dim1Row `json:"rows"`
}

type Dim1Row struct {
	Semantic         string  `json:"semantic"`
	Mutation         string  `json:"mutation"`
	Jitter           int     `json:"jitter"`
	CPURate          float64 `json:"cpu_rate"`
	LatencyMS        float64 `json:"latency_ms"`
	Candidates       int     `json:"n_candidates"`
	SelectedTruth    bool    `json:"selected_truth"`
	LocateLatencyMS  float64 `json:"locate_latency_ms"`
	TextDist         int     `json:"selected_text_dist"`
	StructDist       int     `json:"selected_struct_dist"`
	DualChannelFired bool    `json:"dual_channel_fired"`
	UWSVerdict       string  `json:"uws_verdict"`
	JudgeVerdict     string  `json:"judge_verdict"`
}

type Dim2 struct {
	Trials      int         `json:"trials"`
	LossRate    float64     `json:"loss_rate"`
	URLChanged  bool        `json:"url_changed"`
	CrossOrigin bool        `json:"cross_origin"`
	PerTrial    []Dim2Trial `json:"per_trial"`
}

type Dim2Trial struct {
	URLChanged  bool   `json:"url_changed"`
	CrossOrigin bool   `json:"cross_origin"`
	HitSelector string `json:"hit_selector"`
	OK          bool   `json:"ok"`
}

type Dim3 struct {
	N                int     `json:"n"`
	Kappa            float64 `json:"kappa"`
	KappaDefined     bool    `json:"kappa_defined"`
	AgreementRate    float64 `json:"agreement_rate"`
	UWSSuccessRate   float64 `json:"uws_success_rate"`
	JudgeSuccessRate float64 `json:"judge_success_rate"`
}

type tier2Target struct {
	Selector string
	Text     string
	Tag      string
	Role     string
}

type tier2RawAncestor struct {
	Tag  string `json:"tag"`
	Role string `json:"role"`
}

type tier2RawFeature struct {
	Tag       string             `json:"tag"`
	Role      string             `json:"role"`
	Ancestors []tier2RawAncestor `json:"ancestors"`
}

type tier2TemporalRow struct {
	Page           string
	Semantic       string
	Selector       string
	ByT            map[string]tier2RawFeature
	Hashes         map[string]uint64
	Invariant      bool
	AncRoleChanged bool
	StructT0       tier2RawFeature
}

type tier2ReferenceEntry struct {
	Semantic string
	Struct   tier2RawFeature
}

type tier2Rule struct {
	Semantic   string
	Text       string
	StructHash uint64
	Tag        string
	Role       string
}

type tier2Candidate struct {
	Text       string
	Tag        string
	Role       string
	Path       string
	Truth      bool
	CX, CY     float64
	StructHash uint64
	TextHash   uint64
	TextDist   int
	StructDist int
	Ancestors  []tier2RawAncestor
}

type tier2CandidateRaw struct {
	Text      string             `json:"text"`
	Tag       string             `json:"tag"`
	Role      string             `json:"role"`
	Ancestors []tier2RawAncestor `json:"ancestors"`
	Path      string             `json:"path"`
	Truth     bool               `json:"truth"`
	CX        float64            `json:"cx"`
	CY        float64            `json:"cy"`
}

var tier2Targets = []tier2Target{
	{Selector: "#btn-login", Text: "登录", Tag: "button", Role: "button"},
	{Selector: "#btn-buy", Text: "立即购买", Tag: "button", Role: "button"},
}

var tier2MutationRename = map[string]string{
	"登录":   "立即登录",
	"立即购买": "立即购买商品",
}

var tier2Jitters = []struct {
	CPU   float64
	Delay float64
}{
	{CPU: 1, Delay: 50},
	{CPU: 4, Delay: 150},
	{CPU: 6, Delay: 300},
}

const tier2D = 5
const tier2Soft = 10

// Run 使用真实 Chrome 执行四维动态沙盒测量（会话由调用方创建、经窄接口 Session 注入）。
func Run(ctx context.Context, opts Options, s Session) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	landing, cross, ref, mut := newTier2Servers()
	defer landing.Close()
	defer cross.Close()
	defer ref.Close()
	defer mut.Close()

	var report Report

	// ── 维度0：时间系综采样 ──
	selectors := make([]string, 0, len(tier2Targets))
	for _, t := range tier2Targets {
		selectors = append(selectors, t.Selector)
	}
	if err := installTier2TemporalMarker(ctx, s, selectors); err != nil {
		return report, err
	}
	var records []tier2TemporalRow
	for _, page := range []string{"/stable.html", "/drifting.html"} {
		rows, err := sampleTier2TemporalPage(ctx, s, ref.URL+page, selectors)
		if err != nil {
			return report, err
		}
		records = append(records, rows...)
	}
	refEntries := tier2BuildReference(records)
	report.Dim0TemporalEnsemble = tier2Dim0FromRows(records, refEntries)

	rules := tier2RulesFromReference(refEntries)

	// ── 维度1：Hydration 抖动 ──
	for _, target := range tier2Targets {
		rule, ok := rules[target.Text]
		if !ok {
			continue
		}
		for _, mutation := range []string{"control", "rename", "wrap"} {
			pagePath := fmt.Sprintf("/mut_%s_%s.html", url.QueryEscape(target.Text), mutation)
			for ji, jitter := range tier2Jitters {
				if err := setTier2Throttle(ctx, s, jitter.CPU, jitter.Delay, -1, -1); err != nil {
					return report, err
				}
				if err := s.Navigate(ctx, mut.URL+pagePath); err != nil {
					return report, err
				}
				if err := waitTier2Selector(ctx, s, "[data-t2truth='1']", 5*time.Second); err != nil {
					return report, err
				}
				cands, err := collectTier2Candidates(ctx, s)
				if err != nil {
					return report, err
				}
				start := time.Now()
				selected := locateTier2(cands, rule)
				latency := float64(time.Since(start).Microseconds()) / 1000.0
				if selected != nil {
					_ = s.ClickPoint(ctx, selected.CX, selected.CY)
				}
				row := Dim1Row{
					Semantic:         target.Text,
					Mutation:         mutation,
					Jitter:           ji,
					CPURate:          jitter.CPU,
					LatencyMS:        jitter.Delay,
					Candidates:       len(cands),
					SelectedTruth:    selected != nil && selected.Truth,
					LocateLatencyMS:  roundTier2(latency, 3),
					TextDist:         tier2SelectedDist(selected, "text"),
					StructDist:       tier2SelectedDist(selected, "struct"),
					DualChannelFired: selected != nil && selected.TextDist > tier2D && selected.StructDist <= tier2D,
					UWSVerdict:       tier2UWSVerdict(selected),
					JudgeVerdict:     tier2JudgeVerdict(selected, cands, target),
				}
				report.Dim1Hydration.Rows = append(report.Dim1Hydration.Rows, row)
			}
		}
	}
	report.Dim1Hydration = tier2Dim1FromRows(report.Dim1Hydration.Rows)

	// ── 维度2：跨域证据链 ──
	_ = setTier2Throttle(ctx, s, 1, 0, -1, -1)
	var perTrial []Dim2Trial
	var losses int
	for i := 0; i < 3; i++ {
		if err := s.Navigate(ctx, cross.URL+"/cross.html"); err != nil {
			return report, err
		}
		if err := waitTier2Selector(ctx, s, "#btn-cross", 5*time.Second); err != nil {
			return report, err
		}
		res, err := crossDomainAction(ctx, s)
		if err != nil {
			return report, err
		}
		ok := res.HitSelector != "" && res.URLChanged
		if !ok {
			losses++
		}
		perTrial = append(perTrial, Dim2Trial{
			URLChanged:  res.URLChanged,
			CrossOrigin: res.CrossOrigin,
			HitSelector: res.HitSelector,
			OK:          ok,
		})
	}
	report.Dim2CrossDomain = Dim2{
		Trials:      3,
		LossRate:    roundTier2(float64(losses)/3.0, 4),
		URLChanged:  len(perTrial) > 0 && perTrial[0].URLChanged,
		CrossOrigin: len(perTrial) > 0 && perTrial[0].CrossOrigin,
		PerTrial:    perTrial,
	}

	// ── 维度3：κ 校准 ──
	var uwsLabels, judgeLabels []string
	for _, r := range report.Dim1Hydration.Rows {
		uwsLabels = append(uwsLabels, r.UWSVerdict)
		judgeLabels = append(judgeLabels, r.JudgeVerdict)
	}
	kappa, defined, agreement := tier2Kappa(uwsLabels, judgeLabels)
	report.Dim3Kappa = Dim3{
		N:                len(uwsLabels),
		Kappa:            roundTier2(kappa, 4),
		KappaDefined:     defined,
		AgreementRate:    roundTier2(agreement, 4),
		UWSSuccessRate:   tier2SuccessRate(uwsLabels),
		JudgeSuccessRate: tier2SuccessRate(judgeLabels),
	}

	return report, nil
}
