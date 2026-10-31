package fingerprint

import (
	"strings"
	"testing"
)

func TestNeedsHookScript(t *testing.T) {
	var empty Profile
	if empty.NeedsHookScript() {
		t.Fatal("空档案不应当需要 Hook 脚本")
	}

	hwOnly := Profile{
		Emulation: Emulation{
			Hardware: Hardware{HardwareConcurrency: 8},
		},
	}
	if !hwOnly.NeedsHookScript() {
		t.Fatal("设置了 HardwareConcurrency 应当需要 Hook 脚本")
	}

	memOnly := Profile{
		Emulation: Emulation{
			Hardware: Hardware{DeviceMemory: 8},
		},
	}
	if !memOnly.NeedsHookScript() {
		t.Fatal("设置了 DeviceMemory 应当需要 Hook 脚本")
	}

	canvasOnly := Profile{
		NoiseFeatures: NoiseFeatures{Canvas2D: true},
	}
	if !canvasOnly.NeedsHookScript() {
		t.Fatal("开启了 Canvas2D 应当需要 Hook 脚本")
	}

	audioOnly := Profile{
		NoiseFeatures: NoiseFeatures{AudioContext: true},
	}
	if !audioOnly.NeedsHookScript() {
		t.Fatal("开启了 AudioContext 应当需要 Hook 脚本")
	}

	glOnly := Profile{
		NoiseFeatures: NoiseFeatures{WebGLBuffer: true},
	}

	webglIdentity := Profile{Emulation: Emulation{Hardware: Hardware{WebGL: WebGL{Vendor: "WebTouch", Renderer: "Stable Renderer"}}}}
	if !webglIdentity.NeedsHookScript() {
		t.Fatal("设置了 WebGL vendor/renderer 应当需要 Hook 脚本")
	}
	if !glOnly.NeedsHookScript() {
		t.Fatal("开启了 WebGLBuffer 应当需要 Hook 脚本")
	}
}

func TestBuildHookScriptStructureAndProtection(t *testing.T) {
	p := Profile{
		Seed: 1234567,
		Emulation: Emulation{
			Hardware: Hardware{
				HardwareConcurrency: 12,
				DeviceMemory:        16,
				WebGL:               WebGL{Vendor: "WebTouch Vendor", Renderer: "WebTouch Renderer"},
			},
		},
		NoiseFeatures: NoiseFeatures{
			Canvas2D:     true,
			AudioContext: true,
			WebGLBuffer:  true,
		},
	}

	script := p.BuildHookScript()
	if script == "" {
		t.Fatal("BuildHookScript 生成了空脚本")
	}

	// 必须包含原生化基础设施与 [native code]
	for _, want := range []string{
		"const makeNative =",
		"[native code]",
		"overrideGetter",
		"Navigator.prototype",
		"WorkerNavigator.prototype",
		"hardwareConcurrency",
		"() => 12",
		"deviceMemory",
		"() => 16",
		"const SEED = 1234567 >>> 0;",
		"CanvasRenderingContext2D.prototype.getImageData",
		"HTMLCanvasElement.prototype.toDataURL",
		"OfflineAudioContext",
		"startRendering",
		"WebGLRenderingContext",
		"bufferData",
		"mix32",
		"copyWithNoise",
		"getParameter",
		"0x9245",
		"WebTouch Vendor",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("Hook 脚本缺少预期内容: %q", want)
		}
	}
}
