# WebTouch 浏览器指纹全套方案与展开态真窗口控制最终架构规范

> 生效日期：2026-09-12  
> 归属：`agent-tools/webtouch`（WebTouch 引擎与 Viewer 交互底座）  
> 面向对象：引擎研发、协调器开发者、架构评审者

---

## 0. 方案一句话与核心定位

在**不维护自编译 Chromium 分支**（避免沉重编译包袱与破坏现有登录态复用）的前提下，基于官方 **Google Chrome / Chrome for Testing**，通过 **「CDP 协议覆盖 + 原型链 Hook + 跨端轻量状态包（State Bundle）+ 原生轻量收回胶囊」**，提供有明确测试范围和失败边界的浏览器身份与真实交互底座；它不是、也不宣称是风控绕过保证。

---

## 1. 浏览器指纹能力边界矩阵（能做到什么程度）

对于反爬与风控检测，本方案明确划分「能做到」、「能对抗到哪一层」与「绝对做不到」的物理边界：

| 维度 / 层级 | 核心对抗目标 | 技术手段 | 能做到的上限 | 诚实边界（做不到/不可行） |
|---|---|---|---|---|
| **L1 网络传输与握手** | TLS JA3/JA4 指纹、HTTP/2 帧指纹、出口 IP 与地理位置对齐 | 锁定官方 Chrome 版本；代理透传；CDP 时区与语言覆盖 | 在没有 TLS MITM 时保留该官方 Chrome 进程自身的 TLS/H2 行为；可让浏览器声明的时区/语言与档案一致 | 不能证明代理出口、地理位置或 OS 网络栈与档案一致；MITM、扩展和网络中间件均会改变可观测行为 |
| **L2 浏览器身份标示** | User-Agent、Client Hints (UA-CH) | CDP `Network.setUserAgentOverride` 配合结构化 `userAgentMetadata` 字段 | 已测字段可保持 UA、UA-CH、语言、时区之间的一致 | 未覆盖的 UA-CH/版本组合、服务器侧网络信号仍可能矛盾；不以“无”作为边界 |
| **L3 自动化特征清除** | `navigator.webdriver`、常见 WebDriver 特征 | `--disable-blink-features=AutomationControlled`；Go CDP 直连；有限原型链 Hook | 可消除已测试的 `navigator.webdriver` 与 chromedriver `cdc_` 路径 | CDP 连接、行为节律、调试副作用和未测试探针仍可被检测；不承诺绕过任何挑战系统 |
| **L4 硬件与运行时环境** | 屏幕/视口、硬件并发数、设备内存、GPU 字符串 | CDP `Emulation.setDeviceMetricsOverride`；原型链 Hook；WebGL 参数覆盖 | 已覆盖的 DOM/CSS 与 WebGL 字符串可按档案稳定呈现 | 无法改变真实 GPU、驱动、扩展、字体、色彩管理及多屏窗口几何；覆盖字段之外不作一致性承诺 |
| **L5 动态渲染指纹扰动** | Canvas 2D、AudioContext、部分 WebGL 读取 | PRNG seed；在指定数据导出点注入低位噪声 | 同 seed 在已测读取点可重复，不同 seed 会产生可验证差异，且不修改业务 Canvas | 不能保证不同身份不可关联；字体、合成器、WebGL 扩展与未 Hook API 仍会暴露真实环境 |
| **L6 会话凭证与登录态** | Cookies、指定 origin 的 LocalStorage 与当前 tab 的 SessionStorage | CDP `Network.getAllCookies` 与导航前结构化恢复 | 避开跨 OS 直接复制 Chromium 加密数据库的不可移植性；本地 Chrome 回归覆盖 Cookies/LocalStorage/SessionStorage 往返 | **不导出 IndexedDB、Cache、Service Worker 或二进制 profile 数据**；SessionStorage 仅在恢复所用的同一 tab 有效；站点可绑定 IP、设备、刷新令牌或风控状态，恢复不保证登录成功 |

> **防护能力定论**：  
> 本方案是降低常见自动化信号和维持可控浏览器身份一致性的工程底座，不能证明、更不能承诺绕过 Cloudflare Turnstile、DataDome、FingerprintJS 或其他商业风控。挑战、验证码、登录限制均应停下交由用户处理。  
> **不承诺**：跨操作系统伪装物理芯片/字体/驱动、跨端登录成功，或对抗任何高风险设备指纹系统。

---

## 2. 浏览器指纹工程架构设计

### 2.1 指纹档案（Fingerprint Profile）契约

每个虚拟身份抽象为一个纯数据描述文件（JSON 格式），可版本化、可落盘存储、可网络下发：

```json
{
  "$schema": "https://webtouch.dev/schemas/fingerprint-profile.v1.json",
  "profile_id": "fp_dev_mac_01",
  "version": "1.0.0",
  "created_at": "2026-09-12T14:55:00Z",
  "seed": 894120571,
  "target_platform": {
    "os": "macOS",
    "os_version": "14.5.0",
    "architecture": "arm64",
    "chrome_version": "128.0.6613.119"
  },
  "emulation": {
    "user_agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    "user_agent_metadata": {
      "brands": [
        {"brand": "Chromium", "version": "128"},
        {"brand": "Google Chrome", "version": "128"},
        {"brand": "Not=A?Brand", "version": "24"}
      ],
      "fullVersion": "128.0.6613.119",
      "platform": "macOS",
      "platformVersion": "14.5.0",
      "architecture": "arm",
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
      "hardware_concurrency": 8,
      "device_memory": 8,
      "webgl": {
        "vendor": "Apple",
        "renderer": "Apple M2"
      }
    }
  },
  "noise_features": {
    "canvas_2d": true,
    "audio_context": true,
    "webgl_buffer": true
  },
  "network": {
    "proxy_url": "socks5://127.0.0.1:10808",
    "webrtc_policy": "disable_non_proxied_udp"
  }
}
```

`network` 段两条硬边界（2026-09-13 外站验收后补第 2 条）：

1. `proxy_url` **不许内嵌用户名/密码**（Chrome `--proxy-server` 不支持，放行＝以为换了出口其实没换）；
   `socks5h://` 接受并归一化成 `socks5://`。
2. `webrtc_policy` 可选，落到 Chrome 的 `--force-webrtc-ip-handling-policy=<值>`（取值与 Chrome 枚举逐字对齐）。
   **但实测它当前无效**：三次对照实验里换两个取值，BrowserLeaks 看到的东西一模一样
   → 所以不做「绑代理就自动加旗标」的默认，只保留显式字段（将来 Chrome 生效即自动获益），
   并把 `WebRTCLeakWarning()` 的实情念给使用者听。真正的解法是扩展策略或系统级 TUN，见 §7.5 缺口①。

### 2.2 原型链隐蔽 Hook 规范（Anti-Tamper & Clean Prototype）

所有在页面注入的 JS 脚本（通过 CDP `Page.addScriptToEvaluateOnNewDocument` 注入）**必须**通过 Native 隐蔽函数包装，杜绝通过 `Function.prototype.toString` 暴露痕迹：

```javascript
// 原型链保护基础设施
const makeNative = (fn, name) => {
  const toString = () => `function ${name || fn.name || ''}() { [native code] }`;
  Object.defineProperty(fn, 'toString', {
    value: toString,
    configurable: true,
    writable: true
  });
  return fn;
};

// 属性伪装
const overrideGetter = (obj, prop, valueGetter) => {
  const originalDesc = Object.getOwnPropertyDescriptor(obj, prop) || {};
  const getter = makeNative(function() { return valueGetter(); }, `get ${prop}`);
  Object.defineProperty(obj, prop, {
    get: getter,
    set: originalDesc.set,
    enumerable: originalDesc.enumerable !== undefined ? originalDesc.enumerable : true,
    configurable: true
  });
};
```

1. **Hardware 属性**：
   * `overrideGetter(navigator, 'hardwareConcurrency', () => profile.hardware.hardware_concurrency);`
   * `overrideGetter(navigator, 'deviceMemory', () => profile.hardware.device_memory);`
2. **Canvas 2D 噪点注入**：
   * 基于 `profile.seed` 生成确定性 PRNG；
   * Hook `CanvasRenderingContext2D.prototype.getImageData`，在提取 ImageData 时对 Alpha 通道或最低有效位进行 `±1` 的可控微扰；
   * Hook `HTMLCanvasElement.prototype.toDataURL`，通过中间 Canvas 加上述扰动后再输出。
3. **AudioContext 噪点注入**：
   * Hook `OfflineAudioContext.prototype.startRendering`，对返回的 `AudioBuffer.getChannelData()` 在特定频段注入 `1e-7` 级别的确定性微小白噪声。
4. **WebGL 伪装**：
   * Hook `WebGLRenderingContext.prototype.getParameter`，当查询 `UNMASKED_VENDOR_WEBGL` (0x9245) 和 `UNMASKED_RENDERER_WEBGL` (0x9246) 时，分别返回指纹配置中的值。

---

### 2.3 身份注册表：账号 ↔ 身份 ↔ 代理（2026-09-12 用户追加需求，同轮落地）

> 用户追加口径：「哪怕迁移，本质上隐含一个结论：可能对应账号和浏览器指纹的绑定，以及多浏览器
> 指纹的需求，以及多浏览器指纹和浏览器 SOCKS5 代理绑定的需求（或者其他类型的代理）。」

一句话：**一个账号 = 一份指纹 = 一条出口**。这三件事必须由同一个地方说了算，否则迟早串号。

目录布局（默认 `~/.uws/identities`，`UWS_IDENTITY_ROOT` 可覆盖；实现 `pkg/fingerprint/registry.go`）：

```text
~/.uws/identities/<profile_id>.json   身份档案本体（事实源：UA/UA-CH/时区/语言/屏幕/代理）
~/.uws/identities/bindings.json       账号 → 身份 的绑定（**只放关系**，不复制档案字段）
~/.uws/profiles/<profile_id>/         这个身份的浏览器物理状态（Cookie/LocalStorage/扩展）
```

三条不变式（`Registry.Bind` 在同一身份目录内以系统自动释放的 `flock` 串行化所有注册表写入，
`Conflicts()` 可发现磁盘上的损坏；跨节点原子互斥仍由 Phase 4 Profile Lease 负责）：

1. **一个账号只绑一个身份**——改绑必须先 `unbind`，不许悄悄漂移；
2. **一个身份只被一个账号绑**——两个账号共用同一份指纹，等于直接告诉平台「这俩是同一个人」；
3. **身份名 = 文件名 = 档案里的 `profile_id`**——三者不一致就是数据已经坏了（加载即报错）。

`Registry.Conflicts()`（`uws identity check`）另外报出**软冲突**：多个身份共用同一条代理出口配置
（出口 IP 是平台侧最强的关联信号之一，不是硬错误但必须让人看见）、绑定指向已消失的身份；也会报出
绑定表被外部篡改后出现的重复身份。

代理：写在档案的 `network.proxy_url` 里，由 `pkg/fingerprint` 变成浏览器启动参数
`--proxy-server=...`；已支持 `socks5 / socks5h / socks4 / http / https` 五种写法（其他 scheme 直接拒绝）。
代理与身份是 1:1 的**绑定关系**，不是「运行时选项」——所以它放在档案里而不是命令行上。

两条硬边界（2026-09-13 评审吸收）：

- **不允许在 URL 里内嵌用户名/密码**（`socks5://user:pass@host:port`）：Chrome 的 `--proxy-server` 不支持凭据，
  放行它等于「档案里写着代理、日志里也写着走了代理，实际却没走」——用户以为换了出口。`Validate()` 因此直接拒绝，
  并要求把带凭据的代理先在本机收敛成本地中继（127.0.0.1 转发），档案里只填中继地址。
- **`socks5h://` 接受但归一化成 `socks5://`**：`socks5h` 是 curl 一类工具「让代理解析域名」的写法，Chrome 只认
  `socks5://`；Chromium 的 SOCKS5 客户端本来就把域名交给代理（`ATYP=domain`），不在本地先解析，所以这里做等价映射，
  不引入第二种传输、也不假装能单独配置 DNS 行为。

CLI（`uws identity ...`，`cmd/uws/identity.go`）：

```sh
uws identity list                      # 有哪些身份：平台/时区/代理/绑了哪个账号（凭据脱敏）
uws identity bind <账号> <profile-id>  # 账号 ↔ 身份（1:1）
uws identity resolve <账号>            # 账号 → 身份 → 可直接执行的运行命令
uws identity use <profile-id>          # 打印带 --profile-id/--fingerprint 的运行命令（代理凭据脱敏）
uws identity check                     # 共用代理出口 / 悬空绑定
```

诚实边界：注册表保证的是**绑定关系与档案自洽**；它管不了「你实际是不是从那条代理出去的」
（那要真机核对出口 IP），也管不了跨端物理层一致（那是 §3/§4 的 State Bundle + 租约）。

## 3. 跨端会话凭证同步与租约（解开 OS Crypt 死穴）

### 3.1 跨端数据包：State Bundle 契约

针对 Chromium 在 macOS（Keychain）、Windows（DPAPI）、Linux（SecretService）无法直接拷贝物理 Profile 目录的痛点，会话同步采用**纯逻辑状态包**结构：

```json
{
  "$schema": "https://webtouch.dev/schemas/state-bundle.v1.json",
  "profile_id": "fp_dev_mac_01",
  "created_at": "2026-09-12T14:58:00Z",
  "cookies": [
    {
      "name": "session_id",
      "value": "s_123456789",
      "domain": ".feishu.cn",
      "path": "/",
      "secure": true,
      "httpOnly": true,
      "sameSite": "Lax",
      "expires": 1757689080
    }
  ],
  "origins": [{
    "origin": "https://feishu.cn",
    "local_storage": [{"key": "user_pref_theme", "value": "dark"}],
    "session_storage": [{"key": "active_workspace", "value": "main"}]
  }]
}
```

### 3.2 导出与恢复时序

```text
[源端 Engine]                                          [目标端 Engine]
      │                                                        │
1. 任务暂停 / 准备切端                                         │
2. CDP: Network.getAllCookies ────────────────┐                │
3. Script: 提取各域名 LocalStorage/当前 Tab SessionStorage ────┤ │
4. 打包为 StateBundle.json (明文/AES加密) ──────┼───────────────► 5. 收到 StateBundle
6. 释放租约 (Lease Release)                    │                7. 申请租约 (Lease Acquire)
                                               │                8. 启动干净 Chrome (装配指纹参数)
                                               │                9. CDP: Network.setCookies(cookies)
                                               │                10. 导航前注入 LocalStorage / SessionStorage
                                               │                11. 导航目标页，由目标站点决定是否接受恢复的会话
```

CLI 的目标端默认路径是 `uws state restore <bundle> --profile-file <FingerprintProfile.json> --session <new-session> [--key-file <key>]`：它先确认本地 profile 目录为空，再以同一档案初始化尚未导航的上下文并恢复状态；随后才以同一 `--session` 交给 `uws run` / `uws goal` 导航。已有目录默认拒绝，必须由操作者确认后显式加 `--allow-existing-profile`。`export`、`inspect` 与低层 `import --target-id` 也可用。所有命令均不回显 Cookie、Bundle 内容或密钥；导出文件权限为 `0600`。不带 `--key-file` 的明文包仅适合受控临时环境。

### 3.3 单写者租约机制（Profile Lease）

* 针对每个 `profile_id`，设立单写者锁。
* 租约包含：`holder_id`, 不可打印的随机 `token`, `issued_at`, `expires_at`；续期与释放均须持有同一 token（默认 TTL 60s）。
* 任何节点在启动指定 `profile_id` 前，必须先抢占租约；若已被抢占且心跳存活，则拒绝启动，防止本地磁盘缓存/远程会话相互踩踏。
* 本地后端 `LeaseStore` 仅适用于具备 `O_EXCL` 原子创建语义的单一文件系统；同步盘/最终一致对象存储不安全。
* 跨地域可部署 `bin/uws-lease`：它以同一强一致目录为事实源，提供 bearer-token 认证的 acquire/renew/release HTTP API；远程节点以 `uws-engine --lease-endpoint https://… --lease-token-file … --lease-holder-id <stable-node-id>` 接入。`uws-lease --tls-cert <cert> --tls-key <key>` 可直接提供 TLS（最低 TLS 1.2），并在绑定端口前加载、校验证书对；非回环明文监听会被拒绝，也可在私有网络的 TLS 终止层后运行。客户端拒绝非 loopback 的明文 HTTP。租约服务本身不接收或保存 Cookie、State Bundle 或密钥。

### 3.4 引擎接线与密钥边界（v1.5.7）

* `InitializeContext` 可携带 `profile_id` 与完整 `fingerprint_profile_json`；引擎严格解析档案，二者不一致即拒绝，避免目标端只恢复状态却漏掉身份覆盖。
* `ImportState` 接收结构化明文 `StateBundle`，仅可作用于已初始化、带有**同一 `profile_id`**的真实 CDP 会话；它在首个导航前写 Cookie 并注册 origin-scoped LocalStorage 与当前 tab SessionStorage 新文档脚本。首次导航已经发生、模拟/第三方不支持会话、未指定身份或身份不匹配必须返回 `FAILED_PRECONDITION`。
* AES-256-GCM 信封在可信客户端或密钥存储处解封后才可调用该 RPC。密钥、密文和 Cookie 值不进入引擎日志、错误响应或协议调试输出。
* `engine.Options{LeaseStore, LeaseHolderID, LeaseTTL}` 接受本地或 HTTP `LeaseBackend`，并启用 `LeaseFactory`：持久 profile 创建前 acquire、存活期间按 TTL/2 renew、最后一个会话关闭时 release；重启中的新旧 session 共享同一进程租约引用。续期丢失会关闭关联浏览器，禁止双写。两节点通过本地真实 TLS 服务竞争/释放的回归已覆盖协议与传输路径；默认未配置 Store 时保持单机旧行为。

### 3.5 同身份纯 HTTP 请求（2026-09-13）

`uws request <url> --profile-file <FingerprintProfile.json> [--state <StateBundle.json> --key-file <key>]` 为不需渲染或执行 JavaScript 的接口提供 Go HTTP client 入口，且**不启动 Chrome**。它仅复用同一档案的 User-Agent、Accept-Language、低熵 UA Client Hints 和同一 `profile_id` State Bundle 的 Cookie；`Authorization`、`Accept`、`Content-Type`、`Referer`、`Origin` 等请求上下文由调用方以 `--header` 明确提供。Cookie、UA、语言、UA Client Hints、Host 和代理认证禁止手工覆盖，避免身份自相矛盾；`--follow` 最多 10 次但只允许同 origin（scheme、host 与规范化端口；省略的 HTTP/HTTPS 默认端口等价）跳转，每一跳重新施加档案身份头，Bundle 与服务端新设 Cookie 仍仅按 jar 的域/路径范围携带。跨 origin 跳转一律拒绝，因为 Go client 不能重建浏览器 SameSite/发起页上下文。

SessionStorage 没有 HTTP 线上的自动等价物；分区 Cookie（CHIPS）也不能安全放入 Go 的 Cookie jar，遇到它们即拒绝，必须回到浏览器。档案中的 HTTP/HTTPS/SOCKS5 代理会复用；SOCKS4 明确拒绝而非直连降级。高熵 hints 只在调用方确认目标已用 `Accept-CH` 协商后用 `--client-hints full`。

边界不可模糊：这是同身份的 **HTTP 可见层**，不是 Chrome 网络栈。Go 的 TLS、HTTP/2、连接复用与 JS/WebRTC 行为仍是 Go；任何绑定这些传输属性、需要页面 JS 或分区 Cookie 的请求必须留在真实 Chrome。

---

## 4. 展开态真窗口控制与交互闭环（Viewer S5 完整方案）

### 4.1 设计原则

1. **绝对不注入真实 DOM**：页面内部保持零污染，防风控探知与页面排版干扰；
2. **绝对不抢键盘焦点**：展开前后均遵循非激活规则，用户外部操作不被抢夺；
3. **坚决废弃 CDP 轮询拖拽跟随**：不搞秒级延迟的“跟屁虫”外挂条，避免 UI 抖动与遮挡网页右上角功能区。

### 4.2 双轨收回机制（Auto-Collapse + Float Capsule）

```text
┌──────────────────────────────────────────────────────────────┐
│  macOS 屏幕                                                  │
│                                                              │
│  ┌─────────────────────────────────┐   ┌──────────────────┐  │
│  │ 展开后的真实 Chrome 窗口        │   │ 原生轻量收回胶囊 │  │
│  │ (--app=..., 960x620)            │   │ (NSPanel .float) │  │
│  │                                 │   │                  │  │
│  │                                 │   │  [ ↩ 收回小窗 ]  │  │
│  │                                 │   │  Agent: 网页检索 │  │
│  │                                 │   └──────────────────┘  │
│  │                                 │      (固定在屏幕边缘     │
│  └─────────────────────────────────┘       不随窗口拖动抖动)  │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

1. **轨一：失焦自动收回（Auto-collapse，主路径）**：
   * 引擎/壳持续监控当前系统活跃进程；
   * 用户在真页面内完成人机验证或浏览后，一旦鼠标点击外部应用（如终端、编辑器、飞书）使其失焦，真窗口立即由引擎搬回后台/原位，小窗在原位自动恢复。
2. **轨二：原生微型收回胶囊（Float Capsule，显式点击兜底）**：
   * **形态**：展开时，Viewer 壳弹出一个极小的无焦点置顶面板（`NSPanel`，尺寸约 120×36）；
   * **停靠定位**：停靠在屏幕右上角（或真窗口初始展开位置的侧边外边缘），**静态停靠，不监听 CDP 拖动跟随**；
   * **内容**：半透明毛玻璃背景 + 状态胶囊 + 「↩ 收回小窗」图标；
   * **交互**：点击胶囊，壳通过 UDS 发送 `{"type":"collapse","session_id":"..."}` 给引擎，引擎执行收回逻辑，胶囊退场，小窗回归。

### 4.3 窗口与 Tab 治理（2026-09-24 用户定案；实测证据见案卷）

**用户口径（目标态；同类 ① 已落地为默认模式）**：① 一个浏览器指纹至多一扇受管窗口，同一指纹的 Tab 集中在该窗口内，
只有「一扇窗口 = 一个指纹」时才允许多窗口，且同指纹的窗口固定（**2026-09-24 用户明确：这就是默认行为**）；
② 需要一个全局状态台账，不必每次调 CDP 就能回答「当前有哪些活跃窗口/Tab」，据此能关闭窗口、回收资源、区分活跃与不活跃；
③ 默认窗口的位置与大小必须可预期、用户找得到（旧默认请求屏外坐标，被 macOS 夹成角落一条边）。

**窗口模式（`UWS_WINDOW_MODE`）与实测代价**：

| 模式 | 语义 | 实测代价与边界（Chrome 153，2026-09-24） |
| --- | --- | --- |
| `shared`（**默认**） | 同指纹（同一 attach 端点 + 同一 profile）复用同一扇受管窗口；新会话的 Tab 由该窗口内**自建锚页**（`about:blank`，uws 创建、非用户页面）以中键后台开 Tab 产生；随后用 `Browser.getWindowForTarget` **读回归属**，落到别的窗口立即关闭该 Tab 并 fail-closed | 新建 Tab 会让该窗口被**前置一次**（实测前台 App「飞书」→「Google Chrome」；2026-09-24 引擎路径样本一有一无 —— 会话 3 `Antigravity`→`Google Chrome`、会话 4 未观测到）；CDP 无「移 Tab / 指定窗口开 Tab」能力，这是唯一可行路径 |
| `window` | 每会话一扇专属窗口（`UWS_WINDOW_MODE=window` 显式指定；没有可共享的浏览器端点时也自动退化为它） | 建窗/搬窗**不激活**窗口（实测前台 App 不变）；窗口数随并发增长，且同指纹多窗落在**同一坐标**、后开者遮住先开者（案卷 D3） |
| `tab`（**禁止作默认**） | `Target.createTarget{newWindow:false}` | 落点由 Chrome「最后活动窗口」决定，**实测两次都落进用户自己的窗口**（污染用户标签页），仅作历史兼容保留 |

- R1 澄清（实测）：**输入派发不抢系统焦点**（左键点击后前台 App 不变，仅页面 `hasFocus=true`）；
  **建窗、搬窗也不抢焦点**；唯一会抢一次焦点的是「往某窗口里新建 Tab」。
  这是 `shared` 默认化的**已定取舍**（用户口径 ① 「一个指纹至多一扇受管窗口」优先于「零前置」；「是否必然前置」按实测表述，不写成必发）：
  每起一个会话会让该指纹的窗口被前置一次，`UWS_WINDOW_MODE=window` 可显式换回零前置的多窗行为。
- 锚页纪律：锚页只存在于 uws 自建窗口、只承载「开 Tab」一个动作；**绝不向用户页面注入脚本**（§4.1 原则不变）。
  回收规则：一个共享窗口内**只剩锚页**（会话 Tab 已关、窗内没有任何别的页面 target）才关锚页、随之回收窗口；
  窗内还有别的页面（其他会话或用户自己的）时**一律不动**。

**默认几何（`UWS_WINDOW_POS` 缺省 = `auto`，2026-09-24 落地）**：

1. 取当前会话页的屏幕工作区 `{left, top, width, height}`（`screen.avail*`）；
2. 尺寸 = `min(1440, 宽-2*24) × min(900, 高-2*24)`（`UWS_WINDOW_SIZE` 可覆盖）；
3. 位置 = 工作区**右上角**留 24 DIP 边距（`left = 工作区左 + 工作区宽 - 尺寸宽 - 24`，`top = 工作区上 + 24`）；
4. **读回实际矩形**并写日志（读回值才是生效值）；工作区读不到时不搬窗（保持 Chrome 默认位）并告警；
5. `UWS_WINDOW_POS=offscreen|park|hidden` 仍可显式要求屏外停靠——**已知其会被 macOS 夹取成角落里的一条边**，
   仅作为「宁可看不见也要让开屏幕」的显式选择；`X,Y` / `none|keep` 语义不变。

**全局台账契约（`~/.uws/windows.json`，0600 + 临时文件原子写）**：

- 记录受管窗口：窗口键（attach 端点 + profile/fingerprint）、`window_id`、请求与**实际**几何、
  创建/最后活动时间、锚页 target、以及该窗口的 Tab 列表（会话名、target_id、url、是否锚页、创建/最后活动时间）；
- **活跃/不活跃**由「该 target 是否仍存在」（只读 `/json/list` 或 `Target.getTargets`）判定，
  台账本身是缓存，**不在每次动作里查 CDP**；
- 管理入口 `uws windows list|show|prune|close`：`prune` 默认 dry-run，只清理**已消失**的自建 target 记录；
  **只清单/只关闭 uws 自建窗口与 Tab，用户自己的窗口与 Tab 永不触碰**；
- 与会话续页绑定（`~/.uws/sessions.json`）分工：后者是引擎内核的「会话→target」续页绑定（24h TTL），
  前者是浏览器层的「窗口/Tab」资源台账，两者都不作为对方的替代。

**实现要点（`shared`，v1.5.x 落地）**：进程内注册表 `指纹键（attach 端点 + profile id）→ {window_id, 锚页 target}`；
命中即复用同一扇窗口，未命中才建锚页窗（并按 §4.3 默认几何落位，**只有建窗的那个会话摆位**，后续会话不搬）；
锚页只在 uws 自建窗口内 attach，中键以 `Input.dispatchMouseEvent`（`button=middle`）派发；
新 Tab 的识别判据是「相对点击前的 page target 集合**恰好多出一个**、且其 URL 仍为 `about:blank`」，
多于一个或 URL 不符一律**拒绝认领**（宁可不共享，也不误认别人的 Tab）；归属校验失败时关掉这一个自建 Tab 并 fail-closed；
共享路径的任何故障都**不阻塞会话**：记日志后退化为 `window`（每会话一扇专属小窗）。

**锚页「输入必达」护栏（2026-09-24 真机反证后补，`shared` 默认化的必要条件）**：锚页是**临时 attach 出来的第二个 session**，
会话自身的护栏只装在自己的 target 上（`cdp_session.go::enableTargetDomains`），所以 attach 之后必须给锚页补装同一层 ——
`Page.enable` + `Runtime.enable` + `Emulation.setFocusEmulationEnabled` + `Page.setWebLifecycleState=active`（失败只记日志、不阻塞）。
缺这层时，窗口不在前台会让中键被输入门控挂起到超时（实测：复用窗口成功、`Runtime.evaluate` 也拿到坐标，`Input.dispatchMouseEvent` 却超时 ⇒ 退化成专属小窗）。
派发重试只对 `mouseMoved` 生效（它不产生点击，可安全重放）；`mousePressed` / `mouseReleased` **绝不重试**（多派发一次就可能一次开出两个 Tab），
它们失败时由 **1s 宽限**兜底：先核对是否已有新白板 Tab 出现，有就按成功继续走归属校验，避免把已经发生的点击判成失败、留下无人认领的孤儿 Tab。

**状态**：默认几何（`UWS_WINDOW_POS` 缺省）与窗口模式默认值（`shared`）= `ACTIVE`（真机验收：同指纹两会话 → 一扇窗两个 Tab）
（已落地并真机验收，见 `docs/evidence/pilots/window-tab-governance-2026-09-24.md` 与 `shared-window-2026-09-24.md`）；
`uws windows` 台账 = `DRAFT`（契约已定，落地任务见 `Tasks.md` T-16）。

### 4.4 展开动画与「真窗口到前台」（2026-09-24 用户现场口径；改代码前先写死这几条）

**用户口径（2026-09-24）**：点开小窗时，动画必须是「小窗的**四个角向外飞**到那扇真窗口」；
要**快、流畅**（「嗖的一下」），过程中不做测量、不求精确；点开之后**浏览器必须真的到前台**。

**同日第二轮现场口径（2026-09-24 傍晚，第 5、6 条由此而来）**：① 真窗口到前台的**时刻**要在
**动画末段**——当时看到的是「窗口先到前台、小窗动画后到」，两段脱节（用户原话「影响不大，有洁癖
可以改」）；② 小窗出不出现要看**「真窗口你看不看得见」**（遮挡），不看焦点——真窗口只是**失焦
但仍在屏幕上**时，不该再叠一个小窗。

1. **方向与落点**：动画从**当前小窗矩形**插值到**引擎回执里的真实窗口矩形**（`windowBounds`，壳按
   `ViewerScreenGeometry.appKitFrame(from:primaryScreenHeight:)` 换算成 AppKit 屏幕坐标）——
   即尺寸单调变大、四角向外。**禁止**再自造几何（把落点写成「缩到一半 + 中心对齐」那类，`start/2`）：
   那正是 2026-09-22 实现里让用户看到「一段缩小过程」的原因（`ViewerPanelController.flyAwayAndHide`）。
   回执没带矩形或矩形为空时，落点 = 当前小窗矩形（纯淡出，**不猜**）。
2. **时长与缓动**：`ViewerGeometry.expandDuration = 0.18s` + `.easeOut`（点开链路统一：真窗口飞走与
   回落自绘面板的 `expand(animated:)` 同值）。动画期间**只做** `frame` 与 `alpha` 的插值：不新增 CDP
   往返、不读屏、不测内容。观感优先于精确（用户明确「不需要那么精确」）。
3. **前台抬升是两步，且两步都发生在动画末段**：① 引擎 `Target.activateTarget`（把这扇窗口里的
   目标 Tab/窗口设为活动）；② 壳按 **`browserPID`** 调
   `NSRunningApplication(processIdentifier:).activate(options: [.activateIgnoringOtherApps])`
   （`cmd/viewer/Sources/ViewerCore/ViewerAppActivation.swift`）。
   - **时刻**：`expand` 意图带 `deferRaise: true`，引擎**只把窗口摆好并回矩形、不激活**
     （`PrepareViewerWindow`）；壳飞到动画**末段**（`ViewerGeometry.expandDuration` 的最后一个
     `raiseLeadTime = 0.03s`）才按 pid 激活 + 发 `raise` 意图，引擎此时才 `Target.activateTarget`。
     先抬窗口再播动画 = 「窗口先到前台、小窗动画后到」两段脱节（2026-09-24 第二轮现场口径）。
     老壳不带 `deferRaise` → 引擎保持「收到 expand 就地抬」的老行为（向后兼容）。
   - **排程纪律：末段这一下必须排在非主线程**（2026-09-24 真机读数；壳实现＝
     `ViewerPanelController.raiseQueue`）。展开那一瞬间主线程正在解码/绘制引擎推进来的大帧，
     `DispatchQueue.main.asyncAfter` 实测被推迟到 **0.19 / 0.26 / 0.35 / 0.52 / 0.68s**
     （设计值只有 `raiseDelay` = 0.15s）——窗口于是落在**动画演完之后**，用户看到的仍是
     「小窗先飞完、真窗口后到」两段。两件事的线程归属：**发 `raise` 意图**可跨线程（socket
     写自带锁，立即写出，且引擎那一步才是真正把窗口带到前台的）⇒ 留在抬升队列上；
     **按 pid 激活**必须主线程 ⇒ 异步丢回 main，不拖住前者。
   - **打断即不抬**：动画被收回/换会话打断时**不发** `raise`——用户已经不要了；此时窗口留在
     引擎摆好的位置，用户可直接点它。
   - **订正（2026-09-24 复核）**：上一版在这里写「CDP 那一步在 macOS 上不足」，**已被隔离取证
     证伪**——自建会话 3/3 试次里，引擎那一步本身就把前台切了过去（点击后 **0.017–0.026s**，早于
     壳那一步约 0.07s）。正确表述：引擎那一步在隔离环境**够用**，壳侧按 pid 激活是**真实场景
     兜底**（前台是同 bundle 的**另一个 Chrome 实例**时，只有按 pid 指得到本会话进程）。
     旧壳「点了没到前台」的 0.7s 后小窗回弹，不能只归因于 CDP 激活不足（同刻另有第三方应用
     抢前台的可能）。
   - **边界**：**按 bundle id 激活仍然禁止**（会命中用户自己的 Chrome，见 §7.4），**只允许按 pid
     激活本会话自己的浏览器进程**；附着模式（`browserPID=0`）与无头会话都不动手，只由
     「用户点小窗」这一个手势触发（R1 的唯一例外，§4.1 原则 2 不变）。
4. **判据（只认可观测事实）**：壳日志 `intent result … ok=true` **且**动画末段有
   `activate browser pid=… ok=true`（时间戳比回执晚 ≈`expandDuration - raiseLeadTime` = 0.15s）
   **且**此后 0.4s 轮询**不再**把小窗亮回来；交互手感由用户在真机裁定。
   - **时刻的量化口径**（2026-09-24 真机读数，可复跑）：从 `expand` 回执（动画起步）到
     **前台真的切到 `browserPID`**（`frontwatch` 采样，10Hz）实测 **0.45s**（设计 0.15s +
     引擎激活 20–30ms），偏差来自主线程排队（见第 3 条）；修法是把它移出主线程，修好后应回到
     ≈0.15–0.20s（动画末帧）。同一轮真机读数：`~/.uws/engine.log` 出现
     「观看壳摆好真窗口（未激活，等壳末段通知）」→「观看壳末段激活 target=…」，即分两步成立。

5. **小窗出现的判据 = 「真窗口你看不看得见」，不是「焦点」**（2026-09-24 第二轮现场口径）：
   小窗只在真窗口**看不见**时出现 —— 最小化、被移到别的 Space/屏外、或被别的窗口挡住到
   **可见面积 < 60%**。真窗口在屏幕上（哪怕已经失焦、被点开了别的应用）就**不该**再叠一个小窗：
   那份内容你已经直接看得见了，小窗是重复遮挡。判定是**近似**（macOS 只有窗口列表这一手），
   所以一律**偏向「看得见」**（宁可少显示小窗，也不要在真窗口旁边多贴一块）。
   判据与「前台」的关系：`前台 = 真窗口可见` 的一个子集，旧口径（前台 → 隐藏小窗）是新口径的
   真子集，因此第 5 条**取代**第 4.2/README 里「前台轮询」那一版描述。

6. **判据的实现口径（可单测，壳体不依赖窗口服务器）**：
   - 取 `CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements])`；
   - **目标窗**：**认过窗的会话按那扇窗本身算**（按 `CGWindowID` 认一次就记住——用户的 Chrome 常常
     同时开着好几扇同 pid 的大窗，按「pid 名下面积最大」会量错窗口：2026-09-24 现场那台机器
     `pid=16742` 有两扇 ~887k px² 的窗，「盖住会话那扇、另一扇还露着」会被判成看得见）；
     只拿得到 pid（还没认过窗）时才退回「pid 名下 layer 0 面积最大」；那扇窗不在屏上（含最小化 /
     已关 / 在别的 Space）→ 真窗口看不见 → 显示小窗；
   - **遮挡者** = **同一 layer（0）**且排在目标窗**之前**的窗口（`layer 0` 之外的浮动面板一律不算）；
     实测教训：ChatGPT 的一扇 `layer 3`、1128×2079 的大窗会把**前台**窗口算成「66.7% 被挡」——
     按「层级高于目标」当遮挡者是**错的**，必须限定同层；
   - **自身排除**：自绘面板是 `layer 3`，天然不算遮挡者，但仍按 pid 显式排除（防御）；
   - **比例**：24×24 采样算覆盖比；**可见比例 ≥ 60% → 判「看得见」→ 小窗不出现/立即隐藏**；
   - **附着模式（`browserPID=0`）不能用「没有 pid 就只能看焦点」打发**（2026-09-24 真机复验增补）：
     用户自己的 Chrome（`--attach`）走的**就是**这条路，而第 5 条要修的正是他看到的重复遮挡。
     做法：用引擎给的 `windowBounds`（Chrome 坐标空间，与 `CGWindowList` 同一套左上原点）在屏上
     **按矩形认窗**——命中（各边容差 4px）即记下**那扇窗**与它的 pid（第 7 条：矩形从会话第一帧起
     就有，不等用户点小窗；展开回执带的矩形是同一条通路的第二个来源）。
     实测：引擎报 `72,49 1440×887`，`CGWindowList` 里同一扇窗逐像素一致。
   - **兜底**：拿不到窗口列表（API 空/权限受限）或矩形也认不出进程 → **退回旧的前台判据**
     （前台是浏览器 → 隐藏），仍然拿不准 → 显示小窗。阈值、容差与 `raiseLeadTime` 都是常量，
     改口径只改常量与本节。
7. **附着模式的判据从第一帧起生效：引擎随会话下发真窗口矩形**（2026-09-24 追加，用户现场缺口）：
   第 6 条「按矩形认窗 → 补出 pid」以前只在**用户点过一次小窗**（拿到 expand 回执）之后才拿得到
   矩形——用户真实用法（`--attach`，`browserPID=0`）里正是「盖住真窗口但没点过小窗」⇒ 判据退回
   焦点 ⇒ 判「看得见」⇒ 小窗不出现（2026-09-24 20:38–20:41 现场）。契约：
   - 引擎在**会话建立后随每次帧发布**带上 `windowBounds`（`omitempty`），来源 = `Browser.getWindowForTarget`
     + `Browser.getWindowBounds`（**只读**：不搬窗口、不归位最小化状态——那是用户点了之后 `Prepare` 的事）；
   - 读取**节流**（同一会话最多每 3s 一次），用户移动/缩放窗口后矩形跟得上；读不到（无头会话、
     无窗口、CDP 失败）→ 字段留空 → 壳退回焦点判据（第 6 条兜底不变）；
   - 壳把帧里的 `windowBounds` 当作与展开回执同等的输入（同一条 `setHostWindowBounds` 通路），
     于是第一次 `pollPresence` 就能认窗 → 认 pid → 第 5 条判据生效，**不需要任何点击**；
   - 引擎**自建会话**（非附着）同时在帧里带 `browserPID`（会话知道自己的 Chrome 进程），壳从第一帧
     就有 pid；两类会话的第 5 条判据都从零时刻生效；
   - 老壳忽略新字段（`omitempty`），行为不变。
8. **「静默」只降频，不撤面板**（2026-09-24 追加，同一现场缺口的第二半）：
   以前 Agent 静默 5 分钟（`UWS_VIEWER_IDLE_CLOSE_SEC`，缺省 300s）→ 引擎发 `sessionClosed` → 壳丢掉
   面板；用户时段里根本无面板可显示（20:33:53 现场）。新口径：**面板存续由第 5/6 条的可见性判据
   决定**，Agent 静默只把推流降到空闲频率（既有自适应 pump）。
   - 缺省（`UWS_VIEWER_IDLE_CLOSE_SEC` 未设）= **不撤面板**；显式设 >0 秒 = 保留旧「静默这么久就
     收面板」的回收语义（想省电的长会话用它）；
   - 代价（诚实写在这里）：长会话期间 screencast 常驻——空闲 1Hz 帧 + 每 3s 一次窗口矩形读取；
     换来的是「真窗口看不见 ⇒ 小窗一定在」不再被时间掐掉；
   - **用户的 ✕（`GoQuiet`）与「页面没了」（`ViewerAlive`=false → `Close`）不受影响**：前者是用户
     显式不要，后者是会话真的结束，两路都照旧丢面板。

9. **小窗的「槽位」＝用户放下的位置**（2026-09-24 追加，用户现场缺口）：
   小窗被藏起来（展开飞走 / 让位给看得见的真窗口）之后要回哪儿，由它自己的**槽位**决定；
   槽位的唯一写者是用户，规则如下：
   - **拖动结束即记账**：吸附（`ViewerGeometry.snapped` 的夹取 + 贴边）只决定「要不要顺手把落点
     挪到可见区/边距上」，**与「槽位记不记」无关**。2026-09-24 用户现场：记账当时挂在「吸附确实
     改了坐标」这个分支上，于是把面板拖到屏幕中间（无需吸附）再点开，飞走重现身时小窗回到了
     **移动前**的位置。
   - **跨一切壳体内部动作不变**：展开飞走、让位隐藏、轮询把它亮回来、收回、换绑会话，槽位都
     不动；只有用户再拖一次、或窗口被 macOS 夹取/缩放（`windowDidResize`）才更新。
   - **不落盘**：跨壳进程重启的记忆不在本轮口径内——重启后回到创建时的默认槽位
     （`ViewerGeometry.compactFrame(inVisibleFrame:)`）。

---

## 5. 实施路线与落地阶段

| 阶段 | 实施内容 | 交付产物 / 验收标准 |
|---|---|---|
| **Phase 1<br>(代码已落地 2026-09-12，真机取证待复跑)** | **展开态收回胶囊 + 持久身份 profile** | 1. ✅ 观看壳原生收回胶囊（`cmd/viewer/Sources/ViewerCore/ViewerCapsule.swift`，点击经 UDS 发 `collapse`，见 §6.1）；<br>2. ⏳ S5 真机全链路复测取证（主副屏展开、操作、胶囊收回、失焦收回）；<br>3. ✅ `pkg/browser` 可选 `Config.ProfileID` / `--profile-id`，落 `~/.uws/profiles/<id>/`（见 §6.2）。 |
| **Phase 2<br>(代码已落地 2026-09-12，外网站验收待做)** | **基础指纹成套化 + 自动化特征清除 + 身份注册表** | 1. ✅ `FingerprintProfile` 解析/校验/派生（`pkg/fingerprint`，未知字段拒绝 + 矛盾检测）；<br>2. ✅ 启动参数 `--disable-blink-features=AutomationControlled` / `--lang` / `--proxy-server`；<br>3. ✅ `Emulation.setUserAgentOverride`（带 `userAgentMetadata` + `acceptLanguage`）+ `setLocaleOverride` + 时区 + 屏幕（含 `screen.*`）；<br>4. ✅ 外部站点验收（CreepJS / bot.sannysoft / BrowserLeaks）——**无头已跑通**（2026-09-13，见 §7.5：sannysoft `HEADCHR_UA` FAIL→ok、UA-CH/时区/语言/屏幕/字体全对、0 项硬失败）；⏳ **有头真机复跑待做**（无头没有 WebGL/窗口几何不自洽，见 §7.5 结论）；本机真 Chrome 端到端已过（§7）；<br>5. ✅ 身份注册表：账号 ↔ 身份 ↔ 代理（§2.3）。 |
| **Phase 3<br>(代码已落地 2026-09-13)** | **JS 隐蔽 Hook + 渲染噪声注入** | 1. ✅ 原型链 Hook 与 native `toString()` 保护；<br>2. ✅ 基于 PRNG seed 的 Canvas 2D / AudioContext / WebGL 确定性微扰；<br>3. ✅ CDP `Network.getAllCookies` 与指定 origin 的 LocalStorage、当前 tab SessionStorage 导出/导航前恢复为 `StateBundle`。 |
| **Phase 4<br>(代码已落地，双节点实机验收待做)** | **租约服务 + 跨端会话恢复** | 1. ✅ Token 租约、心跳守卫与认证 HTTP 服务/客户端（`cmd/uws-lease`、`LeaseBackend`；可直接 `--tls-cert/--tls-key`，非回环明文拒绝）；<br>2. ✅ 导出/加密/同档案未导航恢复的 CLI 闭环；⏳ 两台真实节点经共同 TLS 服务的登录态验收（目标站点是否接受会话必须如实记录）。 |

**补充能力（2026-09-13）**：`uws request` 可在不启动浏览器时，以同一指纹档案与可选同身份 State Bundle 发纯 HTTP 请求；它是接口资源请求的伴随能力，不替代浏览器的 TLS/HTTP2/JS/WebRTC 指纹（§3.5）。

四项仍待真实环境取证的统一操作、通过判据与脱敏证据格式见 [真实验收运行手册](evidence/archive/fingerprint-viewer-acceptance-runbook-2026-09-13.md)；不得用无头审计、本机 TLS fixture 或 smoke 替代。

---

## 6. Phase 1 落地记录（2026-09-12）

代码已落地并自测通过；真机取证待用户复跑后补入 `docs/evidence/`。

### 6.1 展开态收回胶囊（壳侧，纯 AppKit）

- `cmd/viewer/Sources/ViewerCore/ViewerCapsule.swift`（新）：
  - `ViewerCapsulePlacement`（纯几何、可无窗口服务器单测）：优先停靠在真窗口**外侧**
    （左 → 右 → 下），没地方才退回屏幕右上角；正常窗口布局下不压住真页面，也不越出可见区。
    若真窗口铺满整个工作区，屏幕上不存在同时满足两者的几何位置，右上角兜底可能与页面重叠，
    这是静态停靠且不读取他进程窗口包围盒的诚实边界。
  - `ViewerCapsuleView`：圆角毛玻璃 + 一行「↩ 收回小窗 · <Agent 名>」；自己处理
    `mouseDown`/`mouseUp`，并显式接收 `acceptsFirstMouse`——面板永远不是 key window，
    `NSButton` 在这种窗口里点不中，胶囊必须保证非激活面板的第一次点击也能触发。
  - `ViewerCapsulePanel`：`.borderless + .nonactivatingPanel`、`canBecomeKey/Main == false`
    ⇒ 点它不抢用户正在打字的焦点（R1 在第三种形态上继续成立）。
- 接线在 `ViewerPanelController`：`enterLivePage` 出现、`leaveLivePage`/`hide` 退场；
  点击 → `ViewerState.requestCollapse()` → 宿主发 `collapse` 意图（搬窗口永远是引擎的事）。
  壳只有在写入成功且收到 `intentAck.ok=true` 后才退场；写失败或超时会保留真窗口与胶囊，避免
  壳态和引擎窗口状态分叉。
  焦点判定的「壳内」矩形集合随之加入胶囊面板，避免点胶囊被误判成「用户切走了」。
- **刻意不做跟随拖动**（§4.1 第 3 条）：跟随要读别的进程的窗口包围盒 → 触发屏幕录制授权。

### 6.2 持久身份 profile（引擎侧）

- `pkg/browser/profile_dir.go`（新）：
  - `Config.ProfileID` 非空 → user-data-dir = `~/.uws/profiles/<id>/`（`UWS_PROFILE_ROOT` 可改根），
    新建及复用时都强制目录 0700、**退出不删**；为空 → 行为与以前完全一致（临时目录、用完即焚）。
  - id 校验 `[A-Za-z0-9._-]{1,64}`，拒绝 `.` / `..` / 前导点：该值将来会从配置或网络下发，
    路径拼接前必须先挡住逃逸。
  - 单写者不另造锁，直接用 Chrome 的 `SingletonLock`：**活着的 pid** 持锁 → 启动前明确报错
    （而不是让调用方干等 15 秒启动超时）；**陈旧锁**（pid 早没了）→ 就地清掉再继续。
  - 退出路径：持久身份先发 `Browser.close` 请 Chrome 自己退（最多等 2s）再补刀——SIGKILL 会让
    最后一段 Cookie/LocalStorage 不落盘，而落盘正是持久身份的全部意义。
  - `seedChromePreferences` 改为最小合并：只替换顶层 `translate` 键，其余顶层值以 `json.RawMessage`
    原样搬运（持久身份里的 Preferences 是用户资产，绝不整体覆盖）。
- app 窗口启动只接管与本次 `InitialURL`/空白起始页明确对应的 target；多个不明页面或超时不再
  降级为普通带浏览器 UI 的窗口，直接失败并交给上层按失败语义处理。
- `cmd/uws-viewer-demo --profile-id <id>` 已接线。

### 6.3 验证（2026-09-12）

| 项 | 命令 | 结果 |
|---|---|---|
| 壳单测 | `swift test`（`cmd/viewer`） | **46 例 0 失败**（原 38 + 胶囊落点 7 + 非激活首击 1） |
| 壳产物 | `swift build -c release --package-path cmd/viewer` | `uws-viewer-prototype` 902,800 B |
| 引擎定向 | `go test ./pkg/browser/ -run 'Viewer\|Window\|Screencast'` | ok（7.97s） |
| 反向意图回归 | `go test ./pkg/viewerhub/ ./pkg/engine/` | ok |
| 静态检查 | `go build ./...`、`gofmt -l`、`go vet` | 干净 |
| 驱动 | `go build -o uws-viewer-demo ./cmd/uws-viewer-demo` | 已重建 |
| 审核修正 | 胶囊首击、收回 ack/超时、profile 既有目录权限、app target fail-closed | 已补实现；构建与定向回归通过 |

### 6.4 Phase 1 收口时未做（历史基线；当前状态见 §5 / §8）

- 真机取证（主副屏展开 / 胶囊收回 / 失焦收回）待用户复跑后采集；胶囊的「非 key 窗口一次点中」
  目前只有代码、纯几何单测与状态回归支撑，**尚无真人点击证据**。
- 当时 Phase 2+ 尚未开始；后续已完成档案/Hook/State Bundle/租约服务代码接线，当前可验证范围以 §5
  与 §8 为准。真实双屏、真实双节点及目标站点是否接受登录态仍不是代码自测可替代的证据。

---

## 7. Phase 2 落地记录（2026-09-12）

### 7.1 落地内容

| 能力 | 位置 | 说明 |
|---|---|---|
| 档案解析/校验/派生 | `pkg/fingerprint/profile.go` | 严格解码（未知字段与尾随 JSON 即错）、矛盾检测（UA↔Client Hints 平台、UA↔mobile、时区合法性、代理 scheme、身份名）、`ChromeArgs()` / `Overrides()` / `MatchesChromeVersion()` / `MissingNoiseFeatures()`；展示代理时统一脱敏 |
| 身份注册表 | `pkg/fingerprint/registry.go` | 目录布局、档案原子落盘（0600）、账号绑定 1:1 不变式、`Conflicts()` 检查共享代理、悬空绑定与被篡改出的重复身份 |
| CLI | `cmd/uws/identity.go` | `list / show / bind / unbind / resolve / use / check` |
| 启动参数落地 | `pkg/browser/cdp_session.go`（Create） | 档案 → `--lang` / `--disable-blink-features=AutomationControlled` / `--proxy-server`；附着模式明确记一条「起不了新进程，启动参数用不上」 |
| CDP 覆盖落地 | `pkg/browser/cdp_session.go`（initTarget） | UA + `acceptLanguage` + `userAgentMetadata`（消除「UA 说 macOS、Client Hints 说 Windows」的矛盾）、`Emulation.setLocaleOverride`、时区、`setDeviceMetricsOverride`（含 `screenWidth/screenHeight`） |
| 优先级 | 同上 | **档案 > cfg 零散设置**（档案是身份事实源；引擎会给 Timezone 注入默认 `UTC`，若让零散设置优先，档案一进门就作废）；**显式任务级 Viewport > 档案 > 引擎默认** |
| 观看壳配套 | `pkg/browser/viewer_window.go` | 展开前**先**摘设备指标覆盖再算展开矩形；附着 + 指纹屏幕同样纳入摘除，收回按优先级恢复显式 viewport 或档案屏幕 |

### 7.2 验证

| 项 | 命令 | 结果 |
|---|---|---|
| 档案纯逻辑单测 | `go test ./pkg/fingerprint/ -count=1` | ok（**20 例 + 8 个子例**：含尾随 JSON、UA↔mobile、代理脱敏、重复身份绑定等复核项） |
| **真 Chrome 端到端** | `go test ./pkg/browser/ -run Fingerprint -count=1 -v` | **PASS**：启动参数含 `--lang=zh-CN`、`--disable-blink-features=AutomationControlled`；页面自报 `userAgent` / `navigator.webdriver=false` / `userAgentData.platform=macOS` / `brands` 含 `Chromium:152` / 时区 `Asia/Shanghai` / `navigator.language=zh-CN` / `screen 1440×900` / `devicePixelRatio 2` / `innerWidth/Height 1440×900` 全部与档案一致 |
| 回归 | `go test ./pkg/browser/ -run 'Viewer\|Window\|Profile\|Fingerprint'`、`./pkg/viewerhub/ ./pkg/engine/` | 全绿 |
| CLI 实跑 | `UWS_IDENTITY_ROOT=/tmp/... uws identity list/bind/resolve/check` | 绑定 1:1 与冲突判定均按预期拒绝/报出（含共用代理出口告警） |
| 静态 | `gofmt -l` / `go build ./...` / `go vet` | 干净 |

### 7.3 当前边界（Phase 2 历史未做项已按当前状态修订）

- **外部站点验收**：无头审计已留证（§7.5）；有头真机复跑仍待。无头证据不能替代有头 GPU、窗口几何与
  真实交互面。
- **噪声与硬件 Hook**：已在 Phase 3 落地并有真 Chrome 回归（§8）；档案之外的环境物理特征仍不可由 JS 保证一致。
- **`color_depth` 覆盖不了**：CDP 没有对应接口，档案里该字段目前只作记账。
- **Launch args / 代理只对启动模式有效**：附着模式（接管用户已有 Chrome）起不了新进程，只应用 CDP 覆盖。
- **账号绑定不校验代理是否真的通**：注册表管绑定关系与档案自洽，实际出口 IP 要真机核对。
- **注册表写锁的范围**：`Save` / `Bind` / `Unbind` 用同一 `.registry.lock` 的 `flock` 串行化，进程异常退出由 OS 释放锁，避免两条 CLI 同时读旧绑定后互相覆盖。它只保护同一目录所在的本机文件系统；共享/分布式文件系统与跨节点一致性仍必须使用 Profile Lease，不能混为一谈。
- **同一代理告警**：`Conflicts()` 已按归一化出口键比较；它仍只能发现配置相同，不能证明实际出口 IP 相同；脱敏只影响展示，不影响 Chrome 启动参数。
- **档案/profile 名不一致已 fail-fast**：`pkg/browser.CDPFactory.Create` 与 `uws-viewer-demo` 都在启动/附着前拒绝不一致组合。
- **WebRTC 不在档案的控制范围内**（2026-09-13 外站验收实测）：`--proxy-server` 只接管 TCP（HTTP/SOCKS5），
  WebRTC 的 UDP 不进去，于是「HTTP 出口 = 代理、WebRTC 出口 = 别处」这种不一致本身会被标出来
  （BrowserLeaks 直接报 `WebRTC IP doesn't match your Remote IP`）。绑代理 ≠ 全链路隔离，见 §7.5。
- **无头形态扛不住外站检查**：无头 + `--disable-gpu` 下 WebGL 整块不存在（`--enable-unsafe-swiftshader`
  能救回来，但 SwiftShader 本身一眼假），且窗口几何（`outerWidth/Height`）与档案屏幕不自洽。
  隐蔽性结论必须在**有头**（产品的真实形态）下取，见 §7.5。
- 引擎/gRPC 已在 v1.5.7 接入 `profile_id`、完整档案与 `ImportState`；恢复必须在同 profile 的干净会话、首次导航前完成，已拒绝迟到导入（§3.4 / §8）。

### 7.4 评审吸收（2026-09-13，两份外部审查对照现码核验）

外部给的是「审查报告 + 落地方案」，逐条对照现码核验后**只吸收成立的部分**：

| 审查结论 | 裁决 | 依据 / 落点 |
|---|---|---|
| 画面宽高比 ≠ 面板页面区时虚拟光标错位（审查称 `contentsGravity = .resizeAspectFill` 会裁边） | **吸收**（前提要改） | 现码早已是 `.resizeAspect`（`ViewerCanvasView.swift:63`），真问题是叠加层按整块 `imageLayer.bounds` 换算：改成按**画面真正落到的矩形**映射（`ViewerGeometry.aspectFitRect` + `ViewerCursorMath.layerPoint(normalized:in rect:)`）。默认尺寸偏 ~4pt，`390×844` 档案视口下偏几十 pt |
| `.retrying` 只 emit、没人消费，断线没有任何提示 | **吸收** | `ViewerSocketClient.State` 广播到所有壳（`ViewerHostController.setConnectionState`），胶囊/展开条显示「引擎未连接 · Ns 后重试」（`ViewerConnectionLabel`）；重连退避本身早就有（1.0s ×1.6、上限 5s） |
| 代理 URL 内嵌用户名/密码（Chrome `--proxy-server` 不支持） | **吸收，且改成 fail-closed** | `Validate()` 直接拒绝：放行等于「以为换了出口、其实没换」。见 §2.1；`socks5h` 归一化同批落地 |
| 附着模式 + 档案绑代理 = 走本机真实出口 | **吸收（把日志点名）** | 原来只有一句「启动参数用不上」；现在点名档案与代理地址，并明说本会话走本机真实出口 |
| 「壳内再造一个放大截图窗 + 切真窗口按钮」 | **拒绝** | 与 §4 冲突：展开 = 真页面本身；再造一个「看截图」的主形态会制造两份状态源 |
| app 级 `NSRunningApplication.activate(bundleIdentifier: "com.google.Chrome")` | **拒绝**（**2026-09-24 修订**：改判**按 `browserPID`** 的激活为展开手势的第二步，见 §4.4；按 bundle id 的激活仍**拒绝**） | bundle id 会命中**用户自己的** Chrome（我们的实例是 UWS 自己起的）；按 pid 激活不会——实测发现只靠引擎侧 `Target.activateTarget` 不足以把 macOS 上的浏览器 App 抬到最前（§4.4 第 3 条的真机反证） |
| 多会话 Pin / Active-Follow 轮播 | **拒绝（记录理由）** | 每会话独立面板 + 级联槽位（`ViewerGeometry.compactFrame(index:)`），不存在「单窗被别的会话抢画面」；真有轮播需求再议 |
| 空闲时把 `Page.startScreencast` 调到 `everyNthFrame: 5` | **记入 Phase 3 候选** | pump 已有 100ms→1s 空转退避，但 screencast 本体仍每秒 1 帧；stop/restart 的代价与 R7 的 150ms 预算要真机实测后再定 |
| 「先外站验收，再进 Phase 3」 | **采纳为顺序** | 与 §7.3 同一件事：先拿 CreepJS / bot.sannysoft 的实际扣分点，Phase 3 的原型链 Hook 才有靶子 |
| 「账号 ↔ 身份 1:1 硬约束不应放宽」 | **与现案一致，无需改动** | 见 §2.3 三条不变式 |

本轮验证：`swift test --package-path cmd/viewer` **56 例 0 失败**（新增几何 4 + 光标 2 + 连接状态 4）；
`go test ./pkg/fingerprint/ ./pkg/viewerhub/` 全绿；`pkg/browser` 定向用例通过（会触碰真实 Chrome 的用例由
`UWS_RUN_CHROME_E2E` 门禁跳过）；`gofmt -l` / `go build ./...` 干净；`bin/webtouch`、`bin/uws-viewer-demo` 与
release 壳（`cmd/viewer/.build/release/uws-viewer-prototype`）已重建。
未做：真机截图/交互取证（等用户复跑）。外网站验收已推进，见 §7.5（无头已跑通、有头待复跑）。

### 7.5 外站验收记录（2026-09-13，**无头**；有头待复跑）

§7 证明的是「档案 → 浏览器自报身份」（自己对自己）；这一节是**别人怎么看我们**：把同一份档案真的开到
外部检测站点上。工具是新增的 `pkg/browser/fingerprint_audit_test.go`（判据与证据都在里面，随代码走）。

```sh
cd agent-tools/webtouch
scripts/fingerprint_audit.sh                       # 默认无头全量（不碰桌面）
scripts/fingerprint_audit.sh --headed              # 有头（会真的开一扇 Chrome 窗口）
scripts/fingerprint_audit.sh --sites egress,sannysoft --chrome-args --enable-unsafe-swiftshader
# 等价的手写形式（脚本只是把环境变量固定下来）：
UWS_RUN_EXTERNAL_AUDIT=1 go test ./pkg/browser/ -run TestExternalFingerprintAudit -v -count=1
```

会话设计：先跑**裸奔基线**（不带档案），再由基线镜像派生一份「本机自洽」档案（同一只 Chrome 的版本与
品牌串、同一套语言/时区，UA 去掉 `HeadlessChrome`，只把屏幕换成指定值），两份会话都开到
`api.ipify.org` → `bot.sannysoft.com` → `creepjs` → `browserleaks.com/webrtc`；
证据（append-only，截图按 `docs/evidence/README.md` §3 不进 git）落在
`docs/evidence/fingerprint-audit/2026-09-13-headless/`（`audit.json` 机读 + `audit.md` 人读 + 逐站截图）。

实测（本机 Chrome `152.0.7977.83` / macOS 13.5）：

| 观测面 | 裸奔基线 | 带档案 | 判定 |
|---|---|---|---|
| sannysoft `HEADCHR_UA` | **FAIL**（UA 写着 `HeadlessChrome`） | **ok** | ✅ 档案消掉了最刺眼的无头标记 |
| sannysoft `WebDriver` / `WebDriver Advanced` / `Chrome` / `Plugins Length` / `Languages` | passed / present / 5 / `zh-CN,zh` | 同左 | ✅ |
| sannysoft `WebGL Vendor/Renderer` | `Canvas has no webgl context` | 同左 | ⚠️ 无头 artifact（见缺口 2） |
| CreepJS `userAgentData` | — | `Google Chrome 152 (152.0.7977.83)` / `macOS 13.5.0 x86_64` | ✅ UA-CH 全对 |
| CreepJS 时区 / 语言 / 屏幕 | — | `Asia/Shanghai (-480)` / `zh-CN, zh` / `1440×900, avail 1440×900, depth 24` | ✅ |
| CreepJS 字体 | — | `load (18/51)`，命中一串真 macOS 字体 | ✅ 物理层如实（不伪装） |
| CreepJS Canvas / Audio / DOMRect / SVG / Speech 哈希 | 与档案一致 | 与基线一致 | 记录：噪声是 Phase 3；**跨会话稳定**正是身份可复现的前提 |
| `navigator.webdriver` / 原生 getter (`ua`/`platform`/`hardwareConcurrency`/`webdriver`) | `false` / 全 `[native code]` | 同左 | ✅ 无 JS 层植入痕迹（Phase 3 的 Hook 必须守住这条线） |
| HTTP 出口 IP | 与档案会话一致 | 一致 | 只记账：本机出口在多次运行间会漂移（疑似轮换代理），故不设硬判据 |
| BrowserLeaks WebRTC | — | `No Local IP Leak` ✔，但 **`WebRTC IP doesn't match your Remote IP`**（srflx `140.235.9.192` vs HTTP 出口 `222.95.184.20`） | ⚠️ **真缺口**，见缺口 1 |

硬判据（档案承诺必须兑现的那批）**0 项失败**：`navigator.userAgent`、`navigator.webdriver=false`、
UA-CH 的 `platform`/`brands`/`fullVersion`/`platformVersion`/`architecture`、`Intl` 时区、
`navigator.language(s)`、`screen.width/height`、`devicePixelRatio`、`innerWidth/Height`、
子 iframe 的身份继承、`window.cdc_*` / `document.$cdc_*` 自动化残留为空。

三条要带进 Phase 3 的缺口（这轮才看清）：

1. **WebRTC 不受档案控制，且「加个开关」这条路已被实测否掉**：`--proxy-server` 只管 TCP，
   WebRTC 的 UDP 直接出网卡；实测那一次 HTTP 出口是代理（`222.95.184.20`）而 WebRTC srflx 是本机
   真实出口（`177.5.52.214`，跨运行稳定），BrowserLeaks 当场报 `WebRTC IP doesn't match your Remote IP`。
   三次对照实验（Chrome 152.0.7977.83 / 无头）：
   | 实验 | 启动参数里的 WebRTC 策略 | BrowserLeaks 结果 |
   |---|---|---|
   | A 现状 | （无） | `WebRTC IP doesn't match your Remote IP`，srflx = 本机真 IP |
   | B | `--force-webrtc-ip-handling-policy=disable_non_proxied_udp` | **与 A 完全一致**（srflx 照样是本机真 IP） |
   | C | `--force-webrtc-ip-handling-policy=default_public_interface_only` | **仍与 A 一致**（本该消失的 `.local` host 候选还在） |
   三条合起来只能得出一个结论：**这个命令行开关在这只 Chrome 上根本没被读**（三份证据留档在
   `docs/evidence/fingerprint-audit/2026-09-13-webrtc-{A-off,B-disablenonproxied,C-publiconly}/`）。
   因此档案层只保留 `network.webrtc_policy` 显式字段（原样下发给 Chrome，不改默认），
   绑代理时由 `WebRTCLeakWarning()` / `identity use` / 引擎日志把实情说出来——不做「加了个看起来
   在防护、其实不生效的参数」这种虚假安全感（与本文件拒绝代理凭据同一条原则）。
   真要堵：扩展的 `chrome.privacy.network.webRTCIPHandlingPolicy`，或系统级 TUN 把 UDP 也接进代理。
2. **无头 + `--disable-gpu` 没有 WebGL**：实测 `--enable-unsafe-swiftshader` 能救回来
   （`ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero)), SwiftShader driver)`，35 个扩展），
   但 SwiftShader 本身就是「软件 GPU」这一眼假。结论：**无头不是能扛外站检查的形态**，
   隐蔽性结论必须在有头下取（有头走真实 GPU，也是产品真实形态）。
3. **无头窗口几何与档案屏幕不自洽**：无头窗口 756×556，而档案把 `innerWidth` 顶到 1440 →
   `outer < inner` 这种真实浏览器不可能出现的组合（sannysoft `PHANTOM_WINDOW_HEIGHT` 与 CreepJS `Screen`
   都看得到）。有头下窗口由引擎自己算，不存在该组合；若将来真要做无头形态，必须一起覆盖窗口几何。

**未做**：有头真机复跑（`UWS_AUDIT_HEADED=1`，会开窗，需用户在场的时机）、代理出口的真实验收
（档案绑 SOCKS5 后「出口 IP ≠ 基线」这条硬判据已写进用例，但还没有可用的真代理来跑）。

---

## 8. Phase 3 落地记录（2026-09-13）

在 Phase 2 的基础上，正式闭环落地 **运行时原型链隐蔽 Hook、硬件参数伪装与确定性渲染噪声注入**。

### 8.1 落地内容

1. **原型链隐蔽基础设施（`pkg/fingerprint/hook.go`）**：
   - 严格兑现 §2.2 规范：`makeNative` 通过重写自身 `Function.prototype.toString`，使得直接调用 `fn.toString()` 以及反爬脚本常用的 `Function.prototype.toString.call(fn)` 均一致返回 `function <name>() { [native code] }`；
   - 属性伪装通过 `overrideGetter(Navigator.prototype, prop, getter)` 将 Getter 挂在原型对象上，`navigator.hasOwnProperty('hardwareConcurrency')` 返回 `false`，杜绝实例属性枚举破功；
   - 纯闭包注入，不向全局 `window` 暴露任何可见标志（如 `__uws_hook_installed__` 等），零全局环境污染。
2. **硬件参数伪装**：
   - 档案中 `emulation.hardware.hardware_concurrency` 与 `device_memory` 由原生 Getter 接管；
   - 同步覆盖 `WorkerNavigator.prototype`，保障 Web Worker 内并发度感知自洽。
3. **确定性渲染噪声注入（Canvas 2D / AudioContext / WebGLBuffer）**：
   - **确定性与幂等设计**：基于档案的 `seed: uint32`。
     - **Canvas 2D**：Hook `CanvasRenderingContext2D.prototype.getImageData`，对像素数据进行步长微扰；同时 Hook `HTMLCanvasElement.prototype.toDataURL` 与 `toBlob`，对绘制区域有效像素的 RGB 最低有效位应用 `(val & ~1) | seedBit` 掩码微调（肉眼无感，差值 <0.4%），确保在 PNG 压缩和 CRC 层面引发确定性雪崩效应，多次调用绝对幂等稳定；
     - **AudioContext**：Hook `OfflineAudioContext.prototype.startRendering`（及 webkit 前缀），在解决的 `AudioBuffer` 采样通道叠加基于 seed 的 `1e-7` 级别确定性位移，同 seed 采样和 100% 相同，不同 seed 产生确定性分歧；
     - **WebGLBuffer**：Hook `WebGLRenderingContext.prototype.bufferData`，对上传的顶点 Float32Array 引入确定性微小扰动。
4. **CDP 会话接线（`pkg/browser/cdp_session.go`）**：
   - 在 `initTarget` 中，当 `fp.NeedsHookScript()` 时，通过 `Page.addScriptToEvaluateOnNewDocument` 在所有新文档解析最前沿注入；
   - 初始化时对当前 target 立即进行 `Runtime.evaluate` 评估，确保零刷新即时生效；
   - 自动穿透主文档与所有同源/跨源子 iframe。

### 8.2 验证结果

1. **专项端到端真 Chrome 测试（`pkg/browser/fingerprint_phase3_test.go`，全 PASS）**：
   - `TestFingerprintPhase3HardwareAndNativeProtection`：
     - `hardwareConcurrency = 12`, `deviceMemory = 16`；
     - `navigator.hasOwnProperty(...) === false`；
     - `get.toString()` 与 `Function.prototype.toString.call(get)` 均返回 `[native code]`；
     - `Function.prototype.toString.toString()` 为 `[native code]`。
   - `TestFingerprintPhase3CanvasAndAudioDeterminism`：
     - 同 seed (1001) 运行 2 次：`toDataURL`、`getImageData` 哈希与 `AudioContext` 采样和 **100% 完全相同**；
     - 不同 seed (1001 vs 2002)：Canvas `toDataURL`、`getImageData` 哈希与 `AudioContext` 采样和 **全部产生确定性分歧**。
   - `TestFingerprintPhase3SubIframeInheritance`：
     - 动态挂载子 iframe，`iframe.contentWindow.navigator` 硬件参数与 `[native code]` 均无缝继承生效。
2. **全量回归与外站审计对账**：
   - `pkg/fingerprint`（23 例）、`pkg/browser` 定向（Viewer/Window/Profile/Fingerprint）、`pkg/engine`、`pkg/viewerhub` 回归全部 PASS；
   - `./scripts/fingerprint_audit.sh`（真机无头验收）硬失败保持 **0** 项，对账表正式记录：
     - `canvas2d 哈希：基线 vs 档案（Phase 3 噪声注入：跨档分歧）`: `fac93a9d vs babbf15` ✅
     - `audio 哈希：基线 vs 档案（Phase 3 噪声注入：跨档分歧）`: `172.67164876 vs 172.67174017` ✅
     - 原生 getter @hardwareConcurrency / @ua / @platform / @webdriver 全为 `[native code]` ✅

### 8.3 审核修订（2026-09-13）

* 有头 app 窗口改为先开 `about:blank`，完成 CDP 覆盖与新文档脚本注册后才导航 `InitialURL`，消除首帧暴露真实环境的竞态。
* `Target.setAutoAttach` 在主页面 session 上暂停 Worker，注入后恢复；真 Chrome 回归覆盖 Dedicated Worker 的 `hardwareConcurrency/deviceMemory`。
* WebGL `UNMASKED_VENDOR_WEBGL` / `UNMASKED_RENDERER_WEBGL` 与 `emulation.hardware.webgl` 同源；Canvas 导出在副本上加噪，不再改写业务 canvas；混合函数不再截断 seed 熵，加入相隔 8,192,000 的碰撞回归。
* `StateBundle` 已实现严格 JSON、AES-256-GCM 信封、CDP Cookie + 导航前 LocalStorage / 当前 tab SessionStorage 恢复、`uws state export|inspect|import|restore` 用户入口与两个临时 Chrome 的 localhost 往返验证；导入强制同一 `profile_id` 且首次导航后 fail-closed。`LeaseStore` 已抽象为本地或认证 HTTP 后端并接入引擎生命周期；真实多机 TLS 服务与目标站点登录态仍待取证。

### 8.4 Viewer 本机真实链路 smoke（2026-09-13）

已用现有 release 壳和 `bin/uws-viewer-demo --url about:blank --seconds 12` 做一次无外站、无账号的真实进程验收：原生 Viewer 成功附着 Unix socket，启动模式 Chrome app 窗口被 CDP 接管，推流在正常关闭前持续运行。该验收只证明本机壳/Chrome/socket 三段链路；双屏展开、用户点击胶囊与失焦收回仍需在真实双屏桌面上人工取证，不以 smoke 代替。
