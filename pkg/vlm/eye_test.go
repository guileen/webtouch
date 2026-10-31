package vlm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestParseEyeContent 锁定结构化 JSON 解析：容忍 markdown 围栏与前后杂音。
func TestParseEyeContent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", `{"verdict":"consistent","summary":"接近","checks":[{"name":"按钮位置","status":"pass","note":"一致"}],"confidence":0.9}`, "consistent"},
		{"fence", "```json\n{\"verdict\":\"different\",\"summary\":\"有差异\",\"checks\":[],\"confidence\":0.8}\n```", "different"},
		{"prose", "我的判断：\n{\"verdict\":\"pass\",\"summary\":\"满足需求\",\"checks\":[{\"name\":\"卡片比例\",\"status\":\"warn\",\"note\":\"略大\"}],\"confidence\":0.7}\n以上。", "pass"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseEyeContent(c.raw)
			if err != nil {
				t.Fatalf("ParseEyeContent: %v", err)
			}
			if got.Verdict != c.want {
				t.Fatalf("verdict = %q, want %q", got.Verdict, c.want)
			}
		})
	}
}

func TestParseEyeContentNoJSON(t *testing.T) {
	if _, err := ParseEyeContent("模型说：看起来还行"); err == nil {
		t.Fatal("want error for non-json")
	}
}

// TestEyeAsk 锁定义眼请求：多图注入、系统角色、关闭 thinking、schema 约束、max_tokens。
func TestEyeAsk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-eye" {
			t.Errorf("auth = %q", got)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if req["model"] != DefaultModel {
			t.Errorf("model = %v", req["model"])
		}
		thinking, ok := req["thinking"].(map[string]any)
		if !ok || thinking["type"] != "disabled" {
			t.Errorf("thinking.type = %v, want disabled", req["thinking"])
		}
		if mt := req["max_tokens"]; mt != float64(1000) {
			t.Errorf("max_tokens = %v, want 1000", mt)
		}
		msgs := req["messages"].([]any)
		if msgs[0].(map[string]any)["role"] != "system" {
			t.Errorf("msg[0] role = %v, want system", msgs[0].(map[string]any)["role"])
		}
		content := msgs[1].(map[string]any)["content"].([]any)
		// 2 图 + 1 文本 = 3 段
		if len(content) != 3 {
			t.Fatalf("content len = %d, want 3", len(content))
		}
		text := content[0].(map[string]any)["text"].(string)
		if !strings.Contains(text, "判断两个实现是否一致") {
			t.Errorf("提问缺失: %q", text)
		}
		if !strings.Contains(text, "参考站") || !strings.Contains(text, "我们的实现") {
			t.Errorf("图片标注缺失: %q", text)
		}
		if !strings.Contains(text, "verdict/summary/checks/confidence") {
			t.Errorf("schema 约束缺失: %q", text)
		}
		// 图1 = 本地 data 内联，图2 = URL
		img0 := content[1].(map[string]any)["image_url"].(map[string]any)
		if got := img0["url"].(string); !strings.HasPrefix(got, "data:image/png;base64,iVBORw0KGgo") {
			t.Errorf("img0 应内联 base64: %q", got[:40])
		}
		if img0["detail"] != "low" {
			t.Errorf("img0 detail = %v", img0["detail"])
		}
		img1 := content[2].(map[string]any)["image_url"].(map[string]any)
		if got := img1["url"].(string); got != "https://example.com/ref.png" {
			t.Errorf("img1 url = %q", got)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"different\",\"summary\":\"按钮位置不同\",\"checks\":[{\"name\":\"按钮位置\",\"status\":\"fail\",\"note\":\"X 偏移\"}],\"confidence\":0.85}"}}]}`))
	}))
	defer srv.Close()

	c := NewEye(Config{BaseURL: srv.URL, APIKey: "sk-eye", Timeout: 3 * time.Second})
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00}
	got, err := c.Ask(context.Background(), EyeRequest{
		Question: "判断两个实现是否一致",
		Detail:   "low",
		JSON:     true,
		Images: []EyeImage{
			{Label: "参考站", Data: png},
			{Label: "我们的实现", URL: "https://example.com/ref.png"},
		},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r, err := ParseEyeContent(got)
	if err != nil {
		t.Fatalf("ParseEyeContent: %v", err)
	}
	if r.Verdict != "different" || len(r.Checks) != 1 || r.Checks[0].Name != "按钮位置" {
		t.Fatalf("EyeResult = %+v", r)
	}
}

func TestEyeAskEmptyQuestion(t *testing.T) {
	c := NewEye(Config{BaseURL: "http://127.0.0.1:1"})
	if _, err := c.Ask(context.Background(), EyeRequest{Question: "", Images: []EyeImage{{Data: []byte("x")}}}); err == nil {
		t.Fatal("want error for empty question")
	}
}

func TestEyeAskHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()
	c := NewEye(Config{BaseURL: srv.URL, APIKey: "sk", Timeout: time.Second})
	if _, err := c.Ask(context.Background(), EyeRequest{Question: "q", Images: []EyeImage{{Data: []byte("x")}}}); err == nil {
		t.Fatal("want error on 429")
	}
}
