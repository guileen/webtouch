# Lightpanda 指纹调研 demo（研究用，不接主线）

> 一次性调研工具，服务案卷 [`docs/evidence/research/lightpanda-fingerprint-2026-10-03.md`](../../../docs/evidence/research/lightpanda-fingerprint-2026-10-03.md)。
> 纪律：只读探测；**不碰** WebTouch 主线代码、共享 daemon 与用户 9222 窗口；对照组 Chrome 用独立 profile + 独立端口。

## 一条命令

```bash
scripts/research/lightpanda-fingerprint/run.sh
```

做的事：下载/复用 Lightpanda 二进制 → 起本地靶站（记录连接层 HTTP 头）→ 起 4 个端点
（Lightpanda 默认 / `--locale zh-CN` / `--load-resources` 全开 / Chrome headless 对照）
→ 同一份 JS 探针逐个打靶 → 生成逐项对照表 → 渲染保真度小检。
产物默认落 `docs/evidence/research/lightpanda-fingerprint-<日期>/`。

## 组件

| 文件 | 作用 |
| --- | --- |
| `probe-payload.js` | 页面内指纹探针（身份 / UA-CH / 屏幕 / 语言时区 / Canvas / WebGL / Audio / 字体 / 布局 / 媒体 / WebRTC / 反检测 / API 面覆盖率）。可直接粘进 DevTools 单跑。 |
| `cdp-probe.mjs` | 通用 CDP 客户端：把上面的探针打到任意 CDP 端点，输出 JSON 快照。零第三方依赖（Node 18+ 内置 `fetch`/`WebSocket`）。 |
| `header-server.mjs` | 本地靶站：既当被测页面，又记录每个请求的**原始头顺序**（页面 JS 看不到连接层）。 |
| `render-check.mjs` | 渲染保真度小检：背景色块 / 绝对定位 / 中文 / canvas 像素 + 截图。 |
| `site-open-probe.mjs` | 真实站点「打得开吗」：导航 + DOM 体量/元素计数/正文摘录 + 截图（只读、不登录）。 |
| `public-search-flow.js` | Lightpanda 1.0.0 无模型 PandaScript：在 `quotes.toscrape.com` 公开练习站选择作者/标签，经 AJAX 筛选并抽取引文；无登录、无 Cookie。 |
| `chrome-search-flow.mjs` | 同样本的 Chrome 154 headless 基线：新建独立临时 profile，经 CDP 执行相同筛选并记录从浏览器启动到结果读取的耗时；不接触用户浏览器/登录态。 |
| `measure-search-flow.py` | 对上述两个脚本交替执行重复测量，采样各自进程树的 RSS 总和，保存原始结果、墙钟耗时和退出状态；无账号、无模型、隔离 Chrome profile。RSS 是进程 RSS 之和，不能当作系统物理内存或普遍性能结论。 |
| `compare.mjs` | 把 N 份 JSON 快照排成逐项对照表 + 自动分歧清单。 |
| `run.sh` | 上面全部的一键编排。 |

## 参数

```
run.sh [--out DIR] [--skip-download] [--keep-running]
# 环境变量：LP_BIN / LP_CACHE / LP_VERSION / CHROME_BIN / HEADER_PORT / LP_PORT / LP_ZH_PORT / LP_FULL_PORT / CHROME_PORT
node cdp-probe.mjs --endpoint http://127.0.0.1:9225 --url http://127.0.0.1:8799/ --label x --out x.json [--settle 3000] [--headers-dump URL] [--screenshot x.png]
node site-open-probe.mjs --endpoint http://127.0.0.1:9223 --url https://chat.deepseek.com/ --label deepseek --out d.json [--wait 9000] [--screenshot d.png]
node compare.mjs --out out.md lightpanda=a.json chrome=b.json
```

## 注意

- 沙箱内 Chrome 会 `Abort trap: 6`，跑对照需放行一次提权（同一命令已加白名单前缀）。
- Lightpanda 二进制（94MB）默认缓存在 `/private/tmp/lightpanda-research/`，不入库。
- 想换成别的浏览器只需换 `--endpoint`；探针不认识的能力会写 `{error:...}` 而不是崩掉。

重复对照：`uv run --no-project scripts/research/lightpanda-fingerprint/measure-search-flow.py --lightpanda /private/tmp/lightpanda-research/lightpanda-x86_64-macos --rounds 3 --out /private/tmp/lightpanda-search-repeat.json`。加 `--page-url https://quotes.toscrape.com/js/` 可改为公开页面正文抽取：LP 等待 networkidle；Chrome 等待导航完成后，再等完整加载与首条引文（若有）。这两种等待口径分别反映适配实现成本，不能当作引擎本体耗时。测试窗口取交替顺序，墙钟包含命令启动/收尾；资源采样间隔为 100ms，短时峰值可能漏采。默认每次建立新 profile，输出与用户配置无关。
