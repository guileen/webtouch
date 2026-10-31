package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"uws/pkg/fingerprint"
)

// ID 返回会话 ID。
func (s *cdpSession) ID() string { return s.id }

// Probe 返回 UA/时区探活结果（Emulation 覆盖后与配置一致）。
func (s *cdpSession) Probe() ProbeResult {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	expr := `JSON.stringify({ua: navigator.userAgent, tz: Intl.DateTimeFormat().resolvedOptions().timeZone})`
	raw, err := s.evalResult(ctx, expr)
	if err != nil {
		return ProbeResult{UserAgent: s.cfg.UserAgent, Timezone: s.cfg.Timezone}
	}
	var pr ProbeResult
	_ = json.Unmarshal([]byte(raw), &pr)
	if pr.UserAgent == "" {
		pr.UserAgent = s.cfg.UserAgent
	}
	if pr.Timezone == "" {
		pr.Timezone = s.cfg.Timezone
	}
	return pr
}

// ExportState 导出 Cookie、LocalStorage 和当前页面 tab 的 SessionStorage。
func (s *cdpSession) ExportState(ctx context.Context, targetOrigin string) (*SerializedSessionState, error) {
	state := &SerializedSessionState{}

	// 1. 导出全量 Cookies（浏览器级域方法，必须走 target 会话：
	// Network.getAllCookies 不能经浏览器端点 Send 直发，否则返回 method not found）。
	var out struct {
		Cookies []any `json:"cookies"`
	}
	if err := s.client.CallSession(ctx, s.sessionID, "Network.getAllCookies", nil, &out); err == nil {
		if b, err2 := json.Marshal(out.Cookies); err2 == nil {
			state.CookiesJSON = string(b)
		}
	}

	// 2. 导出特定域的 LocalStorage 与当前 tab 的 SessionStorage。两者都通过
	// DOMStorage 域读取；session storage 的生命周期仍是当前 tab，恢复时必须在
	// 该 tab 的首次导航前写入。
	if targetOrigin != "" {
		readStorage := func(local bool) ([]map[string]any, error) {
			arg := map[string]any{"storageId": map[string]any{"securityOrigin": targetOrigin, "isLocalStorage": local}}
			var result struct {
				Entries [][]string `json:"entries"`
			}
			if err := s.client.CallSession(ctx, s.sessionID, "DOMStorage.getDOMStorageItems", arg, &result); err != nil {
				return nil, err
			}
			values := make([]map[string]any, 0, len(result.Entries))
			for _, kv := range result.Entries {
				if len(kv) == 2 {
					values = append(values, map[string]any{"key": kv[0], "value": kv[1]})
				}
			}
			return values, nil
		}
		local, localErr := readStorage(true)
		session, sessionErr := readStorage(false)
		if localErr == nil && sessionErr == nil {
			state.Origins = append(state.Origins, StorageOriginContext{Origin: targetOrigin, LocalStorage: local, SessionStorage: session})
		}
	}

	return state, nil
}

// ExportStateBundle converts the existing CDP export into the portable State
// Bundle contract. Unlike the legacy ExportState RPC, it fails closed when the
// requested storage origin cannot be read: an incomplete migration snapshot is
// worse than an explicit failure.
func (s *cdpSession) ExportStateBundle(ctx context.Context, profileID, targetOrigin string) (*fingerprint.StateBundle, error) {
	state, err := s.ExportState(ctx, targetOrigin)
	if err != nil {
		return nil, err
	}
	if state.CookiesJSON == "" {
		return nil, fmt.Errorf("cdp: export state bundle: cookies 未导出")
	}
	bundle, err := fingerprint.NewStateBundle(profileID, time.Now())
	if err != nil {
		return nil, err
	}
	bundle.Cookies = json.RawMessage(state.CookiesJSON)
	for _, origin := range state.Origins {
		out := fingerprint.StateBundleOrigin{Origin: origin.Origin}
		for _, pair := range origin.LocalStorage {
			key, keyOK := pair["key"].(string)
			value, valueOK := pair["value"].(string)
			if !keyOK || !valueOK {
				return nil, fmt.Errorf("cdp: export state bundle: localStorage 项非法")
			}
			out.LocalStorage = append(out.LocalStorage, fingerprint.StateBundleKVPair{Key: key, Value: value})
		}
		for _, pair := range origin.SessionStorage {
			key, keyOK := pair["key"].(string)
			value, valueOK := pair["value"].(string)
			if !keyOK || !valueOK {
				return nil, fmt.Errorf("cdp: export state bundle: sessionStorage 项非法")
			}
			out.SessionStorage = append(out.SessionStorage, fingerprint.StateBundleKVPair{Key: key, Value: value})
		}
		bundle.Origins = append(bundle.Origins, out)
	}
	if targetOrigin != "" && len(bundle.Origins) == 0 {
		return nil, fmt.Errorf("cdp: export state bundle: 未能导出 origin %s 的 localStorage", targetOrigin)
	}
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	return bundle, nil
}

// ImportStateBundle restores cookies and schedules each origin's LocalStorage
// and SessionStorage before its first navigation. It is intentionally a
// separate operation from Navigate: callers must import into a clean profile,
// then navigate the same tab once.
func (s *cdpSession) ImportStateBundle(ctx context.Context, bundle *fingerprint.StateBundle) error {
	if err := bundle.Validate(); err != nil {
		return err
	}
	// 新文档脚本只能在首次导航前保证生效。若此时已经到过页面，继续返回
	// 成功会让调用方误以为当前页面的状态已恢复，实际只能等下一次导航才写入。
	// State Bundle 的恢复路径必须是「干净会话 → Import → 首次 Navigate」，所以在
	// 边界处 fail-closed，而不是接受一个延迟生效的导入。
	s.mu.Lock()
	alreadyNavigated := s.navCnt > 0
	s.mu.Unlock()
	if alreadyNavigated {
		return fmt.Errorf("cdp: %w", ErrStateBundleTooLate)
	}
	if s.cfg.ProfileID != "" && s.cfg.ProfileID != bundle.ProfileID {
		return fmt.Errorf("cdp: state bundle profile_id %q 与当前 profile_id %q 不一致", bundle.ProfileID, s.cfg.ProfileID)
	}
	var rawCookies []map[string]json.RawMessage
	if err := json.Unmarshal(bundle.Cookies, &rawCookies); err != nil {
		return fmt.Errorf("cdp: state bundle cookies: %w", err)
	}
	allowed := map[string]bool{"name": true, "value": true, "url": true, "domain": true, "path": true, "secure": true, "httpOnly": true, "sameSite": true, "expires": true, "priority": true, "sameParty": true, "sourceScheme": true, "sourcePort": true, "partitionKey": true}
	cookies := make([]map[string]json.RawMessage, 0, len(rawCookies))
	for i, cookie := range rawCookies {
		clean := make(map[string]json.RawMessage)
		for key, value := range cookie {
			if allowed[key] {
				clean[key] = value
			}
		}
		if _, ok := clean["name"]; !ok {
			return fmt.Errorf("cdp: state bundle cookies[%d] 缺少 name", i)
		}
		if _, ok := clean["value"]; !ok {
			return fmt.Errorf("cdp: state bundle cookies[%d] 缺少 value", i)
		}
		if _, domain := clean["domain"]; !domain {
			if _, url := clean["url"]; !url {
				return fmt.Errorf("cdp: state bundle cookies[%d] 必须含 domain 或 url", i)
			}
		}
		cookies = append(cookies, clean)
	}
	if len(cookies) > 0 {
		if err := s.client.CallSession(ctx, s.sessionID, "Network.setCookies", map[string]any{"cookies": cookies}, nil); err != nil {
			return fmt.Errorf("cdp: import state bundle cookies: %w", err)
		}
	}
	type storageValues struct {
		Local   []fingerprint.StateBundleKVPair `json:"local"`
		Session []fingerprint.StateBundleKVPair `json:"session"`
	}
	storage := make(map[string]storageValues, len(bundle.Origins))
	for _, origin := range bundle.Origins {
		storage[origin.Origin] = storageValues{
			Local:   append([]fingerprint.StateBundleKVPair(nil), origin.LocalStorage...),
			Session: append([]fingerprint.StateBundleKVPair(nil), origin.SessionStorage...),
		}
	}
	if len(storage) == 0 {
		return nil
	}
	encoded, err := json.Marshal(storage)
	if err != nil {
		return fmt.Errorf("cdp: encode state bundle storage: %w", err)
	}
	// JSON is data, not source code; JSON.stringify makes arbitrary key/value
	// strings safe to embed in a new-document script.
	script := `(() => { const entries = ` + string(encoded) + `; const values = entries[location.origin]; if (!values) return; for (const entry of values.local) { try { localStorage.setItem(entry.key, entry.value); } catch (_) {} } for (const entry of values.session) { try { sessionStorage.setItem(entry.key, entry.value); } catch (_) {} } })();`
	if err := s.client.CallSession(ctx, s.sessionID, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": script}, nil); err != nil {
		return fmt.Errorf("cdp: install state bundle storage: %w", err)
	}
	return nil
}
