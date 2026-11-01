webtouch — 浏览器自动化底座（Agent 自查手册）

命令主名 `webtouch`。`uws` 是已弃用别名（同一二进制；以 `uws` 启动时 stderr 提示改用 `webtouch`）。
下文示例一律写 `webtouch`。仍沿用的旧名：状态目录 `~/.uws/`、Chrome profile `~/.uws-chrome-debug`、
环境变量 `UWS_*`（同义覆盖：`WEBTOUCH_ENGINE_BIN` / `WEBTOUCH_ENGINE_ADDR` / `WEBTOUCH_GATEWAY_BIN` / `WEBTOUCH_STATE_DIR`）、
引擎与网关兼容文件名 `uws-engine` / `uws-gateway`（正式产物为 `webtouch-engine` / `webtouch-gateway`）。
Go module 路径仍是 `uws`（内部 import，不是对外命令名）。

用途
  WebTouch 把 LLM「智能」离线蒸馏成规则/ONNX，低成本驱动真实 Chrome：语义派发优先（单步
  <50ms），语义卡住（no_match / 置信度<θ）才唤 VLM 兜底；并提供网页大模型接管网关。

运行模式硬规则（2026-09-03 定；**2026-09-23 放宽构建/重启，2026-09-24 与 Tasks.md 对齐**）
  - 稳定版入口固定：/usr/local/bin/{webtouch,uws,webtouch-engine,uws-engine} → 仓库 bin/ 内最近成功构建
    （`uws` 与 `uws-engine` 是弃用软链）
    （release = make install、引擎升级 = make engine-restart，由维护者执行；属项目规范，
    见 world-agent/webtouch/AGENTS.md，本手册只讲使用）。
  - Agent 授权范围（**唯一事实源 = `Tasks.md`「停止条件与授权」，2026-09-23 用户定案**）：
    已授权、无需逐次询问 —— `make build/install/engine-restart`、`zsh -ic './webtouch daemon restart'`、
    `go build` / `go test`、在 `/tmp` 下写只读诊断脚本（含只读 CDP）、起停自建浏览器会话与 profile、
    清理自建 profile 锁。只剩两条硬约束：(a) 改完源码必须 restart 才能宣称新版本生效（防版本漂移）；
    (b) 不得用重启/清锁掩盖真实账号页的风险信号（那只锁账号页，不锁引擎）。
    环境异常（入口缺失 / probe 缺 wasm / 版本漂移 / 锁残留）先自行修复，修不了再上报。
  - 执行模式按任务选（2026-09-23）：
      * 简单、已知、可逆目标用一句自然语言目标或一批 DSL，保持快速；
      * 多步、语义歧义、重复失败或需要页面后验的任务，依 `webtouch-skill` 由调用 Agent
        主控：同一专属 `--session`，每次只发一条 DSL，检查 stdout 的 URL/AXTree/截图后决定
        下一步。`SUCCEEDED` 只表示调用成功，不等于页面目标达成。
    目标模式仍支持一句自然语言目标并自动分流——
    `webtouch "发布微博：xxx"`（等价 `webtouch chat "..."` / `webtouch -p "..."`）：
      * DSL 语法校验通过 → 自动走确定性 `run --plan`（不烧 VLM）；
      * 否则 → 目标模式：VLM 每轮读「页面语义 AX 内容树 + 可交互 ref 树」自决下一步
        （截图仅辅助视觉，正文判断以语义树为准；链接行/ref 行带 href，重复短标签/泛化动作
        可带最近行上下文；同文
        「文章页链接 vs # 锚点」可区分）；引擎按 node_id 确定性执行；
        当前页已直接回答目标时 VLM 应直接 done 总结，不重复导航；
        同一节点重复且页面无变化 → 先带候选 href 提示换一次决策，仍重复则
        停下给可消费诊断（不再空转到轮数上限）；
        登录/验证页停下等用户（回车续跑；非交互输出 WAITING_USER，登录态由持久
        Chrome 保留可重跑）；付费/扣费护栏 PAUSED「需要确认」，无绕过；
        结束自动输出状态 + 当前 URL + 页面 AXTree 摘要 + 截图。
        `webtouch "目标" --debug`：逐轮把中间过程打到 stderr（URL/树规模/每轮截图、
        VLM 原始响应与决策、引擎动作结果），VLM“看着笨/卡住”时用它定位；
        stdout 报告契约不变。
    `webtouch chat "..." --img/--url/--page/--diff ...` = 义眼模式（看图/对比/评审）。
  - 确定性操作层级：按任务在目标模式（VLM 决策）与 DSL（`webtouch run --plan`，整段一批或单步）
    间选择 > Python（禁用）。目标模式提示词要求候选与目标相关，节点存在本身不构成点击理由。
    高层缺能力 → 停下上报缺口，不自降级写脚本。
  - 会话续页（v1.5.6）：`webtouch run/goal --session <ID>`（默认 default）——同一 ID = 同一
    标签页上下文；命令停住/失败后带相同 `--session` 的下一命令**原地续页**，不再新开
    空白 Tab 找不到目标。报告输出 Session ID + Target ID（目标模式/run 均带）。
    单任务打开新窗口/新标签页时，只在引擎明确回报活动 Target 已切换时跟随；否则
    `PAUSED / new_window_opened` 并保留原 Tab。暂停后不可把 probe 新开的空白页当成原任务续接。
  - 共享浏览器认知（2026-09-03）：9222 调试 Chrome 可能同时被其他进程/人工使用。
    每个新会话由 webtouch 自动新建独立 Tab 并钉扎 Target ID，所有动作/快照只在自己的
    Target 上执行；**不要把别的标签页/活动页当成自己的上下文**。不同任务/进程请用
    不同 `--session`；共享同一 Session 的人必须传同一 Session 才续同一页。`probe` 不带
    `--session` 绑定，不要假定其标签页能被后续 run/goal 续接。
  - 浏览器唯一实例 = 持久调试 Chrome（端口 9222、profile ~/.uws-chrome-debug 固定；
    由 webtouch 在 --attach debug 时自动拉起并 setsid 常驻，不用 launchd、不用 Agent 手工启动）。
    引擎唯一实例 = webtouch daemon（50051，~/.uws）。
    禁止长驻会话/nohup 保活、禁止自定义存储、禁止给浏览器起别名、禁止手工删 profile 锁。

快速启动（维护/调试模式；普通任务用固定入口，跳过本节）
  make build                # 产出 bin/webtouch + bin/webtouch-engine（仓库根 webtouch/uws 为软链）
  ./webtouch daemon start   # 拉起引擎守护进程（默认 127.0.0.1:50051，真实 Chrome 模式）
      # 引擎 gRPC 默认 127.0.0.1:50051，API Key 默认 uws_dev_key；
      # 状态/重启/停止：webtouch daemon status|restart|stop
  GOCACHE=/tmp/uws-go-cache go run ./cmd/uws-gateway --engine-addr 127.0.0.1:50051 \
      --attach-cdp http://127.0.0.1:9222
      # 网关仍处开发模式（仅维护者 go run）：HTTP 默认 127.0.0.1:8000；
      # 首次在自动弹出的 Chrome 里登录一次 DeepSeek/Gemini
  curl http://127.0.0.1:8000/v1/health   # 网关+引擎健康
  curl http://127.0.0.1:8000/internal/provider/models  # WebTouch 内部网页模型目录；客户端目录走 Agent Hub
  curl http://127.0.0.1:8000/v1/auth/status  # 各站点登录态

附着目标（--attach，run/explore/api 通用）
  debug    持久调试 Chrome：profile ~/.uws-chrome-debug，端口 9222；未运行自动拉起。
           登录态与日常浏览器隔离，适合自动化/反爬实验。
  default  用户日常 Chrome：默认 profile，端口 9223；已在调试端口监听则复用，
           运行中无调试端口时先优雅退出再带 --remote-debugging-port 重启
           （窗口/标签恢复、登录态保留）；非交互环境需 UWS_ATTACH_CONFIRM=1。
  <url>    显式 CDP 端点（如 http://127.0.0.1:9222），原样使用。

窗口与焦点（同指纹共享窗口，2026-09-11；默认模式与几何 2026-09-24）
  附着模式默认**同一个浏览器指纹只开一扇受管窗口**（newWindow+background+focus:false，
  默认 1440x900），后续会话的 Tab 都开进那扇窗（由窗内 webtouch 自建锚页 + 中键产生，
  逐次用 Browser.getWindowForTarget 读回归属，落错窗口即关掉该 Tab 并转专属小窗）。
  默认落位 = 工作区内右上角固定位（留 24 DIP 边距），尺寸夹进工作区，并**读回实际矩形**写日志
  （读回值才是生效值）；工作区读不到时不搬窗并告警。旧默认「停到屏外」已退为显式选项。
  UWS_WINDOW_MODE=shared|window|tab   shared（默认，同指纹一扇窗多 Tab）/ window（每会话一扇
                              专属小窗，零前置）/ tab（旧行为：落到当前窗口的新标签，实测会
                              落进**用户自己的窗口**，禁止作默认）
  UWS_WINDOW_SIZE=WxH          默认 1440x900（正常网页尺寸；超出工作区时夹取，显式值不被改大改小）
  UWS_WINDOW_POS=auto          默认：工作区内右上角固定位
                 offscreen|park|hidden  显式要求停到屏幕外（**macOS 会夹取**，实测屏右缘只剩一条，勿当隐藏用）
                 X,Y / none|keep        指定落点 / 不动位置
  UWS_INPUT_ACTIVATE=auto|always|never  兼容保留：当前所有取值都**不自动前台化**（R1 硬约束）
  焦点事实（Chrome 153 实测 2026-09-24）：**建窗、搬窗、页面内派发输入都不抢系统焦点**；
  唯一会抢一次的是「往某个窗口里新建 Tab」——默认 shared 模式每起一个会话因此会把该指纹
  的窗口前置一次（已定取舍：一指纹一窗优先）；要零前置用 UWS_WINDOW_MODE=window。取证见
  docs/evidence/pilots/window-tab-governance-2026-09-24.md。
  无头浏览器自动回落 tab 行为；要「看着它跑」请自己把该小窗切到前台（webtouch 不代为激活）。

观看小窗（每个会话一块原生面板，2026-09-16 补关闭与自动收面板）
  - 小窗是任务进度条：会话一动就出现，**Agent 静默 5 分钟（默认）后自动收起**；
    会话、真窗口与 `--session` 续页语义都保留，Agent 再动一下面板就回来。
    UWS_VIEWER_IDLE_CLOSE_SEC=N  覆盖静默秒数（0/off 关闭该行为）；改引擎环境后需 restart。
  - 小窗右上角有「✕ 关闭」：用户随时可以自己收掉它，**只收面板**（不杀任务、不关真窗口）。
    关掉之后画面照旧在流，但除非该会话有新动作，否则不再弹回来。
  - 引擎重启/重连时壳会清空全部旧面板（不留一张没人维护的陈旧画面）。
  - 把真窗口展开看着时（点小窗 = 展开真页面）不会自动收面板：那颗「↩ 收回小窗」胶囊
    是用户唯一的收回入口。
  - 只看不管：`webtouch viewer status`（引擎是否在推流 + 壳是否在跑）、`webtouch viewer app`（拉起壳，单实例）。

子命令（多数转发 python3 -m coordinator.cli）
  webtouch "目标文本" / webtouch chat "目标文本" / webtouch -p "目标文本"
              # 推荐入口（chat 目标模式，2026-09-03）：
              # 自然语言目标 → DSL 语法有效自动走 run --plan；否则 VLM 每轮
              # “截图+AXTree → 自决”执行到 done/impossible/登录等待/护栏暂停。
              # 例：webtouch "发微博：今天天气不错"；登录页停下等你回车；结束给状态和页面。
  webtouch daemon start|stop|restart|status  直接控制引擎守护进程（webtouch 是唯一生命周期入口，取代 launchd）
  webtouch run     --attach debug --target-url <url> --task-file task.json [--output result.md] [--screenshot]
              --plan "<DSL>" | --plan-file plan.txt   # 请求级 DSL（nav/drag:x1,y1,x2,y2/click:node:12/extract|$var/reply）
              --observe --session <ID> [--screenshot] # 只读续接已有 session；不会启动/重启 Chrome 或执行页面动作
              # 结果契约：完整报告始终直接打印 stdout（Agent 消费 stdout，不要再读文件）；
              # --output 只在需要留档时另存一份副本。
              # 单步 = 原命令只传一条 DSL：webtouch run --plan "click:站点管理"（不写文件、
              # 直接打印 stdout）。任务正常结束后输出「当前页面（任务结束点）」：
              # 当前 URL + 停留页面 AXTree 摘要，Agent 读完即可继续下一条 DSL。
              # --screenshot 在成功回执附当前 Target 本地截图路径，供 Agent 按需自行查看；
              # 默认不开截图，保持简单/快速 DSL 路径的既有开销。
              # --observe 必须显式给已有 --session；只用已就绪 CDP，不接受 --target-url / plan / task-file，
              # 只初始化会话绑定并读取当前 URL/AXTree，可选截图。about:blank 表示没有可确认的页面。
              # 失败/暂停会停住并输出 Failure/「需要确认」报告：失败步骤+原因+当前 URL
              # +AXTree 片段+截图路径；VLM 可用时附自然语言诊断（不可用会明说，
              # 绝不假装看过图）——先读报告再决策，不要裸重试/猜页面文字。
              # 执行护栏：付费/扣费类动作或页面付费 CTA + 确认类点击 → PAUSED「需要确认」，
              # 附证据并停下等用户；WebTouch 无绕过参数，不会自动下单/支付。
  webtouch crawl   <url> --depth 2 --signals title content author --format md
  webtouch explore <url> --max-pages 12 --max-depth 3 --settle-sec 5 [--output report.md]
              # 启发式探索：BFS 同域链接 → 页面类型分类（landing/detail/player/auth/me/
              # checkout/search）→ 导航图（Mermaid）+ 功能清单 Markdown 报告
              # （无论是否 --output，完整报告都直接打印 stdout；--output 仅另存副本。
              # 每页关键截图：--output 时存 <报告名>.assets/，否则存 cwd/uws-explore-assets/；
              # --no-shots 可关；纯规则不调 VLM）
  webtouch probe  [--url <URL>] [--keep-open] [--output report.md] [--attach debug]
              # 附着持久调试 Chrome 做一次“看一眼”：两段式 AXTree（语义树+ref，
              # 链接带 href）完整打印 stdout；截图默认存 ~/.uws/artifacts/probe-<pid>.png。
              # --output 仅把报告另存为文件（截图改存同目录 <报告名>.png）。
              # 观察已登录控制台 / 等人登录后确认页面状态用它；
              webtouch api     [start|status|help] [--listen 127.0.0.1:8000]
              # 启动 WebTouch 浏览器 provider 执行服务；不提供客户端模型 API
              # 账号、Profile/指纹绑定、站点会话与风险控制由 WebTouch 持有；客户端模型 API 走 Agent Hub
  webtouch qa      --url <URL> [--ref-img <PNG>] [--viewports 390,768,1440] [--output build/qa]
              # WebTouch QA Studio 前端多端交付验收工作台（AI 编码时代的交付质检员）
              # 10 秒切换 390/768/1440px 视口，零 Token 纯算扫描横向溢出、控制台异常、资源死链与热区违规；
              # 自动像素差分（洋红高亮 Overlay）并签发自包含 HTML 与 Markdown 交付验收单
              # 混合指挥：AXTree → VLM 指挥官 → DSL 步骤（node: 确定性引用）
  webtouch chat    "判断两个实现是否一致" --img ref.png --img clone.png [--json|--plain]
              # WebTouch 义眼模式（显式视觉输入时自动分流）：看图/对比/评审
              # （单图评审/图对图/多站点），
              # 返回结构化判断（verdict/summary/checks/confidence）给 Agent 消费；
              # --img 给本地/URL 图，--url 开站点截图，--page 截当前会话，-p/--prompt 为顶层别名
              # 自然语言自决：只问「帮我检查 a.com 和 b.com 的 differences」即可自动提取站点并像素 diff
              # --url 截图稳定等待由「问题（提示词）」驱动：提示词写 等 2 秒 / wait 1.5s / settle 2
              #   即用显式时长；含详情/视频/播放/剧集等重内容信号自动 1.2s；否则 600ms（P1，未加 --settle-ms）
  webtouch config  set <key> <value>   /   webtouch config get <key>   (fallback_api_key 脱敏)
  webtouch session create [--channel <id>] [--account <账号>] [--profile <id>]
              use|show|close --session <id>    list [--all]
              # 对外 session 生命周期。stdout 为 JSON（webtouch.cli.session.v1）。
              # 状态在本机 ~/.uws/cli-sessions.json；CLI 只携带 session_id。
              # 句柄也可用 WEBTOUCH_SESSION / UWS_SESSION。不新建指纹，凭据不进参数。
  webtouch vault   init [--force]   /   set <名> [--note <说明>] [--stdin]   /   get <名> [--fingerprint]
              list   /   rm <名>
              # 本地凭据库（age 加密，只落本机、不联网、不做对外服务）：账号密码/API Key 统一收口
              # 默认 ~/.uws/vault.{key,age}（0600，UWS_VAULT_KEY / UWS_VAULT_FILE 覆盖）
              # set 走终端无回显（值不进 argv/history），只回 sha256 指纹；get 是唯一明文出口；
              # list 只给名字+指纹+时间，不显示明文。搬凭据给别的系统用 --fingerprint 双端比对。
              # 边界：identity 与密文同目录，防误传/误提交/备份泄露，不防本机已被读取。
  webtouch state   export <profile-id> <origin> --target-id <id> --out <bundle.json> [--key-file <key>]
              restore <bundle.json> --profile-file <FingerprintProfile.json> --session <new-session> [--key-file <key>]
              # 跨端逻辑会话迁移：恢复 Cookies、LocalStorage 与当前 tab 的 SessionStorage；
              # restore 先以同一指纹档案创建未导航上下文，再写入状态；
              # 随后用相同 --session 的 run/goal 导航。导出默认文件 0600，Cookie/密钥不回显。
  webtouch request <url> --profile-file <FingerprintProfile.json> [--state <StateBundle.json> --key-file <key>]
              [--method GET] [--header 'Name: value'] [--body <text>|--body-file <path>]
              # 同身份的纯 HTTP 请求：复用 UA、语言、低熵 Client Hints 和 Bundle Cookie，不启动 Chrome；
              # Cookie/UA/Client Hints 不能手填覆盖，默认不跟随重定向。TLS/HTTP2 仍是 Go，不等于 Chrome。
  # run/crawl/api 在引擎未监听时会按需自动拉起（不再依赖 launchd/手动开进程）

引擎守护进程（webtouch daemon）
  地址默认 127.0.0.1:50051（UWS_ENGINE_ADDR 覆盖；子命令可用 --engine-addr 覆盖，但非托管地址不会自动拉起）。
  pid/元数据/日志落在 ~/.uws/engine.{pid,json,log}（UWS_ENGINE_DIR 覆盖，便于测试隔离）。
  引擎二进制默认取 webtouch 同目录的 webtouch-engine（兼容名 uws-engine），其次 PATH；WEBTOUCH_ENGINE_BIN 或 UWS_ENGINE_BIN 可显式指定。
  启动参数默认 --browser cdp --log-path ~/.uws/engine-onlinelogs.db；需要时用 UWS_ENGINE_ARGS 追加/覆盖。

浏览器执行与站点管理（对外入口是 CLI；HTTP 仅迁移期）
  webtouch session create|use|show|list|close
  POST /internal/provider/chat       已废弃。迁移期仍可响应；UWS_PROVIDER_HTTP=410 时退役
  POST /internal/provider/responses  已废弃
  GET  /internal/provider/models     已废弃
  POST /v1/sessions                  网关内部页签会话（不是对外契约）
  GET  /v1/sessions /v1/auth/status /v1/health
  会话锁定通道+模型；同网站账号复用既有 Profile/指纹绑定。
  WebTouch 的 /v1/models、/v1/chat/completions、/v1/responses 客户端模型入口已退役；客户端请连接 Agent Hub。
  Hub 应本机 exec CLI，并用 JSON 的 hub.channel / hub.site_conversation_id 记账。

引擎 gRPC（精确动作，契约=proto/uws_service.proto）
  InitializeContext（context_name=一个标签页，attach_cdp_url=附着已有 Chrome）
  ExecuteAction（target_semantic + expected_action；extract 可 context_hint prefer_sse=1）
  ExecuteAction.node_id（>0 跳过语义/选择器派发，按探针 BFS 序回查执行，v1.5.5）
  CaptureScreenshot（PNG 留证/vlm 打码） / ExportPageTree（导出极简 AXTree，混合指挥）
  CloseContext（幂等关 Tab）
  ExportState（v1.5.6：全量 Cookies JSON + 指定 origin LocalStorage / 当前 tab SessionStorage） / ImportState（v1.5.7：导航前恢复；profile_id 必须匹配）
  鉴权 metadata x-api-key。

请求级自动分解 DSL（auto-planner，coordinator/planner/planner.py）
  nav:首页 / search:A醇精华 / click:第1个结果 || reply:无结果 / extract:成分表 | $成分 / reply:$成分
  drag:124,200,300,200             原生拖拽（坐标直发引擎，v0.2；VLM 坐标兜底仍缺）
  click:node:12 / extract:node:31   确定性引用（探针 BFS 编号，跳过语义派发；
                                    VLM 指挥官经 webtouch commander 产出；引擎按 id 回查执行）
  a || b     失败回退 → on_failure（login→replan、reply:→abort、skip/retry/replan 原样）
  x | $var   本步输出绑定变量（变量名允许 Unicode）
  ?guard:hint 前置守卫（v0.1 只记录）；nav:/reply: 是计划级指令（引擎无 goto/reply 动作）
  注意：nav:/node:/$var/reply 已运行时打通（webtouch run --plan）；?guard 展开仍未做；
  引擎内“卡住才唤 VLM”仍走截图/坐标候选（未接 AXTree→node_id 闭环）。

错误速查（引擎 gRPC 状态码 → 对策）
  DEADLINE_EXCEEDED(4)    超时 → 先退避 1s/2s 最多 2 次（网关已内置 Self 循环）
  NOT_FOUND(5)            可信节点不存在 → 页面结构可能大变，重新规划，勿裸重试
  ABORTED(10)             节点脱离 DOM/遮挡 → 等一帧立即重试（引擎已内置自愈）
  FAILED_PRECONDITION(9)  未初始化/Target 崩溃 → 重新 InitializeContext
  UNAUTHENTICATED(16)     WAF/验证码 → 截图 → VLM 打码或人工接管
  RESOURCE_EXHAUSTED(8)   Target 池满/配额 → 降级阻塞
  error_reason 含 target_destroyed = 标签页被关（网关自动重建并整轮重试一次）。

反模式
  1 跨任务共享 context_name（Cookie 物理隔离，务必一任务一上下文）
  2 NAVIGATING 时立刻下一步（应等稳定）
  3 拿 LLM 解释整个页面（WebTouch 是规则驱动，直接派发语义即可）
  4 --browser sim 做真实调试（sim 只回模拟结果，务必 cdp）
  5 无限重试（按错误速查表退避，NOT_FOUND 不裸重试）
  6 普通任务写 python/gRPC 脚本取树/截图（用 webtouch probe；缺能力就上报）
  7 nohup / 长驻会话保活 Chrome、自定义 profile/存储、给浏览器起别名（Debug/CDB Debug 等）
  8 Agent 手工 rm ~/.uws-chrome-debug 的 Singleton* 锁（工具自动处理，介入前先确认）
  9 失败后不看证据直接裸重试/猜选项文字（先读 webtouch run 的 Failure 报告与截图/AXTree）
  10 对「需要确认（付费/扣费）」暂停加参数/改计划绕过——必须先停下询问用户本人
  11 为了执行 DSL 先写临时计划文件再让 webtouch 读（两次工具操作）——`webtouch run --plan`
     直接传 DSL 字符串；交互/向导逐步走就用原命令只传一条：`webtouch run --plan
     "<动作>"`（不加新命令、不写文件）
  12 动作弹出新窗口/新标签（ContextState=NEW_WINDOW_OPENED）时当成功——当前
     目标模式与 run 不自动跟随新 Target，看到该状态或目标无进展先停下上报/
     请用户接管，别把“点了”当“成了”
  13 页面出现账号风险/安全/风控提示却静默继续——与付费护栏同级：停下把原文
     报给用户确认，不许跳过（重要发现 = 暂停点，不只是失败才算）
  14 为了拿结果传 `--output` 落盘再读文件（两次工具操作）——webtouch run/goal/explore/probe
     的完整结果都直接打印 stdout，Agent 直接消费 stdout；--output 只在需要持久化留档时用
  15 把共享调试浏览器当成自己独占（捡活动页/别的标签页当上下文）——持久 Chrome 可能
     正被其他进程/人工使用；webtouch 每个会话只操作自己创建并钉扎的 Target（报告里
     Session/Target ID 可续页定位），跨进程协作请显式共享同一 Session ID，绝不偷
     别人的 Tab

最佳实践（2026-08-28，复刻/验收实测）
  1 能用提示词就别加 CLI 参数；只在确定性原语能替代 VLM、明显省 token 时才加
    （如 --diff/--zoom/--region-color 属此类）。采样时机（--settle-ms）、机器可读规格
    （--spec-json）、忽略动态媒体（--ignore-region）均不省 VLM token，故一律提示词驱动。
  2 语义可用 全角标点 + ASCII 点号（如「都市逆袭：外卖小哥逆天改命」「25.8M」）——sanitize
    已放行，被拒时优先怀疑页面结构/元素缺失而非过滤规则。
  3 对比/验收先「锚定同一状态」：参考站详情 URL 直开即全屏播放态、线上通常是详情页；误比会
    判假 divergent。可在 prompt 里显式要求「双侧同为播放态/详情态」，或先用引擎驱动到目标态再比。
  4 义眼 webtouch chat 适合「看一看给反馈」（视觉/布局/一致性）；功能驱动（点选/填表/抽屉）
    用 `webtouch run --plan "<一条 DSL>"` 单步推进（原命令，不新增 webtouch step；每次返回
    「当前页面」URL + AXTree 摘要）；稳定已知路径一次走完用
    `webtouch run --plan "<整段 DSL>"`（命令直传，不写临时计划文件）。
  4b 失败 SOP：先读 Failure/「需要确认」报告（原因/AXTree/截图/VLM 诊断），不要裸重试、
    不要猜页面文字。仍不确定 → 用 `webtouch chat "看看这一步为什么失败/页面现状是什么"
    --img <截图> --plain` 请 VLM 接管解释；还不行就继续用原 `webtouch run --plan
    "<动作>"` 单步 DSL 一条条试，每步都拿到 AXTree 摘要再决定下一步。
  5 采样对象要先确认是最新构建（Vite 缓存会让服务返回旧模块，webtouch 据旧图下错结论）——
    对比结果应看得出截图来源/时间/哈希。
 6 接口层级目标：DSL > chat > python（`webtouch run --plan`（整段或单条）> `webtouch chat`
    > 引擎 gRPC/脚本）。能用高层接口就别掉到 python 脚本；drag 已上提 DSL
    （v0.2：`drag:x1,y1,x2,y2`），
    VLM 坐标兜底仍缺（USER_ACTION #28）。
  6b 输出契约：stdout 第一（tool for agent，files only for persistent）——run/goal/
     explore/probe 完整结果都直接打印 stdout，绝不传 --output 后再去读文件；
     --output 只在需要把报告留档/分享时另存副本。
  7 演示/给人看：用 --attach debug 可见 Chrome 窗口（headless 截图观众看不到过程）；
    先复用现成资产（如 go test ./pkg/browser -run TestCDPSessionDrag），别新建演示页/脚本。
  8 坐标能确定性拿到（页面几何已知/可控）就别调 VLM；VLM 只做语义/视觉兜底，不为展示能力烧 token。
  9 会话凭证延续/State Export（v1.5.6，2026-09-02 实测）：InitializeContext 记住返回的
    target_id → 业务态就绪后 ExportState(target_id, target_origin)，返回全量 Cookies JSON
    （含 HttpOnly，Network.getAllCookies）+ 指定 origin LocalStorage。实测对 chatgpt.com
    可导出 __cf_bm（HttpOnly+Secure）。完整链路：初始化 →（403+cf-chl → ExecuteAction 抛
    gRPC UNAUTHENTICATED = CHALLENGE_INTERCEPTED → drag:x1,y1,x2,y2 坐标解锁）→
    受保护界面 → ExportState。
  10 ExportState 回归三要点：target_id 必须回传 init.target_id（非 task_id）；新增 RPC 必须
    同步注册 pkg/engine/auth.go 的 ACL（漏了返回 PERMISSION_DENIED unknown method）；
    Network/DOMStorage 域方法必须走 CDP 会话路由（CallSession），不能浏览器级 Send。
    验证命令（真实 Chrome，沙箱外）：
    UWS_E2E_CDP=1 .venv/bin/python -m pytest tests/e2e/test_export_state.py \
      tests/e2e/test_engine_captcha.py tests/e2e/test_coordinator_vlm.py -v
    挑战分支用本地 403+cf-chl 页做确定性门禁；外部 CF 站点（chatgpt.com）策略随环境漂移，
    不能当常绿 E2E。

深读：.agents/skills/webtouch-skill/SKILL.md（上层入口）与 docs/gateway-contract-2026-08-26.md。

匿名公开页读取：`webtouch fetch --url https://example.com`（HTTP，stdout JSON；不使用登录态）。
