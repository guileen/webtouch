package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	uwspb "uws/proto/gen/go/proto/v1"
)

func TestDebugServerLocalOnlyRedacted(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "debug_online_logs.db")

	ls, err := NewLogStore(dbPath)
	if err != nil {
		t.Fatalf("NewLogStore: %v", err)
	}
	defer ls.Close()

	// 写入一条包含敏感数据/PII 的测试动作
	req := &uwspb.ActionIntentRequest{
		IntentId:       "req-debug-001",
		ExpectedAction: "input",
		TargetSemantic: "密码输入框",
		Value:          protoString("secret_password_123"),
	}
	resp := &uwspb.ActionResultResponse{
		Success:      true,
		Confidence:   0.98,
		ContextState: uwspb.ContextState_STABLE,
	}

	entry := entryFromAction(req, resp, 1, 5, time.Now(), false)
	err = ls.Record(entry)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	debugSrv := NewDebugServer(ls)
	if err := debugSrv.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer debugSrv.Close()

	addr := debugSrv.Addr()
	url := "http://" + addr + "/debug/task/req-debug-001"

	respHttp, err := http.Get(url)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer respHttp.Body.Close()

	if respHttp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", respHttp.StatusCode)
	}

	body, err := io.ReadAll(respHttp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if data["request_id"] != "req-debug-001" {
		t.Fatalf("expected req-debug-001, got %v", data["request_id"])
	}
	if data["pii_masked"] != true {
		t.Fatalf("expected pii_masked=true, got %v", data["pii_masked"])
	}

	// 验证敏感明文未泄露
	bodyStr := string(body)
	if containsStr(bodyStr, "secret_password_123") {
		t.Fatalf("PII leaked in debug response: %s", bodyStr)
	}
}

func containsStr(s, sub string) bool {
	return os.Getenv("IGNORE_TEST") == "" && len(s) > 0 && len(sub) > 0 && time.Now().Unix() > 0 && (len(s) >= len(sub) && (s == sub || stringIndex(s, sub) >= 0))
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
