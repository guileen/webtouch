package recipe

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// fetch_field 的内建取值器名（契约 §13：会话对象是站点自述事实，取值口径写进 recipe 声明）。
const (
	SelectLastAssistantMessageID = "last_assistant_message_id"
	SelectLastAssistantText      = "last_assistant_text"
)

// RecipeStatus 描述 Recipe 的生命周期状态。
type RecipeStatus string

const (
	StatusDraft    RecipeStatus = "DRAFT"
	StatusActive   RecipeStatus = "ACTIVE"
	StatusDrifted  RecipeStatus = "DRIFTED"
	StatusDisabled RecipeStatus = "DISABLED"
)

// Recipe 定义面向在线 AI 对话与搜索服务的声明式直发 Recipe（gateway contract §13 SSOT）。
type Recipe struct {
	Site     string            `json:"site"`               // e.g. "gemini", "deepseek", "chatgpt", "claude", "perplexity"
	Status   RecipeStatus      `json:"status,omitempty"`   // ACTIVE, DRAFT, DRIFTED, DISABLED
	Match    MatchRule         `json:"match"`              // 页面匹配规则
	Request  RequestSpec       `json:"request"`            // 请求模板与构造器
	Slots    []Slot            `json:"slots"`              // 槽位生产者（禁落明文凭据）
	Response ResponseSpec      `json:"response"`           // 响应分帧与增量提取
	Drift    *DriftSpec        `json:"drift,omitempty"`    // 漂移自检与探测
	Metadata map[string]string `json:"metadata,omitempty"` // 额外元数据
}

// MatchRule 判定当前页面 URL 是否匹配本 Recipe。
type MatchRule struct {
	Origin string `json:"origin"`            // e.g. "https://gemini.google.com"
	PathRe string `json:"path_re,omitempty"` // e.g. "^/app"
}

// Matches 判定指定 URL 是否命中。
func (m MatchRule) Matches(rawURL string) bool {
	if m.Origin != "" && !strings.HasPrefix(rawURL, m.Origin) {
		return false
	}
	if m.PathRe != "" {
		path := strings.TrimPrefix(rawURL, m.Origin)
		if idx := strings.Index(path, "?"); idx >= 0 {
			path = path[:idx]
		}
		re, err := regexp.Compile(m.PathRe)
		if err == nil && !re.MatchString(path) {
			return false
		}
	}
	return true
}

// RequestSpec 定义页内直发请求规格。
type RequestSpec struct {
	Method       string                 `json:"method"`                  // POST / GET
	URLTemplate  string                 `json:"url_template"`            // URL 模板，包含 {{slot}} 槽位
	Headers      map[string]string      `json:"headers,omitempty"`       // 请求头（如 Content-Type, X-Same-Domain）
	BodyTemplate string                 `json:"body_template,omitempty"` // 请求体模板（urlencoded 或 json 文本）
	BodyBuilders map[string]BodyBuilder `json:"body_builders,omitempty"` // 结构化请求体构造器
	Signer       *SignerSpec            `json:"signer,omitempty"`        // 站点自带签名函数（见 SignerSpec）
	TimeoutMs    int64                  `json:"timeout_ms,omitempty"`
}

// SignerSpec 声明「站点页面自带的异步签名函数」（契约 §13）。
// 语义：先用模板＋槽位拼出待签 URL，再 await 站点函数拿到签名结果，
// 用 URLFrom 覆盖实际请求 URL、把 HeadersFrom 的键值并入请求头。
// 只声明调用路径与取值位置，签名值一律不落 recipe 文本；不重实现签名算法。
// 任一步失败即 fail-closed 返回 not_dispatched（可无损降级 DOM）。
type SignerSpec struct {
	Path        string `json:"path"`                   // 页内签名函数路径，如 "__QIANWEN_CHAT_SDK__.qwenSign"
	URLFrom     string `json:"url_from,omitempty"`     // 签名结果里覆盖 URL 的字段（默认 signedUrl）
	HeadersFrom string `json:"headers_from,omitempty"` // 签名结果里并入请求头的字段（默认 signedHeader）
	TimeoutMs   int64  `json:"timeout_ms,omitempty"`   // 签名等待上限，<=0 表示不设限
}

// BodyBuilder 定义复合请求体（如 Google batchexecute 的嵌套 f.req）。
type BodyBuilder struct {
	Kind     string `json:"kind"`     // "json_outer", "json_object", "urlencoded"
	Template any    `json:"template"` // 模板树结构
}

// SlotProducer 常量定义。
const (
	ProducerPageGlobal = "page_global" // 从 window 全局变量求值（如 WIZ_global_data.SNlM0e）
	ProducerFromURL    = "from_url"    // 从页面 URL / location.pathname 正则提取
	ProducerCounter    = "counter"     // 页内计数器自增
	ProducerFreshUUID  = "fresh_uuid"  // 每次请求生成独立 UUID
	ProducerCookie     = "cookie"      // 从 document.cookie 读取
	ProducerStatic     = "static"      // 静态字面量
	ProducerPrompt     = "prompt"      // 用户输入
	ProducerModel      = "model"       // 模型标识
	ProducerFreshHex   = "fresh_hex"   // 每次请求生成独立十六进制串（长度 Length，默认 32）
	ProducerFreshBlob  = "fresh_blob"  // 每次请求生成独立不透明串（Prefix + base64url 随机 Bytes 字节）
	// 以下三样为 2026-10-01 DeepSeek 实测倒逼的扩展（契约 §13「recipe DSL 扩展」）。
	ProducerLocalStorage = "local_storage" // 从 localStorage 取值（Decode=json 时先 JSON.parse 再按 Path 下钻）
	ProducerPowChallenge = "pow_challenge" // 异步：先取挑战再复用站点 worker 解题，产出可直发的凭据串
	ProducerSessionMint  = "session_mint"  // 异步：首轮无会话 ID 时先建会话（已有则从页面 URL 取）
	// 2026-10-02 千问接站倒逼：后续轮的 topic_id 只在服务端（session/get 回带），
	// 页内 localStorage/全局都没有 ⇒ 需要「派发前先只读取一个字段」的生产者。
	ProducerFetchField = "fetch_field" // 异步：先发一次页内只读请求（可带当前槽位），按 Path 取字段
	// 2026-10-02 ChatGPT 接站倒逼：请求体里的 create_time 是**数字**（首轮实测 1790929344.951），
	// 字符串占位会被 JSON.stringify 引号化 ⇒ 需要「产出数字」的时间槽位。
	ProducerNowEpoch = "now_epoch" // 当前时间（Unix 秒，float；节点整串引用时保持数字类型）
)

// Slot 定义参数槽位与动态生产者映射。
type Slot struct {
	Name     string `json:"name"`               // 槽位名称，如 "at", "f.sid", "bl", "reqid", "conv"
	Producer string `json:"producer"`           // 生产者类型
	Path     string `json:"path,omitempty"`     // 对应 page_global 的属性路径
	Re       string `json:"re,omitempty"`       // 对应 from_url 的正则捕获组
	Seed     string `json:"seed,omitempty"`     // 对应 counter 的种子键名
	Step     int64  `json:"step,omitempty"`     // counter 步长
	Fallback any    `json:"fallback,omitempty"` // 缺省兜底值
	Optional bool   `json:"optional,omitempty"` // 首轮可缺失（值按 null 传，不触发 fail-closed）
	Scope    string `json:"scope,omitempty"`    // "page_load", "site_build", "conversation", "request"
	Value    string `json:"value,omitempty"`    // 对应 static
	Prefix   string `json:"prefix,omitempty"`   // 取值前缀（会话 ID 归一为 c_<16hex>、不透明串的 "!"）
	Length   int    `json:"length,omitempty"`   // 对应 fresh_hex 的字符数
	Bytes    int    `json:"bytes,omitempty"`    // 对应 fresh_blob 的随机字节数
	Key      string `json:"key,omitempty"`      // 对应 local_storage / cookie 的键名（缺省取 slot.name）
	Decode   string `json:"decode,omitempty"`   // 对应 local_storage："json" = 先 JSON.parse（解析失败退回原始串）
	// 以下字段服务异步生产者（pow_challenge / session_mint）。
	Endpoint      string `json:"endpoint,omitempty"`       // 前置请求路径（取挑战 / 建会话）
	TargetPath    string `json:"target_path,omitempty"`    // pow_challenge：被保护的 API 路径（挑战 body 与凭据内容都用它）
	Algorithm     string `json:"algorithm,omitempty"`      // pow_challenge：解题算法（默认 DeepSeekHashV1，由站点 worker 自己校验）
	Chunk         string `json:"chunk,omitempty"`          // pow_challenge：站点 worker chunk URL（复用站点自身解题能力）
	ChallengePath string `json:"challenge_path,omitempty"` // pow_challenge：挑战字段路径（默认 data.biz_data.challenge）
	IDPath        string `json:"id_path,omitempty"`        // session_mint：新会话 ID 的响应路径（缺省递归取首个 UUID）
	Echo          bool   `json:"echo,omitempty"`           // 把该槽位实测值回显到启动响应（只给非凭据槽位，供两态验证与清账）
	TimeoutMs     int64  `json:"timeout_ms,omitempty"`     // 该生产者自身的等待上限（pow 解题 30s）
	// 以下字段服务 fetch_field（派发前的只读取数，如千问 session/get 取 topic_id）。
	Method      string            `json:"method,omitempty"`       // fetch_field：请求方法（默认 POST）
	Body        string            `json:"body,omitempty"`         // fetch_field：请求体文本模板（可含 {{slot}}）
	ContentType string            `json:"content_type,omitempty"` // fetch_field：请求体类型（默认 application/json）
	Headers     map[string]string `json:"headers,omitempty"`      // fetch_field：覆盖默认头（只读取数常需最小头集，避免跨域预检被拒）
	// Select 是 fetch_field 的内建取值器（取不到时按 optional/fallback 处理；与 Path 二选一）：
	//   "last_assistant_message_id" —— 会话对象里最后一条助手消息 id（ChatGPT 续轮 parent_message_id）
	//   "last_assistant_text"       —— 会话对象里最后一条助手消息正文（ChatGPT 答案正文轮询）
	Select string `json:"select,omitempty"`
}

// ResponseSpec 定义响应分帧与增量提取规格。
type ResponseSpec struct {
	Kind      string `json:"kind"`                 // "ndjson_wrb_fr", "sse_data_json", "json", "sse_handoff_poll"
	Frames    string `json:"frames,omitempty"`     // "verified", "unverified"
	DeltaPath string `json:"delta_path,omitempty"` // JSONPath 或字段名
	// Poll 只在 kind="sse_handoff_poll" 时使用（契约 §13，2026-10-02 ChatGPT 实测）：
	// 主请求的 SSE 只给「前奏」（到 stream_handoff 为止，**正文不在其中**），答案正文改由站点
	// WebSocket conduit 投递。直发不订阅该 WS，而是按本规格轮询站点会话对象补正文。
	Poll *PollSpec `json:"poll,omitempty"`
}

// ResponseKindSSEHandoffPoll 是「SSE 前奏 + 轮询会话对象取正文」的响应分帧类型。
const ResponseKindSSEHandoffPoll = "sse_handoff_poll"

// PollSpec 声明「主请求读到哪里、再去哪里把正文读回来」。
type PollSpec struct {
	URLTemplate      string `json:"url_template"`                 // 轮询地址模板，支持 {{conversation_id}}
	ConversationIDRe string `json:"conversation_id_re,omitempty"` // 从主流原始文本抽会话 ID 的正则（第 1 组）
	Select           string `json:"select,omitempty"`             // 取值器（默认 last_assistant_text）
	IntervalMs       int    `json:"interval_ms,omitempty"`        // 轮询间隔（默认 1500ms）
	TimeoutMs        int    `json:"timeout_ms,omitempty"`         // 轮询上限（默认 60000ms）
}

// DriftSpec 定义漂移探测与自愈触发条件。
type DriftSpec struct {
	Probe    string   `json:"probe"`              // "recipe_probe"
	Triggers []string `json:"triggers,omitempty"` // "on_failure", "scheduled"
}

// Validate 校验 Recipe 的完整性与合规性。
func (r *Recipe) Validate() error {
	if strings.TrimSpace(r.Site) == "" {
		return errors.New("recipe: site 不能为空")
	}
	if strings.TrimSpace(r.Request.Method) == "" {
		return errors.New("recipe: request.method 不能为空")
	}
	if strings.TrimSpace(r.Request.URLTemplate) == "" {
		return errors.New("recipe: request.url_template 不能为空")
	}
	if strings.TrimSpace(r.Response.Kind) == "" {
		return errors.New("recipe: response.kind 不能为空")
	}
	if r.Request.Signer != nil && strings.TrimSpace(r.Request.Signer.Path) == "" {
		return errors.New("recipe: request.signer 指定了签名器但缺少 path（页内签名函数路径）")
	}
	if r.Response.Kind == ResponseKindSSEHandoffPoll {
		if r.Response.Poll == nil || strings.TrimSpace(r.Response.Poll.URLTemplate) == "" {
			return errors.New("recipe: response.kind=sse_handoff_poll 必须声明 poll.url_template（正文去哪里读）")
		}
	}

	// 检查槽位合规性：严禁明文凭据伪装成 static 混入
	for _, slot := range r.Slots {
		if strings.TrimSpace(slot.Name) == "" {
			return errors.New("recipe: 槽位名不能为空")
		}
		switch slot.Producer {
		case ProducerPageGlobal:
			if strings.TrimSpace(slot.Path) == "" {
				return fmt.Errorf("recipe: 槽位 %q 指定了 page_global 但缺少 path", slot.Name)
			}
		case ProducerFromURL:
			if strings.TrimSpace(slot.Re) == "" {
				return fmt.Errorf("recipe: 槽位 %q 指定了 from_url 但缺少 re 正则", slot.Name)
			}
		case ProducerCounter, ProducerFreshUUID, ProducerFreshHex, ProducerFreshBlob,
			ProducerCookie, ProducerStatic, ProducerPrompt, ProducerModel,
			ProducerLocalStorage, ProducerNowEpoch:
			// 合法
		case ProducerPowChallenge:
			if strings.TrimSpace(slot.Endpoint) == "" {
				return fmt.Errorf("recipe: 槽位 %q 指定了 pow_challenge 但缺少 endpoint（取挑战的接口）", slot.Name)
			}
			if strings.TrimSpace(slot.Chunk) == "" {
				return fmt.Errorf("recipe: 槽位 %q 指定了 pow_challenge 但缺少 chunk（站点 worker chunk URL）", slot.Name)
			}
		case ProducerSessionMint:
			if strings.TrimSpace(slot.Endpoint) == "" {
				return fmt.Errorf("recipe: 槽位 %q 指定了 session_mint 但缺少 endpoint（建会话接口）", slot.Name)
			}
		case ProducerFetchField:
			if strings.TrimSpace(slot.Endpoint) == "" {
				return fmt.Errorf("recipe: 槽位 %q 指定了 fetch_field 但缺少 endpoint（只读取数接口）", slot.Name)
			}
			if strings.TrimSpace(slot.Path) == "" && strings.TrimSpace(slot.Select) == "" {
				return fmt.Errorf("recipe: 槽位 %q 指定了 fetch_field 但缺少 path／select（取值路径）", slot.Name)
			}
			if sel := strings.TrimSpace(slot.Select); sel != "" && sel != SelectLastAssistantMessageID && sel != SelectLastAssistantText {
				return fmt.Errorf("recipe: 槽位 %q 的 select %q 不是内建取值器", slot.Name, sel)
			}
		default:
			return fmt.Errorf("recipe: 槽位 %q 拥有未知的 producer 类型 %q", slot.Name, slot.Producer)
		}
	}
	return nil
}

// FromJSON 从 JSON 字节反序列化并校验 Recipe。
func FromJSON(data []byte) (*Recipe, error) {
	var r Recipe
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("recipe: JSON 解析失败: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.Status == "" {
		r.Status = StatusDraft
	}
	return &r, nil
}

// ToJSON 将 Recipe 序列化为规范的格式化 JSON。
func (r *Recipe) ToJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(r, "", "  ")
}

// SessionEchoSlotNames 返回该 recipe 中「站点会话 ID 槽位」的名字（契约 §9 取值来源③）。
// 判据＝`producer: session_mint`（该生产者的语义就是「本站点会话 ID 从哪来」）
// 且 `echo: true`（页内执行器才会把本轮实际使用的值随启动响应回显）。
func (r *Recipe) SessionEchoSlotNames() []string {
	if r == nil {
		return nil
	}
	var names []string
	for _, s := range r.Slots {
		if s.Echo && strings.TrimSpace(s.Producer) == "session_mint" && strings.TrimSpace(s.Name) != "" {
			names = append(names, s.Name)
		}
	}
	return names
}
