package vlm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseResult(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Result
	}{
		{"plain json", `{"found":true,"text":"Godfall: Helios' Inverse Scale","x_ratio":0.5,"y_ratio":0.22}`, Result{Found: true, Text: "Godfall: Helios' Inverse Scale", XRatio: 0.5, YRatio: 0.22}},
		{"markdown fence", "```json\n{\"found\":false}\n```", Result{Found: false}},
		{"with prose", "找到了：\n{\"found\":true,\"text\":\"More\",\"selector\":\"div.more\"}\n以上。", Result{Found: true, Text: "More", Selector: "div.more"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseResult(c.raw)
			if err != nil {
				t.Fatalf("parseResult: %v", err)
			}
			if got.Found != c.want.Found || got.Text != c.want.Text ||
				got.XRatio != c.want.XRatio || got.YRatio != c.want.YRatio ||
				got.Selector != c.want.Selector {
				t.Fatalf("parseResult = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseResultNoJSON(t *testing.T) {
	if _, err := parseResult("模型说：找不到"); err == nil {
		t.Fatal("want error for non-json output")
	}
}

func TestParseResultNodeID(t *testing.T) {
	r, err := parseResult(`{"found":true,"node_id":12,"text":"结算按钮"}`)
	if err != nil {
		t.Fatalf("parseResult: %v", err)
	}
	if r.NodeID != 12 || !r.Found || r.Text != "结算按钮" {
		t.Fatalf("parseResult = %+v", r)
	}
}

func TestDeepSeekResolve(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth = %q", got)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if req["model"] != DefaultModel {
			t.Errorf("model = %v", req["model"])
		}
		if req["temperature"] != float64(0) {
			t.Errorf("temperature = %v", req["temperature"])
		}
		msgs := req["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)
		img := content[1].(map[string]any)["image_url"].(map[string]any)
		if got := img["url"].(string); !strings.HasPrefix(got, "data:image/png;base64,iVBORw0KGgo") {
			t.Errorf("image url prefix = %q", got[:40])
		}
		// §5.0.9 条款 8：412×915 长边 > 768 ⇒ 自动升 high（旧实现硬编码 low）。
		if got := img["detail"]; got != DetailHigh {
			t.Errorf("detail = %v, want %s", got, DetailHigh)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"found\":true,\"text\":\"More\",\"x_ratio\":0.9,\"y_ratio\":0.12}"}}]}`))
	}))
	defer srv.Close()

	c := NewDeepSeek(Config{BaseURL: srv.URL, APIKey: "sk-test", Timeout: 3 * time.Second})
	// 1x1 透明 PNG
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52}
	got, err := c.Resolve(context.Background(), png, 412, 915, ResolveRequest{Target: "More"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Found || got.Text != "More" || got.XRatio != 0.9 || got.YRatio != 0.12 {
		t.Fatalf("Resolve = %+v", got)
	}
}

// TestResolveDetailBudget 锁定 §5.0.9 条款 8 的细节预算选档：
// 整页大图自动 high（低档会把小目标压没）、小裁剪自动 low（不额外付费）、
// 调用方显式给定档位时以显式为准（离线对照臂依赖）。
func TestResolveDetailBudget(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		msgs := req["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)
		got = content[1].(map[string]any)["image_url"].(map[string]any)["detail"].(string)
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"found\":true,\"x_ratio\":0.5,\"y_ratio\":0.5}"}}]}`))
	}))
	defer srv.Close()
	c := NewDeepSeek(Config{BaseURL: srv.URL, APIKey: "sk", Timeout: 3 * time.Second})
	png := []byte{0x89, 0x50, 0x4e, 0x47}

	cases := []struct {
		name string
		w, h int
		req  ResolveRequest
		want string
	}{
		{"整页 2880×1626 自动 high", 2880, 1626, ResolveRequest{Target: "RENAME"}, DetailHigh},
		{"竖屏 412×915 自动 high", 412, 915, ResolveRequest{Target: "More"}, DetailHigh},
		{"小裁剪 640×400 自动 low", 640, 400, ResolveRequest{Target: "RENAME", Crop: true}, DetailLow},
		{"阈值上 769 自动 high", 769, 400, ResolveRequest{Target: "t"}, DetailHigh},
		{"阈值上 768 自动 low", 768, 400, ResolveRequest{Target: "t"}, DetailLow},
		{"显式 low 覆盖自动档", 2880, 1626, ResolveRequest{Target: "t", Detail: DetailLow}, DetailLow},
		{"显式 high 覆盖自动档", 640, 400, ResolveRequest{Target: "t", Detail: DetailHigh}, DetailHigh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Resolve(context.Background(), png, tc.w, tc.h, tc.req); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tc.want {
				t.Fatalf("detail = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeepSeekResolveHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()
	c := NewDeepSeek(Config{BaseURL: srv.URL, APIKey: "sk", Timeout: time.Second})
	if _, err := c.Resolve(context.Background(), []byte("x"), 10, 10, ResolveRequest{Target: "t"}); err == nil {
		t.Fatal("want error on 429")
	}
}

// TestDeepSeekResolveFastGuess 锁定「看图即答」配置：请求体必须带 thinking:disabled
// 与 max_tokens 上限，且提示词含「不要推理」——这是 2026-08-27 实测降本的关键
// （关闭 reasoning 后单次 1.3s / 43 输出 token / 0 推理 token，见 pkg/vlm/MEMORY.md）。
func TestDeepSeekResolveFastGuess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		thinking, ok := req["thinking"].(map[string]any)
		if !ok {
			t.Fatalf("请求体缺少 thinking 字段: %v", req["thinking"])
		}
		if thinking["type"] != "disabled" {
			t.Errorf("thinking.type = %v, want disabled", thinking["type"])
		}
		if mt := req["max_tokens"]; mt != float64(300) {
			t.Errorf("max_tokens = %v, want 300", mt)
		}
		msgs := req["messages"].([]any)
		prompt := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(prompt, "不要推理") {
			t.Errorf("提示词未含「不要推理」: %q", prompt[:60])
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"found\":true,\"text\":\"Godfall\",\"x_ratio\":0.5,\"y_ratio\":0.3}"}}]}`))
	}))
	defer srv.Close()

	c := NewDeepSeek(Config{BaseURL: srv.URL, APIKey: "sk-test", Timeout: 3 * time.Second})
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	got, err := c.Resolve(context.Background(), png, 412, 915, ResolveRequest{Target: "Godfall"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Found || got.Text != "Godfall" || got.XRatio != 0.5 {
		t.Fatalf("Resolve = %+v", got)
	}
}

// TestDeepSeekExtractPromptAsksDescriptiveContent 锁定「描述性修复」（2026-08-27）：
// extract 动作的提示词必须要求模型「照抄与目标相关的原文内容」（价格数字/成分列表），
// 而不是旧版的「找到文字X」——后者会让模型只回标签词（如「价格」）而非内容本身，
// 判定层拿不到足够信息（skincare/smzdm 复测实证，见 pkg/vlm/MEMORY.md）。
func TestDeepSeekExtractPromptAsksDescriptiveContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		msgs := req["messages"].([]any)
		prompt := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(prompt, "照抄原文") {
			t.Errorf("extract 提示词应要求照抄原文: %q", prompt[:100])
		}
		if !strings.Contains(prompt, "价格请带数字和单位") {
			t.Errorf("extract 提示词应要求价格带数字: %q", prompt[:100])
		}
		if strings.Contains(prompt, "找到文字") {
			t.Errorf("extract 提示词不应是字面查找: %q", prompt[:100])
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"action\":\"done\",\"text\":\"226元（需用券）\"}"}}]}`))
	}))
	defer srv.Close()

	c := NewDeepSeek(Config{BaseURL: srv.URL, APIKey: "sk-test", Timeout: 3 * time.Second})
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	got, err := c.Resolve(context.Background(), png, 412, 915,
		ResolveRequest{Target: "商品价格", Action: "extract", Capabilities: Capabilities("extract")})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Action != ActionDone || got.Text != "226元（需用券）" {
		t.Fatalf("Resolve = %+v", got)
	}
}

// TestDeepSeekInputPromptLocatesInputBox 锁定 2026-08-31 input 兜底修复：
// input/fill 动作的提示词必须引导 VLM 定位「输入框/文本域/下拉框」并报告中心坐标
// （聚焦后引擎负责填值），而不是要求找到按钮或返回 impossible。
func TestDeepSeekInputPromptLocatesInputBox(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		msgs := req["messages"].([]any)
		prompt := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		for _, want := range []string{"输入框", "中心坐标", "action\":\"input\""} {
			if !strings.Contains(prompt, want) {
				t.Errorf("input 提示词应含 %q: %q", want, prompt[:200])
			}
		}
		if strings.Contains(prompt, "按钮不算") == false {
			t.Errorf("input 提示词应明确按钮不算: %q", prompt[:200])
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"action\":\"input\",\"found\":true,\"text\":\"产品名称\",\"x_ratio\":0.5,\"y_ratio\":0.2}"}}]}`))
	}))
	defer srv.Close()

	c := NewDeepSeek(Config{BaseURL: srv.URL, APIKey: "sk-test", Timeout: 3 * time.Second})
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	got, err := c.Resolve(context.Background(), png, 412, 915,
		ResolveRequest{Target: "产品名称", Action: "input", Capabilities: Capabilities("input")})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Action != ActionInput || !got.Found || got.XRatio != 0.5 || got.YRatio != 0.2 {
		t.Fatalf("Resolve = %+v", got)
	}
}

// TestCapabilitiesIncludeInput 锁定 VLM 主控能力清单含 input 动作
// （2026-08-31：规则库未覆盖站点的输入兜底缺口）。
func TestCapabilitiesIncludeInput(t *testing.T) {
	caps := Capabilities("input")
	if !strings.Contains(caps, `"action":"input"`) {
		t.Fatalf("Capabilities(input) 应含 input 动作: %q", caps)
	}
}

// TestParseResultToleratesNewlinesInText 锁定 JSON 健壮性修复（2026-08-27）：
// VLM 在 text 字段输出多行原文（成分表/价格列表）时，裸换行不再导致解析失败。
func TestParseResultToleratesNewlinesInText(t *testing.T) {
	raw := "{\"action\":\"done\",\"text\":\"第一行成分\\n第二行成分\"}"
	// 模型实际可能输出字面换行（非 \n 转义）
	rawLiteral := "{\"action\":\"done\",\"text\":\"第一行成分\n第二行成分\"}"
	for _, src := range []string{raw, rawLiteral} {
		r, err := parseResult(src)
		if err != nil {
			t.Fatalf("parseResult(%q): %v", src, err)
		}
		if r.Action != ActionDone || r.Text != "第一行成分\n第二行成分" {
			t.Fatalf("parseResult(%q) = %+v", src, r)
		}
	}
}

func TestSanitizeJSONNewlines(t *testing.T) {
	got := sanitizeJSONNewlines("{\"a\":\"x\n y\",\"b\":1}")
	if got != "{\"a\":\"x\\n y\",\"b\":1}" {
		t.Fatalf("sanitize = %q", got)
	}
	// 已转义序列不受影响
	got = sanitizeJSONNewlines("{\"a\":\"x\\n y\"}")
	if got != "{\"a\":\"x\\n y\"}" {
		t.Fatalf("sanitize escaped = %q", got)
	}
}
