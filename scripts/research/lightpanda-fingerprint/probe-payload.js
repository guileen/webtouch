// WebTouch · 浏览器指纹面探针（研究用，只读、不改页面、不联网发数据）
//
// 配套 scripts/research/lightpanda-fingerprint/cdp-probe.mjs 使用：本文件被原样
// 作为 CDP `Runtime.evaluate` 的 expression 注入页面（awaitPromise=true），返回值是
// 一个 JSON 可序列化对象。同一份探针跑在不同浏览器上，即可做指纹面逐项对齐比对。
//
// 设计口径：只收集「网站能看到的稳定指纹面」，不追求完整；每一项都 try/catch 包裹，
// 缺能力时写 {error:...} 而不是让整份探针失败——研究目标是「缺什么」，不是「跑通」。
(async () => {
  const out = { collected_at: new Date().toISOString(), href: location.href, origin: location.origin };
  const rec = (key, fn) => {
    try { out[key] = fn(); } catch (e) { out[key] = { error: String((e && e.message) || e) }; }
  };
  const recAsync = async (key, fn) => {
    try { out[key] = await fn(); } catch (e) { out[key] = { error: String((e && e.message) || e) }; }
  };
  const hash32 = (str) => {
    let h = 0x811c9dc5;
    for (let i = 0; i < str.length; i++) { h ^= str.charCodeAt(i); h = (h * 0x01000193) >>> 0; }
    return ('00000000' + h.toString(16)).slice(-8);
  };
  const t = (v) => (typeof v === 'undefined' ? 'undefined' : v);

  // ── 1. 身份面（navigator） ───────────────────────────────────────
  rec('navigator', () => {
    const n = navigator;
    const plugins = n.plugins ? Array.from(n.plugins).slice(0, 8).map((p) => p.name) : null;
    return {
      userAgent: n.userAgent, appVersion: n.appVersion, appName: n.appName, appCodeName: n.appCodeName,
      product: n.product, productSub: n.productSub, vendor: n.vendor, vendorSub: n.vendorSub,
      platform: n.platform, language: n.language, languages: n.languages ? Array.from(n.languages) : null,
      webdriver: t(n.webdriver), hardwareConcurrency: t(n.hardwareConcurrency), deviceMemory: t(n.deviceMemory),
      maxTouchPoints: t(n.maxTouchPoints), pdfViewerEnabled: t(n.pdfViewerEnabled), cookieEnabled: t(n.cookieEnabled),
      onLine: t(n.onLine), doNotTrack: t(n.doNotTrack), globalPrivacyControl: t(n.globalPrivacyControl),
      plugins: { length: n.plugins ? n.plugins.length : null, names: plugins },
      mimeTypes: { length: n.mimeTypes ? n.mimeTypes.length : null, first: n.mimeTypes && n.mimeTypes[0] ? n.mimeTypes[0].type : null },
      userAgentData: n.userAgentData ? { brands: n.userAgentData.brands, mobile: n.userAgentData.mobile, platform: n.userAgentData.platform } : null,
    };
  });

  await recAsync('ua_ch_high_entropy', async () => {
    if (!navigator.userAgentData || !navigator.userAgentData.getHighEntropyValues) return { available: false };
    const v = await navigator.userAgentData.getHighEntropyValues([
      'architecture', 'bitness', 'model', 'platformVersion', 'uaFullVersion', 'fullVersionList', 'wow64', 'formFactors',
    ]);
    return { available: true, values: v };
  });

  // ── 2. 屏幕 / 视口 / 几何 ───────────────────────────────────────
  rec('screen', () => ({
    width: screen.width, height: screen.height, availWidth: screen.availWidth, availHeight: screen.availHeight,
    availLeft: t(screen.availLeft), availTop: t(screen.availTop),
    colorDepth: screen.colorDepth, pixelDepth: screen.pixelDepth,
    orientation: screen.orientation ? { type: screen.orientation.type, angle: screen.orientation.angle } : null,
    isExtended: t(screen.isExtended),
  }));
  rec('viewport', () => ({
    innerWidth: innerWidth, innerHeight: innerHeight, outerWidth: t(outerWidth), outerHeight: t(outerHeight),
    devicePixelRatio: t(devicePixelRatio), scrollX: t(scrollX), scrollY: t(scrollY),
    visualViewport: window.visualViewport ? { width: visualViewport.width, height: visualViewport.height, scale: visualViewport.scale } : null,
  }));

  // ── 3. 时区 / 语言 / Intl ───────────────────────────────────────
  rec('intl', () => ({
    timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
    locale: Intl.DateTimeFormat().resolvedOptions().locale,
    calendar: Intl.DateTimeFormat().resolvedOptions().calendar,
    numberingSystem: Intl.DateTimeFormat().resolvedOptions().numberingSystem,
    dateTimezoneOffsetMin: new Date().getTimezoneOffset(),
    dateToString: new Date(1700000000000).toString(),
    localeString: new Date(1700000000000).toLocaleString(),
  }));

  // ── 4. Canvas 2D ───────────────────────────────────────────────
  rec('canvas2d', () => {
    const c = document.createElement('canvas'); c.width = 240; c.height = 60;
    const ctx = c.getContext('2d');
    if (!ctx) return { available: false };
    ctx.textBaseline = 'top'; ctx.font = '14px "Arial"'; ctx.fillStyle = '#f60';
    ctx.fillRect(10, 10, 80, 20);
    ctx.fillStyle = '#069'; ctx.fillText('WebTouch ⋅ 指纹探针 0123', 12, 16);
    const g = ctx.createLinearGradient(0, 0, 240, 0); g.addColorStop(0, 'red'); g.addColorStop(1, 'blue');
    ctx.fillStyle = g; ctx.fillRect(0, 40, 240, 20);
    const url = c.toDataURL();
    const px = ctx.getImageData(0, 0, 240, 60).data;
    let sum = 0; for (let i = 0; i < px.length; i += 977) sum = (sum + px[i]) % 100000;
    return { available: true, dataUrlLength: url.length, dataUrlHead: url.slice(0, 24), hash: hash32(url), pixelSampleSum: sum, toBlob: typeof c.toBlob };
  });

  // ── 5. WebGL ───────────────────────────────────────────────────
  rec('webgl', () => {
    const c = document.createElement('canvas');
    const gl = c.getContext('webgl') || c.getContext('experimental-webgl');
    if (!gl) return { available: false };
    const dbg = gl.getExtension('WEBGL_debug_renderer_info');
    return {
      available: true,
      vendor: String(gl.getParameter(gl.VENDOR)), renderer: String(gl.getParameter(gl.RENDERER)),
      unmaskedVendor: dbg ? String(gl.getParameter(dbg.UNMASKED_VENDOR_WEBGL)) : null,
      unmaskedRenderer: dbg ? String(gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL)) : null,
      version: String(gl.getParameter(gl.VERSION)), shadingLanguageVersion: String(gl.getParameter(gl.SHADING_LANGUAGE_VERSION)),
      maxTextureSize: gl.getParameter(gl.MAX_TEXTURE_SIZE), maxViewportDims: Array.from(gl.getParameter(gl.MAX_VIEWPORT_DIMS) || []),
      extensionCount: (gl.getSupportedExtensions() || []).length,
      webgl2: typeof c.getContext('webgl2') === 'object' || !!c.getContext('webgl2'),
    };
  });

  // ── 6. Audio ───────────────────────────────────────────────────
  await recAsync('audio', async () => {
    const OAC = window.OfflineAudioContext || window.webkitOfflineAudioContext;
    if (!OAC) return { available: false };
    const ctx = new OAC(1, 44100, 44100);
    const osc = ctx.createOscillator(); osc.type = 'triangle'; osc.frequency.value = 10000;
    const comp = ctx.createDynamicsCompressor();
    osc.connect(comp); comp.connect(ctx.destination); osc.start(0);
    const buf = await ctx.startRendering();
    const d = buf.getChannelData(0);
    let s = 0; for (let i = 0; i < 5000; i++) s += Math.abs(d[i]);
    return { available: true, sum: Math.round(s * 1e7) / 1e7, sampleRate: buf.sampleRate, length: d.length, hash: hash32(d.slice(0, 4000).join(',')) };
  });

  // ── 7. 字体探测（宽度比对） ─────────────────────────────────────
  rec('fonts', () => {
    const canvas = document.createElement('canvas');
    const ctx = canvas.getContext('2d');
    if (!ctx) return { available: false };
    const measure = (family) => { ctx.font = `16px ${family}, monospace`; return ctx.measureText('mmmMMMwwwW 汉字 123').width; };
    const base = measure('monospace');
    const list = ['Arial', 'Helvetica', 'Helvetica Neue', 'Times New Roman', 'Courier New', 'Georgia', 'Verdana', 'Tahoma', 'Menlo', 'PingFang SC', 'Microsoft YaHei', 'Roboto'];
    const widths = {};
    for (const f of list) widths[f] = Math.round(measure(f) * 100) / 100;
    return { available: true, monospaceBase: Math.round(base * 100) / 100, widths, detectable: list.filter((f) => widths[f] !== Math.round(base * 100) / 100) };
  });

  // ── 8. 布局真实性（真排版引擎的硬证据） ─────────────────────────
  rec('layout', () => {
    const d = document.createElement('div');
    d.style.cssText = 'position:absolute;left:0;top:0;width:100px;height:50px;padding:5px;border:2px solid black;box-sizing:content-box';
    document.body.appendChild(d);
    const r = d.getBoundingClientRect();
    const cs = getComputedStyle(d);
    const res = {
      rect: { x: r.x, y: r.y, width: r.width, height: r.height },
      offsetWidth: t(d.offsetWidth), offsetHeight: t(d.offsetHeight), clientWidth: t(d.clientWidth),
      computedDisplay: cs.display, computedWidth: cs.width, computedBoxSizing: cs.boxSizing,
      elementFromPointHit: document.elementFromPoint(50, 25) === d,
    };
    d.remove();
    return res;
  });

  // ── 9. 媒体 / 解码 / 存储 ───────────────────────────────────────
  await recAsync('image_decode', async () => {
    const png = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';
    const img = new Image();
    const ok = await new Promise((res) => { img.onload = () => res(true); img.onerror = () => res(false); img.src = png; setTimeout(() => res('timeout'), 1500); });
    return { loaded: ok, naturalWidth: img.naturalWidth, naturalHeight: img.naturalHeight, hasCreateImageBitmap: typeof createImageBitmap };
  });
  await recAsync('media_devices', async () => {
    if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) return { available: false };
    const list = await navigator.mediaDevices.enumerateDevices();
    return { available: true, count: list.length, kinds: list.map((d) => d.kind) };
  });
  await recAsync('permissions', async () => {
    if (!navigator.permissions || !navigator.permissions.query) return { available: false };
    const names = ['geolocation', 'notifications', 'clipboard-read', 'camera', 'microphone'];
    const res = {};
    for (const n of names) { try { res[n] = (await navigator.permissions.query({ name: n })).state; } catch (e) { res[n] = 'ERR:' + e.name; } }
    return { available: true, states: res };
  });
  rec('storage', () => {
    const lsBefore = (() => { try { return localStorage.length; } catch (e) { return 'ERR'; } })();
    try { localStorage.setItem('__probe', '1'); } catch (e) { /* noop */ }
    try { document.cookie = '__probe=1; path=/'; } catch (e) { /* noop */ }
    return {
      localStorage: lsBefore, cookieAfterWrite: document.cookie, indexedDB: typeof indexedDB, caches: typeof caches,
      sessionStorage: typeof sessionStorage, quotaEstimate: !!(navigator.storage && navigator.storage.estimate),
    };
  });

  // ── 10. WebRTC 泄漏面（有界 2.5s） ──────────────────────────────
  await recAsync('webrtc', async () => {
    if (typeof RTCPeerConnection !== 'function') return { available: false };
    const pc = new RTCPeerConnection({ iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] });
    pc.createDataChannel('probe');
    const cands = [];
    pc.onicecandidate = (e) => { if (e.candidate) cands.push(e.candidate.candidate); };
    await pc.setLocalDescription(await pc.createOffer());
    await new Promise((r) => setTimeout(r, 2500));
    const sdp = pc.localDescription ? pc.localDescription.sdp : '';
    pc.close();
    return {
      available: true, candidateCount: cands.length, candidates: cands.slice(0, 6),
      sdpLength: sdp.length, hostLines: (sdp.match(/typ host/g) || []).length, srflxLines: (sdp.match(/typ srflx/g) || []).length,
    };
  });

  // ── 11. Native 性与自动化特征 ──────────────────────────────────
  rec('native_checks', () => {
    const ts = Function.prototype.toString;
    const desc = Object.getOwnPropertyDescriptor(Navigator.prototype, 'userAgent');
    return {
      toStringFetch: ts.call(fetch).slice(0, 60),
      toStringUserAgentGetter: desc && desc.get ? ts.call(desc.get).slice(0, 60) : null,
      toStringHasOwnProperty: ts.call(Object.prototype.hasOwnProperty).slice(0, 60),
      uaOnPrototypeNotOwn: (() => { try { return !Object.prototype.hasOwnProperty.call(navigator, 'userAgent'); } catch (e) { return 'ERR'; } })(),
      cdcKeys: Object.keys(window).filter((k) => /^cdc_|^\$cdc_|^__playwright|^__puppeteer|^__nightmare/.test(k)),
      chromeObject: typeof window.chrome,
      chromeRuntime: window.chrome ? typeof window.chrome.runtime : 'undefined',
      chromeLoadTimes: window.chrome ? typeof window.chrome.loadTimes : 'undefined',
    };
  });
  rec('error_stack', () => {
    const e = new Error('probe');
    return { head: (e.stack || '').split('\n').slice(0, 3), hasPrepareStackTrace: typeof Error.prepareStackTrace };
  });

  // ── 12. API 面覆盖率矩阵 ───────────────────────────────────────
  rec('api_surface', () => {
    const nav = (p) => p.split('.').reduce((o, k) => (o == null ? o : o[k]), navigator);
    const win = (p) => p.split('.').reduce((o, k) => (o == null ? o : o[k]), window);
    const checks = {
      'fetch': () => typeof fetch, 'XMLHttpRequest': () => typeof XMLHttpRequest, 'WebSocket': () => typeof WebSocket,
      'Worker': () => typeof Worker, 'SharedWorker': () => typeof SharedWorker, 'navigator.serviceWorker': () => typeof nav('serviceWorker'),
      'RTCPeerConnection': () => typeof RTCPeerConnection, 'WebGLRenderingContext': () => typeof WebGLRenderingContext,
      'WebGL2RenderingContext': () => typeof WebGL2RenderingContext, 'CanvasRenderingContext2D': () => typeof CanvasRenderingContext2D,
      'OffscreenCanvas': () => typeof OffscreenCanvas, 'AudioContext': () => typeof AudioContext,
      'OfflineAudioContext': () => typeof OfflineAudioContext, 'navigator.mediaDevices': () => typeof nav('mediaDevices'),
      'navigator.permissions': () => typeof nav('permissions'), 'navigator.geolocation': () => typeof nav('geolocation'),
      'navigator.clipboard': () => typeof nav('clipboard'), 'navigator.credentials': () => typeof nav('credentials'),
      'navigator.userAgentData': () => typeof nav('userAgentData'), 'navigator.sendBeacon': () => typeof nav('sendBeacon'),
      'navigator.storage': () => typeof nav('storage'), 'navigator.locks': () => typeof nav('locks'),
      'navigator.usb': () => typeof nav('usb'), 'navigator.bluetooth': () => typeof nav('bluetooth'),
      'navigator.hid': () => typeof nav('hid'), 'navigator.serial': () => typeof nav('serial'),
      'Notification': () => typeof Notification, 'SpeechSynthesis': () => typeof SpeechSynthesis, 'Gamepad': () => typeof Gamepad,
      'requestIdleCallback': () => typeof requestIdleCallback, 'crypto.subtle': () => typeof crypto.subtle,
      'PerformanceObserver': () => typeof PerformanceObserver, 'IntersectionObserver': () => typeof IntersectionObserver,
      'ResizeObserver': () => typeof ResizeObserver, 'MutationObserver': () => typeof MutationObserver,
      'Intl': () => typeof Intl, 'WebAssembly': () => typeof WebAssembly, 'SharedArrayBuffer': () => typeof SharedArrayBuffer,
      'CSS.supports': () => typeof CSS.supports, 'matchMedia': () => typeof matchMedia, 'customElements': () => typeof customElements,
      'HTMLDialogElement': () => typeof HTMLDialogElement, 'indexedDB': () => typeof indexedDB,
      'structuredClone': () => typeof structuredClone, 'queueMicrotask': () => typeof queueMicrotask,
      'FileReader': () => typeof FileReader, 'Blob': () => typeof Blob, 'URL.createObjectURL': () => typeof URL.createObjectURL,
      'WebAuthn(PublicKeyCredential)': () => typeof PublicKeyCredential, 'AbortController': () => typeof AbortController,
      'window.chrome': () => typeof window.chrome, 'document.startViewTransition': () => typeof document.startViewTransition,
    };
    const table = {}; let impl = 0; let total = 0;
    for (const [k, fn] of Object.entries(checks)) {
      let v; try { v = fn(); } catch (e) { v = 'ERR'; }
      const ok = v !== 'undefined';
      if (ok) impl++; total++;
      table[k] = v;
    }
    return { implemented: impl, total, ratio: Math.round((impl / total) * 1000) / 1000, table };
  });

  rec('dom_css', () => {
    const el = document.createElement('div');
    el.innerHTML = '<span id="s1">x</span>';
    document.body.appendChild(el);
    const supports = (p, v) => { try { return CSS.supports(p, v); } catch (e) { return 'ERR'; } };
    const r = {
      querySelectorFound: !!document.querySelector('#s1'),
      childElementCount: el.childElementCount,
      textContent: el.textContent,
      supportsGrid: supports('display', 'grid'),
      supportsBackdropFilter: supports('backdrop-filter', 'blur(4px)'),
      supportsFlex: supports('display', 'flex'),
      localStorageRoundTrip: (() => { try { localStorage.setItem('k', 'v'); return localStorage.getItem('k'); } catch (e) { return 'ERR'; } })(),
      iframeSupport: typeof HTMLIFrameElement,
    };
    el.remove();
    return r;
  });

  return out;
})()
