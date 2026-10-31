// Package vlm 另含 UWS「义眼」对话客户端（EyeClient）。
//
// 与「义肢」Resolve 的分工（2026-08-28 定案）：
//   - 义肢（Resolve）：VLM 产「候选坐标/selector」供引擎做确定性执行（点击/提取）。
//   - 义眼（Ask）：VLM 直接对图 / 图对图 / 站点认知做「体验评审」，返回给 Agent 消费的
//     结构化判断（是否一致、是否满足需求、按钮位置、手势是否成功等）。它不产可执行
//     动作，尤其不做 agent（不规划多步、不自主执行），只做好「看一看 + 给反馈」这一件事。
//
// 设计动机（用户 2026-08-28 明确）：复刻/站点评审这类需求，不需要一整套「测量/规格/diff」
// 原语；只需一条「把图（或两个站点）交给视觉模型，让模型作为有一定认知的体验评审者给反馈」
// 的通路。多图/多站点对比在客户端自由编排，模型按结构化 schema 返回结论。
package vlm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EyeImage 是一次义眼观察的图片源：本地文件字节（Data）或远程图片 URL（URL），二选一。
// Label 用于在 prompt 中标识图片（如「参考站」「我们的实现」），默认自动编号「图N」。
type EyeImage struct {
	Label string
	Data  []byte
	URL   string
}

// EyeRequest 是义眼一次观察的上下文：开放问题 + 一张或多张图。
type EyeRequest struct {
	Question string     // 自然语言问题/任务（如「判断两个实现是否一致」）
	Images   []EyeImage // 可多张：单图评审 / 图对图对比 / 站点对比
	Detail   string     // 图片 detail 档位（low|high|original；默认 low，成本低）
	// JSON 输出 schema 固定为 EyeResult（verdict/summary/checks/confidence）；若为 false
	// 只返回模型的自由文本回答。默认为 true（给 Agent 用，结构比散文更可消费）。
	JSON bool
	// System 覆盖默认系统提示（如主控循环的动态 Capabilities 描述）；为空用默认。
	System string
}

// EyeCheck 是一次结构化发现：某个关注点的判定。
type EyeCheck struct {
	Name   string `json:"name"`   // 关注点（如「按钮位置」「卡片比例」「手势反馈」）
	Status string `json:"status"` // 建议用 pass|warn|fail|info
	Note   string `json:"note"`   // 一句话结论
}

// EyeResult 是义眼的结构化结论（Agent 可直接读 verdict/summary/checks 做决策）。
type EyeResult struct {
	Verdict    string     `json:"verdict"`    // 简短判定：如 consistent/different/pass/fail/unknown
	Summary    string     `json:"summary"`    // 一句话总评
	Checks     []EyeCheck `json:"checks"`     // 逐关注点结论
	Confidence float64    `json:"confidence"` // 0~1 置信度
	Raw        string     `json:"-"`          // 模型原始文本（内含于 Answer，不重复暴露）
}

// eyeSystemPrompt 是义眼的角色与输出约束；json=true 时要求结构化 EyeResult，
// json=false（--plain）时强约束「只用中文自然语言、禁止输出 JSON / markdown 围栏」。
func eyeSystemPrompt(json bool) string {
	base := eyeRoleBase()
	if json {
		return base + `
当需要结构化结论时，只输出一个 JSON 对象（不要 markdown 围栏、不要推理过程），schema 如下：
{
  "verdict": "<简短判定：consistent|different|pass|fail|unknown>",
  "summary": "<一句话总评>",
  "checks": [{"name":"<关注点>","status":"pass|warn|fail|info","note":"<一句话结论>"}],
  "confidence": 0.0
}`
	}
	return base + `
这次是自然语言评审：请用**简明中文**直接回答，不要输出 JSON、不要用 markdown 代码围栏、不要解释你在思考。`
}

// eyeRoleBase 是义眼公共角色定位（单轮评审与主控循环共用）。
func eyeRoleBase() string {
	return `你是 UWS 的「义眼」：一个熟悉 UI/UX、浏览器体验、前端还原度的评审者。
你的职责是观察图片/站点并给出清晰、可执行的认知反馈（一致性、是否满足需求、交互是否成功、视觉/布局/文案差异）。
你不规划多步动作、不执行浏览器操作，只做「看得准 + 说得清」。`
}

// EyeControlSystemPrompt 是义眼「主控循环」的系统提示：VLM 可自决调用证据原语（多轮），
// 但被强约束为优先单轮收尾（conclude），避免为确认而空转变慢。
func EyeControlSystemPrompt(capabilities string) string {
	return eyeRoleBase() + `
你可以调用 UWS 的内置图像证据原语来补充判断依据（这正是 VLM 调用 UWS 能力，而非裸眼看图猜）。
可用的证据原语：
` + capabilities + `
决策规则（每轮只输出一个 JSON 对象，不要 markdown 围栏、不要思考过程）：
- 优先**单轮收尾**：只要现有图片 + 证据足以判断，就直接 action=conclude 并给出 verdict/summary/checks/confidence。
- 只有当**现有证据确不足**时才请求一个证据原语（每轮最多一个；diff 需要前两张字节图，zoom/region_color 作用于第一张图）。
- 不要为了确认而空转；连续请求只在确有需要（如 diff 后需放大细节）时进行。
JSON schema：
{
  "action": "conclude|zoom|region_color|diff",
  "reason": "<一句话：为什么收尾 / 为什么要这个证据>",
  "verdict": "<consistent|different|pass|fail|unknown>",
  "summary": "<一句话总评>",
  "checks": [{"name":"<关注点>","status":"pass|warn|fail|info","note":"<一句话结论>"}],
  "confidence": 0.0,
  "x": 0, "y": 0, "w": 0, "h": 0, "scale": 1
}`
}

// EyeClient 是义眼对话客户端；复用 DeepSeek 视觉通道（同 vlm.Config 约定）。
type EyeClient struct {
	cfg Config
	hc  *http.Client
}

// NewEye 构造义眼客户端；缺省字段走内置默认（与 NewDeepSeek 一致）。
func NewEye(cfg Config) *EyeClient {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &EyeClient{cfg: cfg, hc: &http.Client{Timeout: cfg.Timeout}}
}

// Ask 把问题 + 若干图交给视觉模型，返回模型输出文本。
// 图片以 base64 内联（本地 Data）或原样 image_url（远程 URL）注入 content 数组；
// 适配多图（单图 / 图对图 / 多站点）。
func (c *EyeClient) Ask(ctx context.Context, req EyeRequest) (string, error) {
	if strings.TrimSpace(req.Question) == "" {
		return "", fmt.Errorf("eye: question 不能为空")
	}
	if len(req.Images) == 0 {
		return "", fmt.Errorf("eye: 至少需要一张图片（--img 本地/URL，或 --url 站点）")
	}
	detail := req.Detail
	if detail == "" {
		detail = "low"
	}
	content, err := icoContent(req, detail)
	if err != nil {
		return "", err
	}
	sys := req.System
	if sys == "" {
		sys = eyeSystemPrompt(req.JSON)
	}
	messages := []map[string]any{
		{"role": "system", "content": sys},
		{"role": "user", "content": content},
	}
	payload := map[string]any{
		"model":       c.cfg.Model,
		"temperature": 0,
		// 2026-08-27 实测：关闭 thinking 是「看→判断」的真正开关（reasoning 归零、单次 ~1.3s）。
		"thinking": map[string]any{"type": "disabled"},
		// 结构化评审/对比需要一定输出空间；但防夹带思考尾巴撑爆 JSON，仍设合理上限。
		"max_tokens": 1000,
		"messages":   messages,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("eye: encode: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("eye: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("eye: http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("eye: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("eye: http %d: %s", resp.StatusCode, trunc(string(raw), 240))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("eye: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("eye: empty choices")
	}
	return out.Choices[0].Message.Content, nil
}

// EyeTurn 是义眼主控循环的单轮决策：action=conclude 时携带 EyeResult；否则携带证据原语参数。
type EyeTurn struct {
	Action string `json:"action"`           // conclude|zoom|region_color|diff
	Reason string `json:"reason,omitempty"` // 为什么
	X      int    `json:"x,omitempty"`      // zoom/region_color：区域起点 x
	Y      int    `json:"y,omitempty"`      // zoom/region_color：区域起点 y
	W      int    `json:"w,omitempty"`      // zoom/region_color：宽
	H      int    `json:"h,omitempty"`      // zoom/region_color：高
	Scale  int    `json:"scale,omitempty"`  // zoom：放大倍数（默认 1）
	EyeResult
}

// ParseEyeTurn 从模型输出中提取主控单轮决策（容忍 markdown 围栏与前后杂音/裸换行）。
// 解析失败返回 ErrEyeNoJSON，调用方应降级为安全收尾。
func ParseEyeTurn(raw string) (EyeTurn, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	a, b := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if a < 0 || b <= a {
		return EyeTurn{}, fmt.Errorf("eye: no json object")
	}
	var t EyeTurn
	sub := string(s[a : b+1])
	if err := json.Unmarshal([]byte(sub), &t); err != nil {
		if err2 := json.Unmarshal([]byte(sanitizeJSONNewlines(sub)), &t); err2 != nil {
			return EyeTurn{}, fmt.Errorf("eye: parse turn: %w", err)
		}
	}
	t.Raw = raw
	return t, nil
}

// icoContent 组装 user 消息 content 数组：文本（问题 + 图片标注 + schema 约束）+ 若干图片。
func icoContent(req EyeRequest, detail string) ([]map[string]any, error) {
	var b strings.Builder
	b.WriteString("任务/问题：")
	b.WriteString(req.Question)
	b.WriteString("\n\n待观察的图片：")
	for i, img := range req.Images {
		label := img.Label
		if label == "" {
			label = fmt.Sprintf("图%d", i+1)
		}
		fmt.Fprintf(&b, "\n- %s", label)
	}
	if req.JSON {
		b.WriteString("\n\n请按结构输出 JSON（verdict/summary/checks/confidence），对照逐张图/站点给出明确判定。")
	} else {
		b.WriteString("\n\n请用简明中文直接回答，**不要输出 JSON、不要用 markdown 代码围栏**。")
	}
	text := b.String()
	content := []map[string]any{{"type": "text", "text": text}}
	for _, img := range req.Images {
		url, err := imageSource(img)
		if err != nil {
			return nil, err
		}
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url":    url,
				"detail": detail,
			},
		})
	}
	return content, nil
}

// imageSource 把图片源归一化为可直接作为 image_url.url 的字符串：
// 本地 Data 内联为 data:image/png;base64,<...>，远程 URL 原样返回。
func imageSource(img EyeImage) (string, error) {
	if len(img.Data) > 0 {
		mime := "image/png"
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data), nil
	}
	if img.URL != "" {
		return img.URL, nil
	}
	return "", fmt.Errorf("eye: 图片源为空（Data 与 URL 均未提供）")
}

// ParseEyeContent 从模型输出中提取结构化 EyeResult（容忍 markdown 围栏与前后杂音）。
// 解析失败返回 ErrEyeNoJSON，调用方应降级为自由文本。
func ParseEyeContent(raw string) (EyeResult, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	a, b := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if a < 0 || b <= a {
		return EyeResult{}, fmt.Errorf("eye: no json object")
	}
	var r EyeResult
	sub := string(s[a : b+1])
	if err := json.Unmarshal([]byte(sub), &r); err != nil {
		if err2 := json.Unmarshal([]byte(sanitizeJSONNewlines(sub)), &r); err2 != nil {
			return EyeResult{}, fmt.Errorf("eye: parse: %w", err)
		}
	}
	r.Raw = raw
	return r, nil
}

// DefaultEyeTimeout 是义眼默认超时（评审/对比可能比单点定位略慢，稍宽裕）。
const DefaultEyeTimeout = 20 * time.Second
