package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uws/apps/gateway/recipe"
)

// These tests exercise the replay path with a fake page evaluator. Explicitly
// activate only the fixture under test; embedded recipes remain DRAFT.
func activateRecipeFixture(t *testing.T, site string) {
	t.Helper()
	r, ok := recipe.DefaultRegistry().Get(site)
	if !ok {
		t.Fatalf("missing recipe fixture %q", site)
	}
	previous := r.Status
	r.Status = recipe.StatusActive
	t.Cleanup(func() { r.Status = previous })
}

func TestBuildDirectTransportRequest_DraftRecipeNotDispatched(t *testing.T) {
	r, ok := recipe.DefaultRegistry().Get("claude")
	if !ok || r.Status != recipe.StatusDraft {
		t.Fatalf("expected embedded claude recipe to remain DRAFT, got recipe=%+v", r)
	}
	if CanDirectTransport("claude") {
		t.Fatal("DRAFT recipe must not be reported as direct-transport capable")
	}
	if req, ok := BuildDirectTransportRequest("claude", "test prompt", "claude-sonnet"); ok || req.Recipe != nil {
		t.Fatalf("DRAFT recipe must not enter the direct transport path: ok=%v req=%+v", ok, req)
	}
}

// TestDirectChat_GeminiRecipeFirstTurn 验证 Gemini 首轮（无会话 ID）直发与 NDJSON 流解码闭环。
func TestDirectChat_GeminiRecipeFirstTurn(t *testing.T) {
	activateRecipeFixture(t, "gemini")
	e := newFakeEngine()
	ndjsonStream := `)]}'

177
[["wrb.fr",null,"[null,[\"c_x\",\"r_y\"],null,null,[[\"rc_abc\",[\"Gemini direct answer\"]]]]"]]
26
[["e",9,null,null,4839]]
`
	pollCount := 0
	e.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		if strings.Contains(req.Value, "window.fetch") || strings.Contains(req.Value, "StreamGenerate") {
			// 验证生成的页面脚本中包含核心安全头与槽位求值逻辑
			if !strings.Contains(req.Value, "WIZ_global_data.SNlM0e") {
				return ActionResult{Success: false}, fmt.Errorf("missing SNlM0e slot evaluation")
			}
			return ActionResult{Success: true, ExtractedData: `{"status":"streaming_started","ok":true}`}, nil
		}
		if strings.Contains(req.Value, "chunks") {
			pollCount++
			if pollCount == 1 {
				return ActionResult{Success: true, ExtractedData: fmt.Sprintf(`{"chunks":[%q],"done":false}`, ndjsonStream)}, nil
			}
			return ActionResult{Success: true, ExtractedData: `{"chunks":[],"done":true}`}, nil
		}
		return ActionResult{Success: true, ExtractedData: `{}`}, nil
	}

	g := newTestGateway(t, e)
	req := chatReq("gemini-web", "gemini-flash", Message{Role: "user", Content: "2+3=?"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := g.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Gemini 直发失败: %v", err)
	}

	if len(resp.Choices) == 0 {
		t.Fatal("未收到 Choice 响应")
	}
	got := resp.Choices[0].Message.Content
	if got != "Gemini direct answer" {
		t.Fatalf("NDJSON 解码文本不匹配: got %q, want %q", got, "Gemini direct answer")
	}
}

// TestDirectChat_DeepSeekRecipeSSE 验证 DeepSeek Web 端直发与 SSE 流式抽回。
func TestDirectChat_DeepSeekRecipeSSE(t *testing.T) {
	activateRecipeFixture(t, "deepseek")
	e := newFakeEngine()
	sseChunk1 := "data: {\"choices\":[{\"delta\":{\"content\":\"DeepSeek \"}}]}\n\n"
	sseChunk2 := "data: {\"choices\":[{\"delta\":{\"content\":\"online direct\"}}]}\n\n"
	sseDone := "data: [DONE]\n\n"

	pollStep := 0
	e.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		if strings.Contains(req.Value, "window.fetch") {
			return ActionResult{Success: true, ExtractedData: `{"status":"streaming_started","ok":true}`}, nil
		}
		if strings.Contains(req.Value, "chunks") {
			pollStep++
			switch pollStep {
			case 1:
				return ActionResult{Success: true, ExtractedData: fmt.Sprintf(`{"chunks":[%q],"done":false}`, sseChunk1)}, nil
			case 2:
				return ActionResult{Success: true, ExtractedData: fmt.Sprintf(`{"chunks":[%q],"done":false}`, sseChunk2)}, nil
			default:
				return ActionResult{Success: true, ExtractedData: fmt.Sprintf(`{"chunks":[%q],"done":true}`, sseDone)}, nil
			}
		}
		return ActionResult{Success: true, ExtractedData: `{}`}, nil
	}

	g := newTestGateway(t, e)
	req := chatReq("deepseek-web", "deepseek-fast", Message{Role: "user", Content: "ping"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := g.ChatStream(ctx, req)
	if err != nil {
		t.Fatalf("ChatStream 启动失败: %v", err)
	}

	var accumulated strings.Builder
	for ev := range stream {
		if ev.Err != nil {
			t.Fatalf("流式事件报错: %v", ev.Err)
		}
		accumulated.WriteString(ev.Delta)
	}

	want := "DeepSeek online direct"
	if accumulated.String() != want {
		t.Fatalf("SSE 解码拼接文本不匹配: got %q, want %q", accumulated.String(), want)
	}
}

// TestDirectChat_SlotMissingFailClosedFallbackToDOM 验证双轨自愈底座：
// 当页面态槽位缺失时（如未登录或页面仍未加载完成），fail-closed 毫秒级自动降级至物理 DOM 交互。
func TestDirectChat_SlotMissingFailClosedFallbackToDOM(t *testing.T) {
	activateRecipeFixture(t, "gemini")
	e := newFakeEngine()
	e.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		if strings.Contains(req.Value, "window.fetch") {
			// 模拟页面尚未就绪，WIZ_global_data 槽位缺失，未派发请求
			return ActionResult{Success: true, ExtractedData: `{"status":"not_dispatched","error":"slot_missing: at"}`}, nil
		}
		return ActionResult{Success: true, ExtractedData: `{}`}, nil
	}

	// 物理层执行 mock：DOM 输入与发送点击成功，抽取返回 DOM fallback answer
	e.extractPlan = []extractStep{
		{text: "DOM fallback answer", done: true},
	}

	g := newTestGateway(t, e)
	req := chatReq("gemini-web", "gemini-flash", Message{Role: "user", Content: "降级测试"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := g.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Chat 调用失败: %v", err)
	}

	// 验证降级成功完成了物理输入与点击
	calls := e.callKinds()
	hasInput := false
	hasClick := false
	for _, c := range calls {
		if c.action == "input" {
			hasInput = true
		}
		if c.action == "click" {
			hasClick = true
		}
	}

	if !hasInput || !hasClick {
		t.Fatalf("未自动降级到物理 DOM 交互: calls=%+v", calls)
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "DOM fallback answer" {
		t.Fatalf("未取得物理降级提取结果: %+v", resp)
	}
}

// TestDirectChat_GeminiRecipeMultiTurn 验证 Gemini 多轮对话会话 ID（location.pathname）动态求值与回填。
func TestDirectChat_GeminiRecipeMultiTurn(t *testing.T) {
	activateRecipeFixture(t, "gemini")
	e := newFakeEngine()
	ndjsonStream := `)]}'

177
[["wrb.fr",null,"[null,[\"c_25096c6ba2166662\",\"r_y\"],null,null,[[\"rc_abc\",[\"Gemini turn 2 answer\"]]]]"]]
26
[["e",9,null,null,4839]]
`
	pollCount := 0
	e.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		if strings.Contains(req.Value, "window.fetch") || strings.Contains(req.Value, "StreamGenerate") {
			// 验证脚本中正确提取了 conv 槽位
			if !strings.Contains(req.Value, "location.pathname") {
				return ActionResult{Success: false}, fmt.Errorf("missing location.pathname evaluation")
			}
			return ActionResult{Success: true, ExtractedData: `{"status":"streaming_started","ok":true}`}, nil
		}
		if strings.Contains(req.Value, "chunks") {
			pollCount++
			if pollCount == 1 {
				return ActionResult{Success: true, ExtractedData: fmt.Sprintf(`{"chunks":[%q],"done":false}`, ndjsonStream)}, nil
			}
			return ActionResult{Success: true, ExtractedData: `{"chunks":[],"done":true}`}, nil
		}
		return ActionResult{Success: true, ExtractedData: `{}`}, nil
	}

	g := newTestGateway(t, e)
	req := chatReq("gemini-web", "gemini-flash", Message{Role: "user", Content: "继续分析"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := g.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Gemini 多轮直发失败: %v", err)
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "Gemini turn 2 answer" {
		t.Fatalf("多轮 NDJSON 结果不符: %+v", resp)
	}
}

// TestDirectChat_AI_Search_Perplexity 验证 AI 搜索服务（Perplexity 搜索与问答摘要流）直发闭环。
func TestDirectChat_AI_Search_Perplexity(t *testing.T) {
	activateRecipeFixture(t, "perplexity")
	e := newFakeEngine()
	sseChunk1 := "data: {\"choices\":[{\"delta\":{\"content\":\"搜索结果摘要：WebTouch 统一闭环已建立。\"}}]}\n\n"
	sseDone := "data: [DONE]\n\n"

	pollStep := 0
	e.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
		if strings.Contains(req.Value, "__uws_auth_probe") {
			return ActionResult{Success: true, ExtractedData: `{"status":"logged_in"}`}, nil
		}
		if strings.Contains(req.Value, "window.fetch") {
			return ActionResult{Success: true, ExtractedData: `{"status":"streaming_started","ok":true}`}, nil
		}
		if strings.Contains(req.Value, "chunks") {
			pollStep++
			if pollStep == 1 {
				return ActionResult{Success: true, ExtractedData: fmt.Sprintf(`{"chunks":[%q],"done":false}`, sseChunk1)}, nil
			}
			return ActionResult{Success: true, ExtractedData: fmt.Sprintf(`{"chunks":[%q],"done":true}`, sseDone)}, nil
		}
		return ActionResult{Success: true, ExtractedData: `{}`}, nil
	}

	cfg := DefaultConfig("127.0.0.1:50051")
	cfg.Channels = append(cfg.Channels, ChannelConfig{
		ID:         "perplexity-web",
		Type:       ChannelTypeWeb,
		Site:       "perplexity",
		EngineAddr: "127.0.0.1:50051",
		PageURL:    "https://www.perplexity.ai/",
		Models: []ModelConfig{
			{ID: "perplexity-copilot", Family: "Search", Label: "Perplexity Copilot", Aliases: []string{"auto", "perplexity"}},
		},
	})
	if err := cfg.Normalize("127.0.0.1:50051"); err != nil {
		t.Fatal(err)
	}
	stable := time.Duration(0)
	g, err := NewGateway(&cfg, Options{
		EngineFactory: func(string) (EngineClient, error) { return e, nil },
		AnswerStable:  &stable,
		Sleep:         func(time.Duration) {},
		AuthStatePath: filepath.Join(t.TempDir(), "gateway-auth-state.json"),
	})
	if err != nil {
		t.Fatal(err)
	}

	req := chatReq("perplexity-web", "perplexity-copilot", Message{Role: "user", Content: "AI search query"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := g.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Perplexity AI 搜索直发失败: %v", err)
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "搜索结果摘要：WebTouch 统一闭环已建立。" {
		t.Fatalf("Perplexity 搜索流式解码不匹配: %+v", resp)
	}
}
