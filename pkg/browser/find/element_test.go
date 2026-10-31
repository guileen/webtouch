package find

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakePointEval struct {
	expr string
	raw  json.RawMessage
}

func (f *fakePointEval) EvalRaw(_ context.Context, expr string) (json.RawMessage, error) {
	f.expr = expr
	return f.raw, nil
}

// 坐标必须代入表达式（同一拍解析受体 ＋ 取文本），命中身份与文本原样返回。
func TestElementAtPointParsesHit(t *testing.T) {
	fe := &fakePointEval{raw: json.RawMessage(`{"sel":"div#app > button:nth-of-type(2)","tag":"button","text":"立即购买"}`)}
	p, err := ElementAtPoint(context.Background(), fe, 137, 42)
	if err != nil {
		t.Fatalf("ElementAtPoint: %v", err)
	}
	if !strings.Contains(fe.expr, "document.elementFromPoint(137, 42)") {
		t.Fatalf("求值表达式未按坐标代入: %s", fe.expr)
	}
	if p.Selector != "div#app > button:nth-of-type(2)" || p.Tag != "button" || p.Text != "立即购买" {
		t.Fatalf("HitPoint = %+v", p)
	}
}

// 落点文本按 rune 截断 ≤120（不按字节/UTF-16 码元切）。
func TestElementAtPointTruncatesTextByRune(t *testing.T) {
	long := strings.Repeat("张", 200)
	raw, _ := json.Marshal(HitPoint{Selector: "div#x", Tag: "div", Text: long})
	p, err := ElementAtPoint(context.Background(), &fakePointEval{raw: raw}, 1, 2)
	if err != nil {
		t.Fatalf("ElementAtPoint: %v", err)
	}
	if got := len([]rune(p.Text)); got != hitTextLimit {
		t.Fatalf("截断后 rune 数 = %d，want %d", got, hitTextLimit)
	}
}

// 无 value / null ⇒ errEvalEmpty（宿主 ClickAt 据此把观测串写成 click_at:…）；
// 「无命中元素」是正常结果（零值 ＋ nil），由调用方按空选择器走 no_element。
func TestElementAtPointEmptyValueAndNoHit(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null")} {
		if _, err := ElementAtPoint(context.Background(), &fakePointEval{raw: raw}, 1, 2); err == nil {
			t.Fatalf("raw=%q 应报 errEvalEmpty", string(raw))
		}
	}
	p, err := ElementAtPoint(context.Background(), &fakePointEval{raw: json.RawMessage(`{"sel":"","tag":"","text":""}`)}, 1, 2)
	if err != nil || p.Selector != "" || p.Text != "" {
		t.Fatalf("无命中元素应返回零值 ＋ nil，got %+v err=%v", p, err)
	}
}
