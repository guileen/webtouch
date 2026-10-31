package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func probeJSON(t *testing.T, probe ModelMenuProbe) string {
	t.Helper()
	b, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func healthyProbe() ModelMenuProbe {
	closedOK := true
	return ModelMenuProbe{
		ClosedOK:      &closedOK,
		PickerPresent: true, PickerLabel: "Open mode picker, currently Flash",
		MenuContainerPresent: true, MenuOpened: true,
		MenuItems: []string{"3.5 Flash-Lite Fastest answers", "3.8 Flash All-around help",
			"3.1 Pro Advanced reasoning", "Extended thinking Complex problem solving"},
	}
}

func TestClassifyModelMenuDrift(t *testing.T) {
	expect := []string{"Flash-Lite", "Flash", "Pro", "thinking"}
	cases := []struct {
		name        string
		probe       ModelMenuProbe
		deep        bool
		wantDrift   bool
		wantReasons []string
	}{
		{name: "正常深自检", probe: healthyProbe(), deep: true},
		{name: "轻自检不开菜单不算漂移",
			probe: ModelMenuProbe{PickerPresent: true, PickerLabel: "Flash", MenuContainerPresent: true}, deep: false},
		{name: "选择器缺失即漂移",
			probe: ModelMenuProbe{}, deep: true, wantDrift: true, wantReasons: []string{"picker_missing", "menu_not_opened"}},
		{name: "菜单打不开（懒渲染后仍读不到）",
			probe: ModelMenuProbe{PickerPresent: true, PickerLabel: "Flash", MenuContainerPresent: true},
			deep:  true, wantDrift: true, wantReasons: []string{"menu_not_opened"}},
		{name: "档位改名/下线",
			probe: ModelMenuProbe{PickerPresent: true, PickerLabel: "Flash", MenuOpened: true,
				MenuItems: []string{"3.5 Flash-Lite Fastest answers", "3.8 Flash All-around help"}},
			deep: true, wantDrift: true, wantReasons: []string{"family_missing:Pro", "family_missing:thinking"}},
		{name: "菜单形状不可识别",
			probe: ModelMenuProbe{PickerPresent: true, PickerLabel: "Flash", MenuOpened: true,
				MenuItems: []string{"GPT-5 Turbo", "Claude Opus"}},
			deep: true, wantDrift: true,
			wantReasons: []string{"family_missing:Flash-Lite", "family_missing:Flash", "family_missing:Pro",
				"family_missing:thinking", "menu_unknown_shape"}},
		{name: "新增档位不算漂移",
			probe: func() ModelMenuProbe {
				p := healthyProbe()
				p.MenuItems = append(p.MenuItems, "4.0 Ultra Experimental")
				return p
			}(), deep: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drift, reasons, found, unexpected := classifyModelMenuDrift(expect, tc.probe, tc.deep)
			if drift != tc.wantDrift {
				t.Fatalf("drift=%v reasons=%v（want %v %v）", drift, reasons, tc.wantDrift, tc.wantReasons)
			}
			for _, want := range tc.wantReasons {
				if !containsStr(reasons, want) {
					t.Fatalf("缺少判据 %q（实得 %v）", want, reasons)
				}
			}
			if tc.wantDrift && len(tc.wantReasons) == 1 && len(reasons) > 1 {
				t.Fatalf("多报了判据: %v", reasons)
			}
			if !tc.wantDrift && len(reasons) != 0 {
				t.Fatalf("正常场景不应有判据: %v", reasons)
			}
			if tc.name == "正常深自检" && len(found) != len(expect) {
				t.Fatalf("命中档位=%v", found)
			}
			if tc.name == "新增档位不算漂移" && len(unexpected) != 1 {
				t.Fatalf("unexpected=%v", unexpected)
			}
		})
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestCurrentFamilyFromPickerLabel(t *testing.T) {
	cases := map[string]string{
		"Open mode picker, currently Flash":    "Flash",
		"Open mode picker, currently 3.1 Pro.": "3.1 Pro",
		"Flash":                                "Flash",
		"":                                     "",
	}
	for in, want := range cases {
		if got := currentFamily(in); got != want {
			t.Fatalf("currentFamily(%q)=%q want %q", in, got, want)
		}
	}
}

// setProbeEvaluator 让假引擎对模型菜单自检脚本返回固定采样，并记录收到的脚本。
func setProbeEvaluator(fake *fakeEngine, payload string, scripts *[]string) {
	fake.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if !strings.Contains(req.Value, modelMenuProbeMarker) {
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		*scripts = append(*scripts, req.Value)
		return ActionResult{Success: true, ExtractedData: payload}, nil
	}
}

func TestDiagModelMenuEndpoint(t *testing.T) {
	h, fake := newTestHandler(t, nil)
	var scripts []string
	setProbeEvaluator(fake, probeJSON(t, healthyProbe()), &scripts)
	rec := doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{"channel_id": "gemini-web", "session_id": "s-mm"})
	if rec.Code != http.StatusOK {
		t.Fatalf("建会话失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodGet, "/v1/diag/model-menu?channel=gemini-web&session=s-mm", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rep ModelMenuReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.PickerPresent || !rep.MenuOpened || rep.Drift || rep.Error != "" {
		t.Fatalf("健康页不应判漂移: %+v", rep)
	}
	if rep.CurrentFamily != "Flash" || len(rep.FoundFamilies) != 4 || !rep.MenuClosedOK {
		t.Fatalf("报告字段不对: %+v", rep)
	}
	if len(scripts) != 1 || !strings.Contains(scripts[0], "findPicker().click()") {
		t.Fatalf("默认应走深自检（真实开菜单）: %d", len(scripts))
	}

	// deep=0 必须退化为纯只读脚本（不点击）。
	scripts = nil
	rec = doJSON(t, h, http.MethodGet, "/v1/diag/model-menu?channel=gemini-web&session=s-mm&deep=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(scripts) != 1 || strings.Contains(scripts[0], "findPicker().click()") {
		t.Fatalf("deep=0 不得点击页面: %d %v", len(scripts), scripts)
	}
}

func TestDiagModelMenuDetectsDrift(t *testing.T) {
	h, fake := newTestHandler(t, nil)
	var scripts []string
	drifted := ModelMenuProbe{PickerPresent: true, PickerLabel: "Open mode picker, currently Flash",
		MenuOpened: true, MenuItems: []string{"3.5 Flash-Lite Fastest answers", "4.0 Ultra"}}
	setProbeEvaluator(fake, probeJSON(t, drifted), &scripts)
	doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{"channel_id": "gemini-web", "session_id": "s-mm2"})
	rec := doJSON(t, h, http.MethodGet, "/v1/diag/model-menu?channel=gemini-web&session=s-mm2", nil)
	var rep ModelMenuReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Drift || !containsStr(rep.DriftReasons, "family_missing:Pro") {
		t.Fatalf("应判漂移: %+v", rep)
	}
	if !containsStr(rep.UnexpectedItems, "4.0 Ultra") {
		t.Fatalf("应记录未知档位: %+v", rep.UnexpectedItems)
	}
}

func TestDiagModelMenuNoSession(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	rec := doJSON(t, h, http.MethodGet, "/v1/diag/model-menu?channel=gemini-web", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("无会话应 404: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodGet, "/v1/diag/model-menu?channel=deepseek-web", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "仅支持 gemini") {
		t.Fatalf("非 gemini 站点应显式拒绝: %d %s", rec.Code, rec.Body.String())
	}
}

func TestModelSwitchFailureCarriesMenuDigest(t *testing.T) {
	h, fake := newTestHandler(t, nil)
	var scripts []string
	setProbeEvaluator(fake, probeJSON(t, healthyProbe()), &scripts)
	fake.failAlways["gemini_model_switch"] = &EngineError{Code: codeInternal, Reason: "页面未就绪"}
	rec := doJSON(t, h, http.MethodPost, "/v1/sessions", map[string]any{"model": "gemini-flash", "session_id": "s-g3"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	s, _ := created["model_error"].(string)
	if !strings.Contains(s, "模型菜单自检") || !strings.Contains(s, "判定=正常") {
		t.Fatalf("失败应自带菜单现场: %v", created["model_error"])
	}
	if !strings.Contains(s, "Extended thinking") {
		t.Fatalf("摘要应含当时菜单内容: %v", created["model_error"])
	}
	if len(scripts) != 1 {
		t.Fatalf("失败即查应只跑一次自检: %d", len(scripts))
	}
}
