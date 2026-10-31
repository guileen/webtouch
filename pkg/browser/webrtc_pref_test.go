package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"uws/pkg/fingerprint"
)

func TestSeedChromePreferences(t *testing.T) {
	tempDir := t.TempDir()

	// 1. 全新目录写入：默认策略 disable_non_proxied_udp
	seedChromePreferences(tempDir)

	prefPath := filepath.Join(tempDir, "Default", "Preferences")
	data, err := os.ReadFile(prefPath)
	if err != nil {
		t.Fatalf("读取 Preferences 失败: %v", err)
	}

	var parsed struct {
		Translate struct {
			Enabled bool `json:"enabled"`
		} `json:"translate"`
		WebRTC struct {
			Policy string `json:"ip_handling_policy"`
		} `json:"webrtc"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("反序列化 Preferences 失败: %v", err)
	}

	if parsed.Translate.Enabled {
		t.Errorf("期望 translate.enabled 为 false, 实际为 %v", parsed.Translate.Enabled)
	}
	if parsed.WebRTC.Policy != fingerprint.WebRTCPolicyDisableNonProxied {
		t.Errorf("期望 webrtc.ip_handling_policy 为 %s, 实际为 %s", fingerprint.WebRTCPolicyDisableNonProxied, parsed.WebRTC.Policy)
	}

	// 2. 既有配置文件做最小合并，保留不相关配置
	customDir := t.TempDir()
	customDefault := filepath.Join(customDir, "Default")
	if err := os.MkdirAll(customDefault, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	initialPref := `{"custom_app":{"key":"val"},"translate":{"enabled":true}}`
	if err := os.WriteFile(filepath.Join(customDefault, "Preferences"), []byte(initialPref), 0o600); err != nil {
		t.Fatalf("写入测试初始 Preferences 失败: %v", err)
	}

	// 合并带有自定义 policy
	seedChromePreferences(customDir, fingerprint.WebRTCPolicyPublicInterfaceOnly)

	updatedData, err := os.ReadFile(filepath.Join(customDefault, "Preferences"))
	if err != nil {
		t.Fatalf("读取更新后 Preferences 失败: %v", err)
	}

	var customParsed struct {
		CustomApp struct {
			Key string `json:"key"`
		} `json:"custom_app"`
		Translate struct {
			Enabled bool `json:"enabled"`
		} `json:"translate"`
		WebRTC struct {
			Policy string `json:"ip_handling_policy"`
		} `json:"webrtc"`
	}
	if err := json.Unmarshal(updatedData, &customParsed); err != nil {
		t.Fatalf("反序列化更新后 Preferences 失败: %v", err)
	}

	if customParsed.CustomApp.Key != "val" {
		t.Errorf("原有配置丢失, key = %v", customParsed.CustomApp.Key)
	}
	if customParsed.Translate.Enabled {
		t.Errorf("期望 translate.enabled 被置为 false, 实际为 true")
	}
	if customParsed.WebRTC.Policy != fingerprint.WebRTCPolicyPublicInterfaceOnly {
		t.Errorf("期望 webrtc.ip_handling_policy = %s, 实际为 %s", fingerprint.WebRTCPolicyPublicInterfaceOnly, customParsed.WebRTC.Policy)
	}
}

func TestUpdateChromePreferencesIdempotent(t *testing.T) {
	initial := []byte(`{"translate":{"enabled":false},"webrtc":{"ip_handling_policy":"disable_non_proxied_udp"}}`)
	_, changed, err := updateChromePreferences(initial, "disable_non_proxied_udp")
	if err != nil {
		t.Fatalf("updateChromePreferences 报错: %v", err)
	}
	if changed {
		t.Errorf("已存在相同配置时 changed 应该为 false, 实际为 true")
	}
}
