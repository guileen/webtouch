package fingerprint

import (
	"fmt"
	"strings"
)

// NeedsHookScript 判断当前档案是否需要通过 CDP 在文档最前端注入 JS Hook 脚本。
// 当配置了硬件参数伪装（HardwareConcurrency / DeviceMemory）或开启了任何渲染噪声时，
// 返回 true。
func (p *Profile) NeedsHookScript() bool {
	if p == nil {
		return false
	}
	if p.Emulation.Hardware.HardwareConcurrency > 0 || p.Emulation.Hardware.DeviceMemory > 0 ||
		(p.Emulation.Hardware.WebGL.Vendor != "" && p.Emulation.Hardware.WebGL.Renderer != "") {
		return true
	}
	if p.NoiseFeatures.Canvas2D || p.NoiseFeatures.AudioContext || p.NoiseFeatures.WebGLBuffer {
		return true
	}
	return false
}

// BuildHookScript 生成注入到 Chrome 的原生隐蔽 Hook 脚本。
// 严格遵守 docs/fingerprint-and-viewer-final-spec-2026-09-12.md §2.2 原生包装规范：
// 1. 原型链挂载在 Navigator.prototype，避免在 navigator 实例上直接赋值暴露 hasOwnProperty；
// 2. 所有 Getter 和被替换函数均经 makeNative 处理，重写 toString 返回 [native code]，杜绝 Function.prototype.toString 破功；
// 3. 噪声注入基于档案的 Seed 确定性生成，同 Seed 多次执行严格幂等稳定，不同 Seed 确定性分歧。
func (p *Profile) BuildHookScript() string {
	if p == nil || !p.NeedsHookScript() {
		return ""
	}

	seed := p.Seed
	if seed == 0 {
		seed = 0x5f3759df
	}

	var sb strings.Builder
	sb.WriteString("(() => {\n")
	sb.WriteString("  // 原型链隐蔽与原生化保护基础设施 (§2.2 规范)\n")
	sb.WriteString("  const origToString = Function.prototype.toString;\n")
	sb.WriteString("  const nativeFns = new WeakSet();\n")
	sb.WriteString("  const nativeNames = new WeakMap();\n\n")

	sb.WriteString("  const makeNative = (fn, name) => {\n")
	sb.WriteString("    nativeFns.add(fn);\n")
	sb.WriteString("    if (name) {\n")
	sb.WriteString("      nativeNames.set(fn, name);\n")
	sb.WriteString("      try { Object.defineProperty(fn, 'name', { value: name, configurable: true }); } catch (e) {}\n")
	sb.WriteString("    }\n")
	sb.WriteString("    return fn;\n")
	sb.WriteString("  };\n\n")

	sb.WriteString("  const customToString = function() {\n")
	sb.WriteString("    if (nativeFns.has(this)) {\n")
	sb.WriteString("      const name = nativeNames.get(this) || this.name || '';\n")
	sb.WriteString("      return `function ${name}() { [native code] }`;\n")
	sb.WriteString("    }\n")
	sb.WriteString("    return origToString.call(this);\n")
	sb.WriteString("  };\n")
	sb.WriteString("  makeNative(customToString, 'toString');\n\n")

	sb.WriteString("  try {\n")
	sb.WriteString("    Object.defineProperty(Function.prototype, 'toString', {\n")
	sb.WriteString("      value: customToString,\n")
	sb.WriteString("      writable: true,\n")
	sb.WriteString("      configurable: true,\n")
	sb.WriteString("      enumerable: false,\n")
	sb.WriteString("    });\n")
	sb.WriteString("  } catch (e) {}\n\n")

	sb.WriteString("  const overrideGetter = (proto, prop, valueGetter) => {\n")
	sb.WriteString("    if (!proto) return;\n")
	sb.WriteString("    const desc = Object.getOwnPropertyDescriptor(proto, prop) || {};\n")
	sb.WriteString("    const getterName = `get ${prop}`;\n")
	sb.WriteString("    const getter = makeNative(function() { return valueGetter(); }, getterName);\n")
	sb.WriteString("    Object.defineProperty(proto, prop, {\n")
	sb.WriteString("      get: getter,\n")
	sb.WriteString("      set: desc.set,\n")
	sb.WriteString("      enumerable: desc.enumerable !== undefined ? desc.enumerable : true,\n")
	sb.WriteString("      configurable: true,\n")
	sb.WriteString("    });\n")
	sb.WriteString("  };\n\n")

	// 1. Hardware Concurrency
	if p.Emulation.Hardware.HardwareConcurrency > 0 {
		fmt.Fprintf(&sb, "  // Hardware Concurrency\n")
		fmt.Fprintf(&sb, "  if (typeof Navigator !== 'undefined') {\n")
		fmt.Fprintf(&sb, "    overrideGetter(Navigator.prototype, 'hardwareConcurrency', () => %d);\n", p.Emulation.Hardware.HardwareConcurrency)
		fmt.Fprintf(&sb, "  }\n")
		fmt.Fprintf(&sb, "  if (typeof WorkerNavigator !== 'undefined') {\n")
		fmt.Fprintf(&sb, "    overrideGetter(WorkerNavigator.prototype, 'hardwareConcurrency', () => %d);\n", p.Emulation.Hardware.HardwareConcurrency)
		fmt.Fprintf(&sb, "  }\n\n")
	}

	// 2. Device Memory
	if p.Emulation.Hardware.DeviceMemory > 0 {
		fmt.Fprintf(&sb, "  // Device Memory\n")
		fmt.Fprintf(&sb, "  if (typeof Navigator !== 'undefined') {\n")
		fmt.Fprintf(&sb, "    overrideGetter(Navigator.prototype, 'deviceMemory', () => %g);\n", p.Emulation.Hardware.DeviceMemory)
		fmt.Fprintf(&sb, "  }\n")
		fmt.Fprintf(&sb, "  if (typeof WorkerNavigator !== 'undefined') {\n")
		fmt.Fprintf(&sb, "    overrideGetter(WorkerNavigator.prototype, 'deviceMemory', () => %g);\n", p.Emulation.Hardware.DeviceMemory)
		fmt.Fprintf(&sb, "  }\n\n")
	}

	// Seed & PRNG. mix32 是由 xor-shift 与奇数乘法组成的 32 位置换；它不截断
	// 种子熵，避免旧实现的低位/取模让相隔 8,192,000 的档案整体碰撞。
	fmt.Fprintf(&sb, "  const SEED = %d >>> 0;\n", seed)
	sb.WriteString("  const mix32 = (input) => {\n")
	sb.WriteString("    let x = input >>> 0;\n")
	sb.WriteString("    x ^= x >>> 16; x = Math.imul(x, 0x7feb352d);\n")
	sb.WriteString("    x ^= x >>> 15; x = Math.imul(x, 0x846ca68b);\n")
	sb.WriteString("    return (x ^ (x >>> 16)) >>> 0;\n")
	sb.WriteString("  };\n")
	sb.WriteString("  const noiseBit = (surface, index, channel) => (mix32(SEED ^ surface ^ Math.imul(index + 1, 0x9e3779b1)) >>> (channel & 31)) & 1;\n")
	sb.WriteString("  const noiseOffset = (surface, index) => ((mix32(SEED ^ surface ^ Math.imul(index + 1, 0x85ebca6b)) / 0xffffffff) - 0.5) * 2e-7;\n\n")

	// 3. Canvas 2D 确定性噪声
	if p.NoiseFeatures.Canvas2D {
		sb.WriteString("  // Canvas 2D 确定性噪声注入\n")
		sb.WriteString("  if (typeof CanvasRenderingContext2D !== 'undefined') {\n")
		sb.WriteString("    const origGetImageData = CanvasRenderingContext2D.prototype.getImageData;\n")
		sb.WriteString("    const hookedGetImageData = function(sx, sy, sw, sh) {\n")
		sb.WriteString("      const imgData = origGetImageData.apply(this, arguments);\n")
		sb.WriteString("      try {\n")
		sb.WriteString("        const d = imgData.data;\n")
		sb.WriteString("        if (d && d.length > 0) {\n")
		sb.WriteString("          const step = Math.max(4, (d.length >> 8) & ~3);\n")
		sb.WriteString("          for (let i = 0; i < d.length; i += step) {\n")
		sb.WriteString("            d[i] = (d[i] & ~1) | noiseBit(0x13579bdf, i >> 2, 0);\n")
		sb.WriteString("          }\n")
		sb.WriteString("        }\n")
		sb.WriteString("      } catch (e) {}\n")
		sb.WriteString("      return imgData;\n")
		sb.WriteString("    };\n")
		sb.WriteString("    makeNative(hookedGetImageData, 'getImageData');\n")
		sb.WriteString("    CanvasRenderingContext2D.prototype.getImageData = hookedGetImageData;\n")
		sb.WriteString("  }\n\n")

		sb.WriteString("  if (typeof HTMLCanvasElement !== 'undefined') {\n")
		sb.WriteString("    const origToDataURL = HTMLCanvasElement.prototype.toDataURL;\n")
		sb.WriteString("    const origToBlob = HTMLCanvasElement.prototype.toBlob;\n")
		sb.WriteString("    const copyWithNoise = (canvas) => {\n")
		sb.WriteString("      const ctx = canvas.getContext && canvas.getContext('2d');\n")
		sb.WriteString("      if (!ctx || canvas.width <= 0 || canvas.height <= 0 || typeof document === 'undefined') return null;\n")
		sb.WriteString("      const copy = document.createElement('canvas'); copy.width = canvas.width; copy.height = canvas.height;\n")
		sb.WriteString("      const copyCtx = copy.getContext('2d');\n")
		sb.WriteString("      const rawGet = CanvasRenderingContext2D.prototype.getImageData;\n")
		sb.WriteString("      const rawPut = CanvasRenderingContext2D.prototype.putImageData;\n")
		sb.WriteString("      const pixels = rawGet.call(ctx, 0, 0, canvas.width, canvas.height);\n")
		sb.WriteString("      for (let i = 0; i < pixels.data.length; i += 4) {\n")
		sb.WriteString("        if (pixels.data[i + 3] === 0) continue;\n")
		sb.WriteString("        const index = i >> 2;\n")
		sb.WriteString("        pixels.data[i] = (pixels.data[i] & ~1) | noiseBit(0x2468ace1, index, 1);\n")
		sb.WriteString("        pixels.data[i + 1] = (pixels.data[i + 1] & ~1) | noiseBit(0x2468ace1, index, 2);\n")
		sb.WriteString("      }\n")
		sb.WriteString("      rawPut.call(copyCtx, pixels, 0, 0); return copy;\n")
		sb.WriteString("    };\n")
		sb.WriteString("    const hookedToDataURL = function(...args) {\n")
		sb.WriteString("      try {\n")
		sb.WriteString("        const copy = copyWithNoise(this); if (copy) return origToDataURL.apply(copy, args);\n")
		sb.WriteString("      } catch (e) {}\n")
		sb.WriteString("      return origToDataURL.apply(this, args);\n")
		sb.WriteString("    };\n")
		sb.WriteString("    makeNative(hookedToDataURL, 'toDataURL');\n")
		sb.WriteString("    HTMLCanvasElement.prototype.toDataURL = hookedToDataURL;\n\n")

		sb.WriteString("    if (origToBlob) {\n")
		sb.WriteString("      const hookedToBlob = function(callback, ...args) {\n")
		sb.WriteString("        try {\n")
		sb.WriteString("          const copy = copyWithNoise(this); if (copy) return origToBlob.call(copy, callback, ...args);\n")
		sb.WriteString("        } catch (e) {}\n")
		sb.WriteString("        return origToBlob.call(this, callback, ...args);\n")
		sb.WriteString("      };\n")
		sb.WriteString("      makeNative(hookedToBlob, 'toBlob');\n")
		sb.WriteString("      HTMLCanvasElement.prototype.toBlob = hookedToBlob;\n")
		sb.WriteString("    }\n")
		sb.WriteString("  }\n\n")
	}

	// 4. AudioContext 确定性噪声
	if p.NoiseFeatures.AudioContext {
		sb.WriteString("  // AudioContext 确定性噪声注入\n")
		sb.WriteString("  const hookAudioCtx = (AudioCtxClass) => {\n")
		sb.WriteString("    if (!AudioCtxClass || !AudioCtxClass.prototype) return;\n")
		sb.WriteString("    const origStart = AudioCtxClass.prototype.startRendering;\n")
		sb.WriteString("    if (!origStart) return;\n")
		sb.WriteString("    const hookedStart = function() {\n")
		sb.WriteString("      return origStart.apply(this, arguments).then(buffer => {\n")
		sb.WriteString("        try {\n")
		sb.WriteString("          if (buffer && typeof buffer.getChannelData === 'function') {\n")
		sb.WriteString("            const chan = buffer.getChannelData(0);\n")
		sb.WriteString("            if (chan && chan.length > 0) {\n")
		sb.WriteString("              const step = Math.max(1, Math.floor(chan.length / 100));\n")
		sb.WriteString("              for (let i = 0; i < chan.length; i += step) {\n")
		sb.WriteString("                chan[i] += noiseOffset(0xa1b2c3d4, i);\n")
		sb.WriteString("              }\n")
		sb.WriteString("            }\n")
		sb.WriteString("          }\n")
		sb.WriteString("        } catch (e) {}\n")
		sb.WriteString("        return buffer;\n")
		sb.WriteString("      });\n")
		sb.WriteString("    };\n")
		sb.WriteString("    makeNative(hookedStart, 'startRendering');\n")
		sb.WriteString("    AudioCtxClass.prototype.startRendering = hookedStart;\n")
		sb.WriteString("  };\n")
		sb.WriteString("  if (typeof OfflineAudioContext !== 'undefined') hookAudioCtx(OfflineAudioContext);\n")
		sb.WriteString("  if (typeof webkitOfflineAudioContext !== 'undefined') hookAudioCtx(webkitOfflineAudioContext);\n\n")
	}

	// 5. WebGL Buffer 确定性噪声及 renderer 自报覆盖
	if p.NoiseFeatures.WebGLBuffer || (p.Emulation.Hardware.WebGL.Vendor != "" && p.Emulation.Hardware.WebGL.Renderer != "") {
		sb.WriteString("  // WebGL 确定性噪声与 renderer 自报覆盖\n")
		sb.WriteString("  const hookGL = (GLClass) => {\n")
		sb.WriteString("    if (!GLClass || !GLClass.prototype) return;\n")
		if p.NoiseFeatures.WebGLBuffer {
			sb.WriteString("    const origBufferData = GLClass.prototype.bufferData;\n")
			sb.WriteString("    if (origBufferData) {\n")
			sb.WriteString("      const hookedBufferData = function(target, data, usage) {\n")
			sb.WriteString("        try {\n")
			sb.WriteString("          if (data instanceof Float32Array && data.length > 0) {\n")
			sb.WriteString("            const copy = new Float32Array(data);\n")
			sb.WriteString("            const step = Math.max(1, Math.floor(copy.length / 50));\n")
			sb.WriteString("            for (let i = 0; i < copy.length; i += step) {\n")
			sb.WriteString("              copy[i] += noiseOffset(0xd4c3b2a1, i);\n")
			sb.WriteString("            }\n")
			sb.WriteString("            return origBufferData.call(this, target, copy, usage);\n")
			sb.WriteString("          }\n")
			sb.WriteString("        } catch (e) {}\n")
			sb.WriteString("        return origBufferData.apply(this, arguments);\n")
			sb.WriteString("      };\n")
			sb.WriteString("      makeNative(hookedBufferData, 'bufferData');\n")
			sb.WriteString("      GLClass.prototype.bufferData = hookedBufferData;\n")
			sb.WriteString("    }\n")
		}
		if p.Emulation.Hardware.WebGL.Vendor != "" && p.Emulation.Hardware.WebGL.Renderer != "" {
			fmt.Fprintf(&sb, "    const webglVendor = %q; const webglRenderer = %q;\n", p.Emulation.Hardware.WebGL.Vendor, p.Emulation.Hardware.WebGL.Renderer)
			sb.WriteString("    const origGetParameter = GLClass.prototype.getParameter;\n")
			sb.WriteString("    if (origGetParameter) {\n")
			sb.WriteString("      const hookedGetParameter = function(parameter) {\n")
			sb.WriteString("        if (parameter === 0x9245) return webglVendor;\n")
			sb.WriteString("        if (parameter === 0x9246) return webglRenderer;\n")
			sb.WriteString("        return origGetParameter.apply(this, arguments);\n")
			sb.WriteString("      };\n")
			sb.WriteString("      makeNative(hookedGetParameter, 'getParameter');\n")
			sb.WriteString("      GLClass.prototype.getParameter = hookedGetParameter;\n")
			sb.WriteString("    }\n")
		}
		sb.WriteString("  };\n")
		sb.WriteString("  if (typeof WebGLRenderingContext !== 'undefined') hookGL(WebGLRenderingContext);\n")
		sb.WriteString("  if (typeof WebGL2RenderingContext !== 'undefined') hookGL(WebGL2RenderingContext);\n\n")
	}

	sb.WriteString("})();\n")
	return sb.String()
}
