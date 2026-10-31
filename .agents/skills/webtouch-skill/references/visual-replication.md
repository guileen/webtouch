# 像素级复刻/对比：区域扫描（webtouch-skill 引用，仅相关任务必读）

> 结论：**别把整页左右并列一次丢给 VLM**。VLM 在整页图上会「看花眼」，对局部的
> 字号/间距/位置感知被媒体图带偏，容易漏抓真差异或臆测假差异。像素级对齐必须走
> **「按区域裁剪→源站|本地左右并列→逐区域喂 VLM→聚合 verdict」** 的标准流程，
> 这是 VLM 应做的**主动判断**，也是本技能的系统提示词级约定，每次对比任务自动执行。

标准步骤：
1. **锚定同一状态**：双侧同为 hero 第 0 帧 / 同一滚动位置 / 同一页面态（详情/播放态）。
2. **抓截图**：视口固定（如 390x844@DPR3），fresh 载入，`window.scrollTo(0,0)` 后截图。
3. **裁剪+并列**：按区域（nav / hero-overlay / popular-heading / row1 / install-bar…）
   用 ffmpeg `crop` + `hstack` 生成每区 `源站|本地` 并列图。
4. **逐区域喂 VLM**：`uws chat "…只在该区域内比对…" --img 区域图`，提示词里显式：
   「忽略照片媒体的压缩/缩放/亮度细微差、卡片 AI 水印/版权角标、viewCount 数字文字（实时计数）」，
   避免把媒体/版权/实时数字误判为 divergent。
5. **聚合**：汇总各区域 verdict，取其最低置信；有 DIFF 才返工，全 PASS 才算 consistent。

一键 harness（复用工具）：`.mem/drama/recon/compare-regions.mjs <src.png> <local.png> <outdir>`
——自动裁剪→并列→逐区 uws chat→写 `report.json`。生产截图用 `.mem/drama/recon/capture-page.mjs`。

抽样/测量补充：除视觉外，直接用 CDP 读源站 computed style 与 boundingRect（fontSize/lineHeight/
object-position/borderRadius/gap）作为「权威规格」，比目测更准；字体差异（如源站 Noto Sans vs
本机系统字体）会改变换行点，需加载同字体（Google Fonts @import）对齐。

## 进阶：把「VLM 说一致但人审发现不同」这类漏抓找出来（2026-08-28 v2 实测）

> VLM 的区域扫描会漏抓「被媒体图/实时播报带偏」的真差异（人脸复核发现 banner 遮罩、卡片其实不同，
> 而区域扫描仍报 PASS）。VLM verdict 只作**第一遍粗筛**，关键区域必须用**确定性量测**二次确认。

1. **先锚定同一帧，再裁剪**：自动轮播（~3s/帧）会让源站/本地停在不同帧（一帧亮图一帧暗图），
   直接并排会把「帧不同」误判成「遮罩/亮度不同」。必须**点击视觉最左分页点 + 读到同一标题文本**
   （如都读到 `My Billionaire Boss…`）后再截图。工具参考 `.mem/drama/recon/capture-f0.mjs`。
2. **跨域图片会污染 canvas，别用页内 `drawImage+getImageData` 量亮度**：源站图是跨域 CDN，
   `getImageData` 抛 SecurityError 导致整个 evaluate 返回 `undefined`。改从**截图**上用 ffmpeg
   采样像素亮度（`crop=…:…:…,scale=1:1,format=gray` → 读单像素值）；图 src/object-position/filter
   用只读的 DOM computedStyle 拿（无污染）。
3. **隔离「媒体差」 vs 「遮罩/样式差」**：在两侧 `src.png`/`local.png` 的**同一设备像素坐标**取样
   （DPR3 → device px = CSS px × 3）：①无遮罩图像区（如人物面部）应完全一致 → 证明同一张图渲染相同；
   ②纯遮罩区（避开文字/按钮的小块）应一致 → 证明遮罩已对齐。面部 83/84、纯遮罩 3/3、4/4、19/19
   一致，则遮罩对齐（不要只看整条大带的均值，会被白色标题字带偏）。
4. **遮罩/渐变结构要按源站量，不要凭观感猜**：源站 hero 遮罩是**底部 40%** 的
   `absolute inset-x-0 -bottom-1 h-2/5 max-h-72 bg-gradient-to-t from-black to-transparent`
   （CDP 读取 `backgroundImage=linear-gradient(to top, rgb(0,0,0), rgba(0,0,0,0))` + rect 高度≈40%），
   不是本地原用的全幅 `inset-0 from-black via-black/25 to-black/40`。后者顶部被压暗、渐变分布不同。
5. **遮罩层与其上内容必须明确 z 序**：给遮罩设 `z-index`（如 `z-1`）后，标题/Play/分页点容器必须
   `z-10`（或更高），否则遮罩盖住内容、把按钮「压暗」（Play 看着发灰）。验证：对比按钮的计算色
   `rgb(247,87,34)` 与渲染采样是否一致即可暴露。
6. **区域范围扩展到全页 + 跨页**：`nav/hero-overlay/popular-heading/row1/install-bar` 只覆盖首屏，
   要核「视频卡片」请把 `row2`/`footer`/`below-fold` 也纳入；且复刻要按页面逐个扫描
   （`home/detail/play/checkout/login/me`），不能只扫 home 就说全站一致。
7. **跨页审计要主动列能力缺口**（不只比视觉）：评论（源站详情/播放全屏 `scrollH=844` 无评论 →
   本地无评论属一致）、支付（源站=真实 Stripe/Adyen 卡片表单 `Card number/Expiry/Pay` 按需弹出 vs
   本地=mock 权益收银台 `解锁本集/VIP/金币`，v1 明确真实支付不在范围）、品牌一致性（局部页面是否
   仍用旧品牌色而与首页 B25 品牌不符）。

## 响应式/多分辨率扫描（2026-08-28 桌面宽屏实战）

1. **内容列规则先于截图**：这类站点的「responsive」不是改列数，而是**内容列恒为 max-w-xl(576px)
   居中**——桌面宽屏下源站 main = `mx-auto w-full max-w-xl`，hero 有 `max-h-[700px]` 上限，
   固定顶栏/安装条同样 `inset-x-0 + mx-auto + max-w-xl`。先 CDP 读 `main`/`hero`/`nav` 的
   rect 与 class，确认「宽屏收窄」后再谈细节；本地曾全宽拉伸（hero 1440×1765 vs 源站 576×700）。
2. **多分辨率清单**：390×844 / 768×1024 / 1024×768 / 1280×800 / 1440×900 / 1920×1080 六档，
   每档 fresh 载入 + 锚定轮播第 0 帧 + 视口截图 + 全页截图（`captureBeyondViewport`）+ CDP
   量测（nav/hero/grid 列数/卡图 w×h/横向溢出），存 JSON 逐档对比，不要只扫 390。
3. **区域裁剪要按全页坐标分档**：桌面区域（nav / hero-img / hero-overlay / popular-heading /
   row1-3 / footer / install-bar）与移动端不同；VLM 对「1px 亚像素偏移 + 媒体压缩 + 实时角标数字」
   会误报 divergent——用 ffmpeg 提取两侧同一坐标的文字像素包围盒（行/列分布）做确定性裁决，
   或把提示词收窄为「只比布局结构，忽略图片内容/角标数字」重问。
4. **固定元素也要收窄**：fixed inset-x-0 的顶栏/安装条/沉浸播放器在宽屏同样要
   `mx-auto w-full max-w-xl`（播放器 = 全屏黑底 + 576 视频列），否则只有内容列收窄、
   上下条仍全宽，观感断裂。
5. **小图标尺寸差只有小分辨率才暴露**：顶栏 logo(h≈19px)/头像(24px)/设置(20px) 的固定尺寸差，
   在 1440 全宽截图上 VLM 会放过，390 档一眼即见；每档都量 `img` rect 与源站对拍，
   且 logo 若在源站是视口绝对居中，本地 `justify-between` 会让它偏 2-3px。
