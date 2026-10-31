// Package gateway 中的模型提供商沙箱与指纹档案绑定（Profile Binding）。
//
// 架构依据（2026-09-14 战略校准）：
// 杜绝全局共享 ~/.uws-chrome-debug 单一 Profile 导致的账号串扰与风控封禁风险。
// 每个 Web AI 提供商（ChatGPT, Claude, DeepSeek, Gemini）分配独立的沙箱目录与
// 确定性 FingerprintProfile，在启动时通过 seedChromePreferences 焊死 WebRTC，
// 注入硬件伪装与 PRNG 渲染噪声，全力守护个人账号安全。
package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"uws/pkg/fingerprint"
)

// ProviderProfileManager 管理各 Web AI 提供商的隔离沙箱与指纹档案。
type ProviderProfileManager struct {
	baseDir  string
	profiles map[string]*fingerprint.Profile
}

// NewProviderProfileManager 初始化沙箱管理器。
// 默认基础目录位于 ~/.webtouch/profiles。
func NewProviderProfileManager(baseDir string) (*ProviderProfileManager, error) {
	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("profile_manager: 无法获取家目录: %w", err)
		}
		baseDir = filepath.Join(home, ".webtouch", "profiles")
	}
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, fmt.Errorf("profile_manager: 无法创建沙箱目录: %w", err)
	}

	return &ProviderProfileManager{
		baseDir:  baseDir,
		profiles: make(map[string]*fingerprint.Profile),
	}, nil
}

// ProviderProfileDir 返回指定提供商专属的 user-data-dir。
func (m *ProviderProfileManager) ProviderProfileDir(provider string) string {
	cleanName := strings.ToLower(strings.TrimSpace(provider))
	if cleanName == "" {
		cleanName = "default"
	}
	return filepath.Join(m.baseDir, cleanName)
}

// EnsureProviderProfile 获取或创建指定提供商的独立指纹档案。
func (m *ProviderProfileManager) EnsureProviderProfile(provider string) (*fingerprint.Profile, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if p, ok := m.profiles[provider]; ok {
		return p, nil
	}

	profileID := fmt.Sprintf("webtouch-ai-%s", provider)
	profileDir := m.ProviderProfileDir(provider)
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		return nil, fmt.Errorf("profile_manager: 无法创建提供商目录 %s: %w", profileDir, err)
	}

	// 依据提供商确定性派生档案种子
	seed := uint32(0)
	for i, r := range provider {
		seed += uint32(r) * uint32(i+1) * 31337
	}

	// 构造坚固的指纹档案：
	// WebRTC 策略设为 disable_non_proxied_udp 阻断物理网卡泄漏
	profile := &fingerprint.Profile{
		Schema:    fingerprint.SchemaV1,
		ProfileID: profileID,
		Seed:      seed,
		TargetPlatform: fingerprint.TargetPlatform{
			OS:            "macOS",
			OSVersion:     "14.5.0",
			Architecture:  "arm64",
			ChromeVersion: "152.0.7933.0",
		},
		Emulation: fingerprint.Emulation{
			UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
			UserAgentMetadata: fingerprint.UserAgentMetadata{
				Brands: []fingerprint.Brand{
					{Brand: "Chromium", Version: "152"},
					{Brand: "Google Chrome", Version: "152"},
				},
				FullVersion:     "152.0.7933.0",
				Platform:        "macOS",
				PlatformVersion: "14.5.0",
				Architecture:    "arm",
				Model:           "",
				Mobile:          false,
			},
			Screen: fingerprint.Screen{
				Width:             1440,
				Height:            900,
				DeviceScaleFactor: 2.0,
				ColorDepth:        24,
			},
			Locale: fingerprint.Locale{
				TimezoneID: "Asia/Shanghai",
				Languages:  []string{"zh-CN", "zh", "en"},
			},
			Hardware: fingerprint.Hardware{
				HardwareConcurrency: 8,
				DeviceMemory:        8,
			},
		},
		Network: fingerprint.Network{
			WebRTCPolicy: fingerprint.WebRTCPolicyDisableNonProxied,
		},
	}

	if err := profile.Validate(); err != nil {
		return nil, fmt.Errorf("profile_manager: 档案校验不自洽: %w", err)
	}

	m.profiles[provider] = profile
	return profile, nil
}

// ResolveAccountProfile resolves an account's already-bound identity from the
// WebTouch registry. It never creates or substitutes a profile.
func (m *ProviderProfileManager) ResolveAccountProfile(accountID string) (*fingerprint.Profile, error) {
	registry, err := fingerprint.OpenRegistry("")
	if err != nil {
		return nil, fmt.Errorf("profile_manager: open identity registry: %w", err)
	}
	profile, err := registry.Resolve(accountID)
	if err != nil {
		return nil, fmt.Errorf("profile_manager: resolve account %q: %w", strings.TrimSpace(accountID), err)
	}
	return profile, nil
}
