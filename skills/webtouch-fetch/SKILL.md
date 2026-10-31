---
name: webtouch-fetch
description: 用 WebTouch 匿名读取公开网页或保存截图。
---

公开页面使用 `webtouch fetch --url https://example.com --format markdown`；截图使用 `webtouch fetch --url https://example.com --format screenshot --output page.png`。解析 stdout JSON，失败时报告错误；内容可能是登录页、挑战页或空壳，应检查结果并在需要时回源核验。需要登录态或交互时使用 WebTouch 的受管 Chrome 路径。参数和身份边界见 `docs/cli-fetch-contract-2026-10-31.md`。
