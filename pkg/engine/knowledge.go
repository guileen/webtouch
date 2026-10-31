package engine

import (
	"context"
	"fmt"
	"log"

	"uws/pkg/classifier"
	"uws/pkg/knowledge"
	"uws/pkg/matcher"
)

// DefaultArtifact 是内置初始知识库元信息（真实 SQLite+ONNX 产物由
// pipeline/exporter（M6B）产出后，经 --knowledge-path 接入）。
func DefaultArtifact() *knowledge.Artifact {
	return &knowledge.Artifact{
		Version:           EngineVersion,
		SchemaHash:        "sha256:builtin-schema-v1.5.2",
		ModelHash:         "sha256:builtin-model-v1.5.2",
		ArtifactURI:       "builtin://v1.5.2",
		MinEngineVersion:  "v1.5.0",
		MaxEngineVersion:  "v1.5.x",
		KeyRuleIndexCount: 18,
		OnnxReady:         false,
		MemoryEstimateMB:  64,
		Rules: map[string]string{
			"l1:login":               "ACTIVE",
			"l1:search":              "ACTIVE",
			"l2:submit_btn":          "ACTIVE",
			"l2:username_in":         "ACTIVE",
			"l2:password_in":         "ACTIVE",
			"l1:chat_input":          "ACTIVE",
			"l1:send_button":         "ACTIVE",
			"l1:chat_answer":         "ACTIVE",
			"l1:new_chat_button":     "ACTIVE",
			"l1:chat_sidebar_toggle": "ACTIVE",
			"l1:chat_retry_button":   "ACTIVE",
			"l1:gemini_input":        "ACTIVE",
			"l1:gemini_send":         "ACTIVE",
			"l1:gemini_answer":       "ACTIVE",
			"l1:gemini_new_chat":     "ACTIVE",
			"l1:chatgpt_input":       "ACTIVE",
			"l1:chatgpt_send":        "ACTIVE",
			"l1:chatgpt_answer":      "ACTIVE",
			"l1:chatgpt_new_chat":    "ACTIVE",
		},
		// R1：KB 单一事实源——matcher 由 artifact 全量重建，
		// 内置种子与旧 NewDefaultMatcher 完全对齐（零配置启动行为不变）。
		RulesFull: []knowledge.RuleSnapshot{
			{RuleType: "text_pattern", TextPattern: "登录", SemanticLabel: "登录", Action: "click", Confidence: 0.95, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "search", SemanticLabel: "搜索", Action: "click", Confidence: 0.90, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "username", SemanticLabel: "用户名输入框", Action: "input", Confidence: 0.90, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "password", SemanticLabel: "密码输入框", Action: "input", Confidence: 0.90, Source: "component_gene", Status: "active"},
			// DeepSeek chat 页（2026-08-25 实测 DOM）：输入框 = textarea placeholder
			// 「给 DeepSeek 发送消息」；发送 = div[role=button].ds-button--primary（纯 SVG、
			// 无文本/aria，纯语义匹配永远找不到，必须选择器）；回答区 = .ds-markdown。
			{RuleType: "text_pattern", TextPattern: "chat_input", SemanticLabel: "chat_input", Selector: "textarea[placeholder*='发送消息'], textarea", Action: "input", Confidence: 0.95, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "send_button", SemanticLabel: "send_button", Selector: "div[role='button'].ds-button--primary", Action: "click", Confidence: 0.95, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "chat_answer", SemanticLabel: "chat_answer", Selector: ".ds-markdown", Action: "extract", Confidence: 0.95, Source: "component_gene", Status: "active"},
			// 2026-08-25 新会话修复：DeepSeek「开启新对话」是纯 div（无 role/button/
			// onclick），常规交互元素扫描抓不到，用 text= 前缀按精确文本取最小可见元素；
			// 侧边栏默认折叠时先用顶部第一个图标按钮展开（chat_sidebar_toggle）。
			{RuleType: "text_pattern", TextPattern: "new_chat_button", SemanticLabel: "new_chat_button", Selector: "text=开启新对话", Action: "click", Confidence: 0.95, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "chat_sidebar_toggle", SemanticLabel: "chat_sidebar_toggle", Selector: "div[role='button'][class*='ds-button--iconLabelPrimary']", Action: "click", Confidence: 0.90, Source: "component_gene", Status: "active"},
			// DeepSeek 频率受限/网络波动重试按钮（2026-09-14 实测：连续快速发送出现「请检查网络后重试」警告按钮）
			{RuleType: "text_pattern", TextPattern: "chat_retry_button", SemanticLabel: "chat_retry_button", Selector: ".ds-button--warning, div[role='button'].ds-button--warning", Action: "click", Confidence: 0.95, Source: "component_gene", Status: "active"},
			// Gemini chat 页（2026-08-25 实测 DOM，gemini.google.com）：
			// 输入 = rich-textarea 内 .ql-editor（contenteditable，aria-label 为
			// 「为 Gemini 输入提示」）；发送 = button[aria-label="发送"]（输入后出现）；
			// 回答 = structured-content-container.model-response-text（每轮一个，
			// extract 用 preferLast 取最后一轮）；新对话 = 侧栏 a[aria-label*="发起新对话"]。
			// 模型切换走物理层专用语义 gemini_model_switch（cdpSession 特殊处理，
			// 不在此声明选择器——选择器随菜单动态打开，见 pkg/browser/cdp_session.go）。
			{RuleType: "text_pattern", TextPattern: "gemini_input", SemanticLabel: "gemini_input", Selector: "rich-textarea .ql-editor, .ql-editor[aria-label*='输入提示'], .ql-editor[aria-label*='prompt'], [contenteditable='true'][aria-label*='输入提示'], [contenteditable='true'][aria-label*='prompt']", Action: "input", Confidence: 0.95, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "gemini_send", SemanticLabel: "gemini_send", Selector: "button[aria-label='发送'], button[aria-label='Send message'], button[aria-label='Send'], button[data-test-id='send-button']", Action: "click", Confidence: 0.95, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "gemini_answer", SemanticLabel: "gemini_answer", Selector: "model-response, response-container, structured-content-container.model-response-text, message-content .markdown, .model-response-text", Action: "extract", Confidence: 0.95, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "gemini_new_chat", SemanticLabel: "gemini_new_chat", Selector: "a[aria-label*='发起新对话'], a[aria-label*='New chat']", Action: "click", Confidence: 0.95, Source: "component_gene", Status: "active"},
			// ChatGPT chat 页（chatgpt.com，2026-10-02 真机 DOM 读数）：
			// 输入 = ProseMirror contenteditable（实测 aria-label「询问 ChatGPT」；旧 id
			// #prompt-textarea 在现版本已不再稳定 ⇒ 选择器以 contenteditable 为准，id 只作兜底）；
			// 发送 = button[aria-label="发送"]（**输入非空后才出现**，空输入时页面上没有该按钮，
			// 所以必须先 input 再 click）；回答 = [data-message-author-role="assistant"]
			// （每轮一个，extract 取最后一个）；新对话 = button[aria-label="新聊天"]。
			// 直发 recipe 被 Cloudflare JSD 哨兵挡（prepare 的 proofofwork/turnstile 必须由
			// 站点挑战脚本产出，离线不可铸）⇒ ChatGPT 只走 DOM 通路（站点自己铸 token）。
			{RuleType: "text_pattern", TextPattern: "chatgpt_input", SemanticLabel: "chatgpt_input", Selector: "[contenteditable='true'][aria-label*='ChatGPT'], .ProseMirror[contenteditable='true'], #prompt-textarea", Action: "input", Confidence: 0.95, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "chatgpt_send", SemanticLabel: "chatgpt_send", Selector: "button[aria-label='发送'], button[aria-label='Send'], button[data-testid='send-button'], #composer-submit-button", Action: "click", Confidence: 0.95, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "chatgpt_answer", SemanticLabel: "chatgpt_answer", Selector: "[data-message-author-role='assistant'] .markdown, [data-message-author-role='assistant'], .markdown", Action: "extract", Confidence: 0.95, Source: "component_gene", Status: "active"},
			{RuleType: "text_pattern", TextPattern: "chatgpt_new_chat", SemanticLabel: "chatgpt_new_chat", Selector: "button[aria-label='新聊天'], button[aria-label='New chat'], a[data-testid='create-new-chat-button']", Action: "click", Confidence: 0.95, Source: "component_gene", Status: "active"},
		},
		Synonyms: []knowledge.SynonymSnapshot{
			{SiteDomain: "*", Term: "Sign in", BaseWeight: 0.2, Status: "TENTATIVE"},
		},
		NegativeKeywords: []string{"广告", "弹窗", "banner_zone", "popup"},
	}
}

// memoryFetcher 是 knowledge.Fetcher 的内存实现（零配置启动）。
type memoryFetcher struct {
	art *knowledge.Artifact
}

func (f *memoryFetcher) Fetch(_ context.Context, version, _ string) (*knowledge.Artifact, error) {
	if version != f.art.Version {
		return nil, fmt.Errorf("artifact not found: %s", version)
	}
	cp := *f.art
	cp.Rules = make(map[string]string, len(f.art.Rules))
	for k, v := range f.art.Rules {
		cp.Rules[k] = v
	}
	return &cp, nil
}

// matcherSnapshot 把 knowledge.Artifact 的规则/同义词/黑名单快照映射为
// matcher 输入（两包解耦；映射集中在引擎组合根）。
func matcherSnapshot(art *knowledge.Artifact) ([]matcher.Rule, []matcher.Synonym, []string) {
	rules := make([]matcher.Rule, 0, len(art.RulesFull))
	for _, r := range art.RulesFull {
		rules = append(rules, matcher.Rule{
			RuleType:      r.RuleType,
			TextPattern:   r.TextPattern,
			Selector:      r.Selector,
			Role:          r.Role,
			Action:        r.Action,
			SemanticLabel: r.SemanticLabel,
			Confidence:    r.Confidence,
			Source:        r.Source,
			Status:        r.Status,
			StructHash:    r.StructHash,
		})
	}
	syns := make([]matcher.Synonym, 0, len(art.Synonyms))
	for _, s := range art.Synonyms {
		syns = append(syns, matcher.Synonym{
			SiteDomain:   s.SiteDomain,
			Term:         s.Term,
			BaseWeight:   s.BaseWeight,
			Status:       s.Status,
			HitCount:     s.HitCount,
			SourceRuleID: s.SourceRuleID,
		})
	}
	return rules, syns, art.NegativeKeywords
}

// matcherFromArtifact 从知识库 artifact 全量构建内存 matcher（R1：KB 单一事实源，
// 替换旧的双源种子 NewDefaultMatcher）。
func matcherFromArtifact(art *knowledge.Artifact) (*matcher.Matcher, error) {
	m, err := matcher.New(":memory:")
	if err != nil {
		return nil, err
	}
	if art == nil {
		return m, nil
	}
	rules, syns, kws := matcherSnapshot(art)
	if err := m.ReplaceAll(rules, syns, kws); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

// syncMatcherWithKnowledge 以 KB 当前 active artifact 重建 matcher
// （热更/人工回滚/P0 自动回滚成功后调用；R1 接线）。
func (s *Server) syncMatcherWithKnowledge() error {
	art := s.kb.ActiveArtifact()
	if art == nil {
		return nil
	}
	rules, syns, kws := matcherSnapshot(art)
	return s.matcher.ReplaceAll(rules, syns, kws)
}

// syncScorerWithKnowledge 以 KB 当前 active artifact 重建真实 ONNX scorer
// （热更/人工回滚/P0 自动回滚成功后调用）。模型字节缺失、未配置 ORT 库或
// 构建失败时降级为 nil（决策核继续走确定性 mock，日志可见、不阻塞动作）。
func (s *Server) syncScorerWithKnowledge() {
	if s.onnxLib == "" {
		return
	}
	art := s.kb.ActiveArtifact()
	var next *classifier.OnnxScorer
	if art != nil && len(art.OnnxModel) > 0 {
		sc, err := classifier.NewOnnxScorer(s.onnxLib, art.OnnxModel)
		if err != nil {
			log.Printf("onnx scorer 构建失败（降级 mock 决策核）: %v", err)
		} else {
			next = sc
			log.Printf("onnx scorer 就绪: version=%s model=%dB", art.Version, len(art.OnnxModel))
		}
	}
	s.scorerMu.Lock()
	old := s.onnxScorer
	s.onnxScorer = next
	s.scorerMu.Unlock()
	if old != nil {
		if err := old.Close(); err != nil {
			log.Printf("onnx scorer 旧会话关闭: %v", err)
		}
	}
}
