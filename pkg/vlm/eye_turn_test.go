package vlm

import "testing"

func TestParseEyeTurnConclude(t *testing.T) {
	raw := "好的，我来判断。结果如下：\n```json\n{\"action\":\"conclude\",\"reason\":\"两站布局差异明显\",\"verdict\":\"different\",\"summary\":\"布局完全不同\",\"checks\":[{\"name\":\"布局\",\"status\":\"fail\",\"note\":\"差异大\"}],\"confidence\":0.9}\n```"
	turn, err := ParseEyeTurn(raw)
	if err != nil {
		t.Fatalf("ParseEyeTurn: %v", err)
	}
	if turn.Action != "conclude" {
		t.Fatalf("action = %q, want conclude", turn.Action)
	}
	if turn.Verdict != "different" {
		t.Fatalf("verdict = %q, want different", turn.Verdict)
	}
	if turn.Confidence != 0.9 {
		t.Fatalf("confidence = %v", turn.Confidence)
	}
	if len(turn.Checks) != 1 || turn.Checks[0].Name != "布局" {
		t.Fatalf("checks = %+v", turn.Checks)
	}
}

func TestParseEyeTurnZoom(t *testing.T) {
	raw := `{"action":"zoom","reason":"要看按钮圆角","x":120,"y":80,"w":200,"h":120,"scale":4}`
	turn, err := ParseEyeTurn(raw)
	if err != nil {
		t.Fatalf("ParseEyeTurn: %v", err)
	}
	if turn.Action != "zoom" {
		t.Fatalf("action = %q, want zoom", turn.Action)
	}
	if turn.X != 120 || turn.Y != 80 || turn.W != 200 || turn.H != 120 || turn.Scale != 4 {
		t.Fatalf("zoom params = %+v", turn)
	}
}

func TestParseEyeTurnGarbage(t *testing.T) {
	if _, err := ParseEyeTurn("我没看懂这张图"); err == nil {
		t.Fatal("want error for non-JSON turn")
	}
}
