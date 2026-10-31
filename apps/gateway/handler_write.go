package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ── JSON / 错误 helpers ──

func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "X-Requested-With, Content-Type, Authorization, X-UWS-*")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

func writeSSE(w http.ResponseWriter, payload any) {
	switch v := payload.(type) {
	case []byte:
		_, _ = w.Write(v)
	default:
		data, _ := json.Marshal(payload)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(data)
		_, _ = w.Write([]byte("\n\n"))
	}
}

func writeError(w http.ResponseWriter, status int, message, typ, semantic string) {
	writeJSON(w, status, map[string]any{"error": openAIError(message, typ, semantic)})
}

func openAIError(message, typ, semantic string) map[string]any {
	e := map[string]any{
		"message": message,
		"type":    typ,
		"param":   nil,
		"code":    nil,
	}
	if semantic != "" {
		e["semantic"] = semantic
	}
	return e
}

// ── WebTouch browser-provider 执行失败分类（契约 §5）──
//
// 分类是唯一事实源：HTTP 状态（chatErrorStatus）、错误体（writeChatError）、
// SSE error 帧（chatErrorObject）与整轮重试判据（preflight.go isRetryableChatFailure）
// 共读一份 chatErrorInfo，杜绝「日志写一个码、调用方收到另一个」的漂移
// （同源由 TestChatErrorStatusMatchesWriteChatError 守护）。

// 失败分类码（error.code；调用方按此判断，不读文案——契约 §5）。
const (
	errCodeSessionBindingDead = "session_binding_dead"
	errCodePageNotReady       = "page_not_ready"
	errCodeEngineUnavailable  = "engine_unavailable"
	errCodeUpstreamTimeout    = "upstream_timeout"
	errCodeSubmissionUnknown  = "submission_outcome_unknown"
	errCodeProviderError      = "provider_error"
	errCodeInternal           = "internal_error"
	errCodeAuthCheckFailed    = "authentication_check_failed"
	errCodeRateLimited        = "rate_limited"
	errCodeAccountBanned      = "account_banned"
)

// 调用方动作（error.action）。retry＝原样重发；reset_session＝先删/重建会话；
// relogin＝人工登录；abort＝不要自动重发（可能已提交，先核对原页面）。
const (
	actionRetry        = "retry"
	actionResetSession = "reset_session"
	actionRelogin      = "relogin"
	actionAbort        = "abort"
)

// chatErrorInfo 是一类失败的完整分类。
type chatErrorInfo struct {
	Code       string
	Type       string
	Status     int
	Retryable  bool // 该失败类是否已纳入网关的自动整轮重放（契约 §5）
	RetryAfter int  // 秒；>0 时写 Retry-After 头
	Action     string
}

// chatErrorClasses 是「显式分类码 → 分类」表，供构造点已写死 Code 的错误直接取用。
var chatErrorClasses = map[string]chatErrorInfo{
	errCodeSessionBindingDead: {errCodeSessionBindingDead, "engine_error", http.StatusConflict, true, 0, actionResetSession},
	errCodePageNotReady:       {errCodePageNotReady, "engine_error", http.StatusServiceUnavailable, true, 2, actionRetry},
	errCodeEngineUnavailable:  {errCodeEngineUnavailable, "engine_error", http.StatusServiceUnavailable, true, 2, actionRetry},
	errCodeUpstreamTimeout:    {errCodeUpstreamTimeout, "engine_error", http.StatusGatewayTimeout, false, 0, actionAbort},
	errCodeSubmissionUnknown:  {errCodeSubmissionUnknown, "engine_error", http.StatusBadGateway, false, 0, actionAbort},
	errCodeProviderError:      {errCodeProviderError, "engine_error", http.StatusBadGateway, false, 0, actionAbort},
	errCodeInternal:           {errCodeInternal, "internal_error", http.StatusInternalServerError, false, 0, actionAbort},
	errCodeRateLimited:        {errCodeRateLimited, "rate_limit_error", http.StatusTooManyRequests, false, 300, actionAbort},
	errCodeAccountBanned:      {errCodeAccountBanned, "account_banned_error", http.StatusForbidden, false, 0, actionRelogin},
}

// classifyChatError 给出错误的机器可判分类（唯一事实源）。
//
// 用 errors.As 而非类型断言：网关错误常被 `fmt.Errorf("%w；…")` 包一层
// （如 ensureModel 把菜单自检现场拼进错误），断言会漏掉内层真实类型，把
// 「页签已毁」这种明确可重试的失败退化成 internal_error/500，调用方就没法自动处置。
func textOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func classifyChatError(err error) chatErrorInfo {
	if err == nil {
		return chatErrorClasses[errCodeInternal]
	}
	var badReq *BadRequestError
	if errors.As(err, &badReq) {
		return chatErrorInfo{"invalid_request", "invalid_request_error", http.StatusBadRequest, false, 0, actionAbort}
	}
	var notFound *NotFoundError
	if errors.As(err, &notFound) {
		return chatErrorInfo{"unknown_model", "invalid_request_error", http.StatusNotFound, false, 0, actionAbort}
	}
	var notImpl *NotImplementedError
	if errors.As(err, &notImpl) {
		return chatErrorInfo{"not_implemented", "not_implemented_error", http.StatusNotImplemented, false, 0, actionAbort}
	}
	var authReq *AuthRequiredError
	if errors.As(err, &authReq) {
		return chatErrorInfo{textOr(authReq.Code, "login_required"), "authentication_error", http.StatusUnauthorized, false, 0, actionRelogin}
	}
	var authChk *AuthCheckError
	if errors.As(err, &authChk) {
		return chatErrorInfo{textOr(authChk.Code, errCodeAuthCheckFailed), "authentication_check_failed", http.StatusServiceUnavailable, false, 0, actionRelogin}
	}
	var ce *ChatError
	if errors.As(err, &ce) {
		return classifyChatFailure(ce)
	}
	// 非网关类型错误（含被 Sprintf 掉类型的引擎错误）：走同一梯子，别一律 internal_error。
	return classifyFailureLadder(err)
}

// classifyChatFailure 分类聊天执行失败：先看构造点写死的 Code，再按引擎原文/阶段推定。
// 顺序即优先级；T-26（提交结果未知绝不重放）排在最前。
func classifyChatFailure(e *ChatError) chatErrorInfo {
	if e == nil {
		return chatErrorClasses[errCodeInternal]
	}
	if isSubmissionUnknown(e) {
		return chatErrorClasses[errCodeSubmissionUnknown]
	}
	if _, ok := chatErrorClasses[e.Code]; ok {
		return chatErrorClasses[e.Code]
	}
	return classifyFailureLadder(e)
}

// classifyFailureLadder 是「按结构码/原文推定分类」的唯一梯子，顺序即优先级；
// T-26（提交结果未知绝不重放）排最前。ChatError 与其它错误类型共用，避免两处漂移。
func classifyFailureLadder(err error) chatErrorInfo {
	if isSubmissionUnknown(err) {
		return chatErrorClasses[errCodeSubmissionUnknown]
	}
	// 账号禁言 / 封禁：网页端明确拒绝，绝对禁止自动重放，触发告警并要求人工介入。
	if isAccountBanned(err) {
		return chatErrorClasses[errCodeAccountBanned]
	}
	// 操作频繁 / 429：触发限频熔断，禁止立即重试。
	if isRateLimited(err) {
		return chatErrorClasses[errCodeRateLimited]
	}
	// 绑定失活：引擎找不到会话原 Target，fail-closed 拒绝换页续用。该错误目前是引擎
	// 通用 code=13、无专用 gRPC 码，故在此集中识别一次（不在别处散落 grep）。
	if isSessionBindingDead(err) {
		return chatErrorClasses[errCodeSessionBindingDead]
	}
	if IsTargetDestroyed(err) || isPreflightReadinessFailure(err) {
		return chatErrorClasses[errCodePageNotReady]
	}
	if isEngineUnavailable(err) {
		return chatErrorClasses[errCodeEngineUnavailable]
	}
	if isRoundTimeout(err) {
		return chatErrorClasses[errCodeUpstreamTimeout]
	}
	return chatErrorClasses[errCodeProviderError]
}

// isAccountBanned 识别账号禁言、封号、限制发言等风控拦截
func isAccountBanned(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	patterns := []string{
		"禁言", "已被禁言", "账号异常", "限制使用", "账号已被封禁", "封禁", "限制发言",
		"account is banned", "account banned", "account suspended", "banned from sending",
	}
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// isRateLimited 识别操作过于频繁、429、请求超限
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	patterns := []string{
		"频繁", "操作过于频繁", "请求过于频繁", "请稍后再试", "请稍后重试",
		"429", "too many requests", "rate limit", "rate limited",
		"人机验证", "滑动验证", "security check",
	}
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// isEngineUnavailable 识别「网关 → 引擎这一段不可用」（上游接入面不可用）：
// 请求到了网关，但网关连不上 uws-engine。结构判据优先（EngineError 携带 gRPC
// code=14 UNVAILABLE）；被 Sprintf 掉类型时退化为认 `engine error code=14` 这一条
// 固定格式原文。只认这两条，不用泛化的 connection refused——避免把站点侧网络
// 错误也吞进来当成引擎不可用。
func isEngineUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var ee *EngineError
	if errors.As(err, &ee) && ee.Code == codeUnavailable {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "engine error code=14") || strings.Contains(msg, "引擎不可用")
}

// isSessionBindingDead 识别「会话原 Target 已不存在」这类绑定失活。
func isSessionBindingDead(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "cannot be adopted") || strings.Contains(msg, "not be adopted") ||
		strings.Contains(msg, "已不存在")
}

// chatErrorStatus 是逐次重试日志与 writeChatError 共用的状态映射。
func chatErrorStatus(err error) int { return classifyChatError(err).Status }

// writeChatError 按分类写出错误体（含 code / retryable / action）。
func writeChatError(w http.ResponseWriter, err error) {
	if info := classifyChatError(err); info.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(info.RetryAfter))
	}
	writeJSON(w, classifyChatError(err).Status, map[string]any{"error": chatErrorObject(err)})
}

// chatErrorObject 构造 OpenAI 风格 error 对象（HTTP 体与 SSE error 帧共用）。
// ChatError 上的站点/步骤级 semantic 原样保留，便于定位到具体动作。
func chatErrorObject(err error) map[string]any {
	info := classifyChatError(err)
	e := map[string]any{
		"message":   err.Error(),
		"type":      info.Type,
		"param":     nil,
		"code":      info.Code,
		"retryable": info.Retryable,
		"action":    info.Action,
	}
	if ce, ok := err.(*ChatError); ok && strings.TrimSpace(ce.Semantic) != "" {
		e["semantic"] = ce.Semantic
	}
	return e
}

func readJSONBody(r *http.Request, out *map[string]any) error {
	if err := readJSONInto(r, out); err != nil {
		return err
	}
	if *out == nil {
		*out = map[string]any{}
	}
	return nil
}

// readJSONInto 把请求体反序列化到任意目标；空体不报错、不写入。
func readJSONInto(r *http.Request, out any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("读取请求体失败: %w", err)
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

// jsonString 安全取字符串字段。
func jsonString(v any) string {
	s, _ := v.(string)
	return s
}

// jsonBool 安全取布尔字段。
func jsonBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// flexBool 支持 bool / "on"|"off" / "true"|"false"。
func flexBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		return parseBoolString(t)
	default:
		return false, false
	}
}

// parseBoolString 解析 "on"/"off"/"true"/"false"/"1"/"0"。
func parseBoolString(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "1":
		return true, true
	case "off", "false", "0":
		return false, true
	default:
		return false, false
	}
}
