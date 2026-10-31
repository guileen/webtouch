package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"uws/pkg/fingerprint"
)

const phase3BaseProfile = `{
  "$schema": "https://webtouch.dev/schemas/fingerprint-profile.v1.json",
  "profile_id": "fp_p3_%d",
  "version": "1.0",
  "created_at": "2026-09-13T04:30:00Z",
  "seed": %d,
  "target_platform": {
    "os": "macOS",
    "os_version": "14.5",
    "architecture": "arm64",
    "chrome_version": ""
  },
  "emulation": {
    "user_agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
    "user_agent_metadata": {
      "brands": [
        {"brand": "Chromium", "version": "152"},
        {"brand": "Google Chrome", "version": "152"},
        {"brand": "Not/A)Brand", "version": "24"}
      ],
      "fullVersion": "152.0.0.0",
      "platform": "macOS",
      "platformVersion": "14.5.0",
      "architecture": "arm64",
      "model": "",
      "mobile": false
    },
    "screen": {
      "width": 1440,
      "height": 900,
      "device_scale_factor": 2.0,
      "color_depth": 24
    },
    "locale": {
      "timezone_id": "Asia/Shanghai",
      "languages": ["zh-CN", "zh", "en"]
    },
    "hardware": {
      "hardware_concurrency": 12,
      "device_memory": 16
    }
  },
  "noise_features": {
    "canvas_2d": true,
    "audio_context": true,
    "webgl_buffer": true
  },
  "network": {
    "proxy_url": ""
  }
}`

func TestFingerprintPhase3HardwareAndNativeProtection(t *testing.T) {
	requireCDP(t)

	profileJSON := fmt.Sprintf(phase3BaseProfile, 1, 12345678)
	profile, err := fingerprint.Parse([]byte(profileJSON))
	if err != nil {
		t.Fatalf("档案解析失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := (&CDPFactory{Headless: true}).Create(ctx, Config{
		TaskID:      "fingerprint-p3-native",
		Fingerprint: profile,
		InitialURL:  "about:blank",
	})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	defer func() { _ = session.Close() }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body><div id="content">test</div></body></html>`))
	}))
	defer server.Close()

	if err := session.Navigate(ctx, server.URL); err != nil {
		t.Fatalf("导航失败: %v", err)
	}

	raw := session.(*cdpSession)
	const evalScript = `(() => {
		const hwDesc = Object.getOwnPropertyDescriptor(Navigator.prototype, 'hardwareConcurrency');
		const memDesc = Object.getOwnPropertyDescriptor(Navigator.prototype, 'deviceMemory');
		return JSON.stringify({
			hw: navigator.hardwareConcurrency,
			mem: navigator.deviceMemory,
			hasOwnHw: Object.prototype.hasOwnProperty.call(navigator, 'hardwareConcurrency'),
			hasOwnMem: Object.prototype.hasOwnProperty.call(navigator, 'deviceMemory'),
			hwGetterToString: hwDesc && hwDesc.get ? hwDesc.get.toString() : "",
			hwGetterCallToString: hwDesc && hwDesc.get ? Function.prototype.toString.call(hwDesc.get) : "",
			memGetterToString: memDesc && memDesc.get ? memDesc.get.toString() : "",
			memGetterCallToString: memDesc && memDesc.get ? Function.prototype.toString.call(memDesc.get) : "",
			toStringToString: Function.prototype.toString.toString()
		});
	})()`

	var payload string
	if err := raw.evalValue(ctx, evalScript, &payload); err != nil {
		t.Fatalf("执行原生原型链探测失败: %v", err)
	}

	var res struct {
		HW                    int     `json:"hw"`
		Mem                   float64 `json:"mem"`
		HasOwnHW              bool    `json:"hasOwnHw"`
		HasOwnMem             bool    `json:"hasOwnMem"`
		HWGetterToString      string  `json:"hwGetterToString"`
		HWGetterCallToString  string  `json:"hwGetterCallToString"`
		MemGetterToString     string  `json:"memGetterToString"`
		MemGetterCallToString string  `json:"memGetterCallToString"`
		ToStringToString      string  `json:"toStringToString"`
	}
	if err := json.Unmarshal([]byte(payload), &res); err != nil {
		t.Fatalf("JSON 解析失败: %v, raw: %s", err, payload)
	}

	// 1. 硬件属性覆盖正确
	if res.HW != 12 {
		t.Errorf("hardwareConcurrency 期望 12，实际 %d", res.HW)
	}
	if res.Mem != 16 {
		t.Errorf("deviceMemory 期望 16，实际 %g", res.Mem)
	}

	// 2. 原型链隐蔽：绝不挂在 navigator 实例上（防止 hasOwnProperty 识破）
	if res.HasOwnHW {
		t.Errorf("navigator.hasOwnProperty('hardwareConcurrency') 应为 false，实际为 true")
	}
	if res.HasOwnMem {
		t.Errorf("navigator.hasOwnProperty('deviceMemory') 应为 false，实际为 true")
	}

	// 3. 原生函数伪装：Function.prototype.toString 无论直接调用还是 call 均输出 [native code]
	for name, str := range map[string]string{
		"HWGetterToString":      res.HWGetterToString,
		"HWGetterCallToString":  res.HWGetterCallToString,
		"MemGetterToString":     res.MemGetterToString,
		"MemGetterCallToString": res.MemGetterCallToString,
		"ToStringToString":      res.ToStringToString,
	} {
		if !strings.Contains(str, "[native code]") {
			t.Errorf("%s 未伪装成原生函数，暴露痕迹: %q", name, str)
		}
	}
}

func TestFingerprintPhase3CanvasAndAudioDeterminism(t *testing.T) {
	requireCDP(t)

	runProbe := func(seed uint32) (dataURL string, canvasHash string, audioSum float64, err error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		pJSON := fmt.Sprintf(phase3BaseProfile, seed, seed)
		profile, pErr := fingerprint.Parse([]byte(pJSON))
		if pErr != nil {
			return "", "", 0, pErr
		}

		session, cErr := (&CDPFactory{Headless: true}).Create(ctx, Config{
			TaskID:      fmt.Sprintf("fingerprint-p3-det-%d", seed),
			Fingerprint: profile,
			InitialURL:  "about:blank",
		})
		if cErr != nil {
			return "", "", 0, cErr
		}
		defer func() { _ = session.Close() }()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<!doctype html><html><body><canvas id="c" width="120" height="40"></canvas></body></html>`))
		}))
		defer server.Close()

		if nErr := session.Navigate(ctx, server.URL); nErr != nil {
			return "", "", 0, nErr
		}

		raw := session.(*cdpSession)

		const script = `(async () => {
			// 1. Canvas 2D 绘制与导出
			const canvas = document.getElementById('c');
			const ctx = canvas.getContext('2d');
			ctx.textBaseline = 'top';
			ctx.font = '16px sans-serif';
			ctx.fillStyle = '#f60';
			ctx.fillRect(5, 5, 50, 25);
			ctx.fillStyle = '#069';
			ctx.fillText('WebTouch P3', 10, 8);

			const beforeExport = Array.from(ctx.getImageData(0, 0, 120, 40).data).join(',');
			const durl = canvas.toDataURL();
			const afterExport = Array.from(ctx.getImageData(0, 0, 120, 40).data).join(',');
			const imgData = ctx.getImageData(0, 0, 120, 40);
			let canvasSum = 0;
			for (let i = 0; i < imgData.data.length; i++) {
				canvasSum = (canvasSum + imgData.data[i] * (i + 1)) % 1000000007;
			}

			// 2. AudioContext 渲染
			let audioSum = 0;
			try {
				const AudioCtx = window.OfflineAudioContext || window.webkitOfflineAudioContext;
				if (AudioCtx) {
					const actx = new AudioCtx(1, 4410, 44100);
					const osc = actx.createOscillator();
					osc.type = 'triangle';
					osc.frequency.setValueAtTime(1000, actx.currentTime);
					osc.connect(actx.destination);
					osc.start(0);
					const buffer = await actx.startRendering();
					const chan = buffer.getChannelData(0);
					for (let i = 0; i < chan.length; i++) {
						audioSum += chan[i];
					}
				}
			} catch (e) {}

			return JSON.stringify({
				dataURL: durl,
				canvasSum: canvasSum,
				audioSum: audioSum,
				canvasUnchanged: beforeExport === afterExport
			});
		})()`

		var payload string
		if evalErr := raw.evalValue(ctx, script, &payload); evalErr != nil {
			return "", "", 0, evalErr
		}

		var out struct {
			DataURL         string  `json:"dataURL"`
			CanvasSum       int64   `json:"canvasSum"`
			AudioSum        float64 `json:"audioSum"`
			CanvasUnchanged bool    `json:"canvasUnchanged"`
		}
		if uErr := json.Unmarshal([]byte(payload), &out); uErr != nil {
			return "", "", 0, uErr
		}
		if !out.CanvasUnchanged {
			return "", "", 0, fmt.Errorf("canvas.toDataURL 改写了业务画布")
		}

		h := sha256.Sum256([]byte(fmt.Sprintf("%d", out.CanvasSum)))
		return out.DataURL, hex.EncodeToString(h[:]), out.AudioSum, nil
	}

	// 运行同一 seed 两次：必须 100% 相同（确定性与稳定性）
	durl1, chash1, audio1, err := runProbe(1001)
	if err != nil {
		t.Fatalf("Probe 1 failed: %v", err)
	}
	durl2, chash2, audio2, err := runProbe(1001)
	if err != nil {
		t.Fatalf("Probe 2 failed: %v", err)
	}

	if durl1 != durl2 {
		t.Errorf("同 seed 下 Canvas toDataURL 产生漂移: run1=%s run2=%s", durl1, durl2)
	}
	if chash1 != chash2 {
		t.Errorf("同 seed 下 Canvas getImageData 哈希产生漂移: run1=%s run2=%s", chash1, chash2)
	}
	if audio1 != audio2 {
		t.Errorf("同 seed 下 Audio 采样和产生漂移: run1=%g run2=%g", audio1, audio2)
	}

	// 运行不同 seed：必须发生确定性分歧（不同身份隔离防关联）
	durlB, chashB, audioB, err := runProbe(2002)
	if err != nil {
		t.Fatalf("Probe B failed: %v", err)
	}

	if durl1 == durlB {
		t.Errorf("不同 seed 下 Canvas toDataURL 应当不同，实际完全一致")
	}
	if chash1 == chashB {
		t.Errorf("不同 seed 下 Canvas getImageData 应当不同，实际完全一致")
	}
	if audio1 == audioB {
		t.Errorf("不同 seed 下 Audio 采样和应当不同，实际完全一致: %g", audio1)
	}

	// 回归：旧算法把 Canvas 取低位、Audio/WebGL 取 seed%%1000，导致相隔
	// 8,192,000 的档案在所有三个表面碰撞；新混合函数不得重现该碰撞。
	durlFar, chashFar, audioFar, err := runProbe(1001 + 8192000)
	if err != nil {
		t.Fatalf("Probe far-seed failed: %v", err)
	}
	if durl1 == durlFar || chash1 == chashFar || audio1 == audioFar {
		t.Errorf("相隔 8,192,000 的 seed 不得在 Canvas/Audio 表面碰撞: durl=%t hash=%t audio=%t", durl1 == durlFar, chash1 == chashFar, audio1 == audioFar)
	}
}

func TestFingerprintPhase3SubIframeInheritance(t *testing.T) {
	requireCDP(t)

	profileJSON := fmt.Sprintf(phase3BaseProfile, 42, 99999)
	profile, err := fingerprint.Parse([]byte(profileJSON))
	if err != nil {
		t.Fatalf("档案解析失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := (&CDPFactory{Headless: true}).Create(ctx, Config{
		TaskID:      "fingerprint-p3-subiframe",
		Fingerprint: profile,
		InitialURL:  "about:blank",
	})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	defer func() { _ = session.Close() }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body><div id="root">parent</div></body></html>`))
	}))
	defer server.Close()

	if err := session.Navigate(ctx, server.URL); err != nil {
		t.Fatalf("导航失败: %v", err)
	}

	raw := session.(*cdpSession)
	const script = `(() => {
		const iframe = document.createElement('iframe');
		document.body.appendChild(iframe);
		const subWin = iframe.contentWindow;
		const subNav = subWin.navigator;
		const desc = subWin.Object.getOwnPropertyDescriptor(subWin.Navigator.prototype, 'hardwareConcurrency');
		return JSON.stringify({
			subHW: subNav.hardwareConcurrency,
			subMem: subNav.deviceMemory,
			subGetterToString: desc && desc.get ? desc.get.toString() : "",
			subGetterCallToString: desc && desc.get ? subWin.Function.prototype.toString.call(desc.get) : ""
		});
	})()`

	var payload string
	if err := raw.evalValue(ctx, script, &payload); err != nil {
		t.Fatalf("执行子 iframe 探测失败: %v", err)
	}

	var res struct {
		SubHW                 int     `json:"subHW"`
		SubMem                float64 `json:"subMem"`
		SubGetterToString     string  `json:"subGetterToString"`
		SubGetterCallToString string  `json:"subGetterCallToString"`
	}
	if err := json.Unmarshal([]byte(payload), &res); err != nil {
		t.Fatalf("解析子 iframe 数据失败: %v, raw: %s", err, payload)
	}

	if res.SubHW != 12 {
		t.Errorf("子 iframe 中 hardwareConcurrency 期望 12，实际 %d", res.SubHW)
	}
	if res.SubMem != 16 {
		t.Errorf("子 iframe 中 deviceMemory 期望 16，实际 %g", res.SubMem)
	}
	if !strings.Contains(res.SubGetterToString, "[native code]") {
		t.Errorf("子 iframe getter.toString 缺少 [native code]: %q", res.SubGetterToString)
	}
	if !strings.Contains(res.SubGetterCallToString, "[native code]") {
		t.Errorf("子 iframe getter call toString 缺少 [native code]: %q", res.SubGetterCallToString)
	}
}

func TestFingerprintPhase3WorkerInheritance(t *testing.T) {
	requireCDP(t)
	profile, err := fingerprint.Parse([]byte(fmt.Sprintf(phase3BaseProfile, 77, 424242)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	session, err := (&CDPFactory{Headless: true}).Create(ctx, Config{TaskID: "fingerprint-p3-worker", Fingerprint: profile, InitialURL: "about:blank"})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	defer func() { _ = session.Close() }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><title>worker</title>"))
	}))
	defer server.Close()
	if err := session.Navigate(ctx, server.URL); err != nil {
		t.Fatal(err)
	}
	var payload string
	err = session.(*cdpSession).evalValue(ctx, `new Promise((resolve, reject) => {
  const source = "postMessage(JSON.stringify({hw:navigator.hardwareConcurrency, mem:navigator.deviceMemory}))";
  const worker = new Worker(URL.createObjectURL(new Blob([source], {type:'text/javascript'})));
  worker.onmessage = event => { worker.terminate(); resolve(event.data); };
  worker.onerror = event => reject(new Error(event.message));
})`, &payload)
	if err != nil {
		t.Fatalf("Worker 身份探测失败: %v", err)
	}
	var got struct {
		HW  int     `json:"hw"`
		Mem float64 `json:"mem"`
	}
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("解析 Worker 探测失败: %v: %s", err, payload)
	}
	if got.HW != 12 || got.Mem != 16 {
		t.Errorf("Worker 未继承身份 Hook: hw=%d mem=%g", got.HW, got.Mem)
	}
}

func TestStateBundleLocalRoundTrip(t *testing.T) {
	requireCDP(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><title>state</title>"))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	first, err := (&CDPFactory{Headless: true}).Create(ctx, Config{TaskID: "state-export", InitialURL: "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if err := first.Navigate(ctx, server.URL); err != nil {
		t.Fatal(err)
	}
	if err := first.(*cdpSession).evalValue(ctx, `(() => { document.cookie = 'bundle_cookie=present; path=/'; localStorage.setItem('bundle_key', 'bundle_value'); sessionStorage.setItem('bundle_tab_key', 'bundle_tab_value'); return 'ok'; })()`, new(string)); err != nil {
		t.Fatal(err)
	}
	bundle, err := first.(*cdpSession).ExportStateBundle(ctx, "state-roundtrip", server.URL)
	if err != nil {
		t.Fatalf("export bundle: %v", err)
	}
	second, err := (&CDPFactory{Headless: true}).Create(ctx, Config{TaskID: "state-import", InitialURL: "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if err := second.(*cdpSession).ImportStateBundle(ctx, bundle); err != nil {
		t.Fatalf("import bundle: %v", err)
	}
	if err := second.Navigate(ctx, server.URL); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := second.(*cdpSession).evalValue(ctx, `JSON.stringify({cookie:document.cookie, storage:localStorage.getItem('bundle_key'), session:sessionStorage.getItem('bundle_tab_key')})`, &state); err != nil {
		t.Fatal(err)
	}
	if state != `{"cookie":"bundle_cookie=present","storage":"bundle_value","session":"bundle_tab_value"}` {
		t.Fatalf("restored state = %s", state)
	}
	// 导航之后再导入只能让脚本在下一次导航时生效；若仍报告成功，调用方会
	// 误以为当前页已经恢复，因此必须 fail-closed。
	if err := second.(*cdpSession).ImportStateBundle(ctx, bundle); !errors.Is(err, ErrStateBundleTooLate) {
		t.Fatalf("post-navigation import = %v, want first-navigation precondition error", err)
	}
}
