// Package matcher - WSTN 契约 v1 消费端定义 (spec/wstn-routing-v1.schema.json)
//
// 消费方约束：仅依赖请求/结果 Schema (wstn.route.v1) 与示例契约。
// 内部的评分函数、波前采样、能量势能计算与权重学习归算法内部，不得侵入本定义。
package matcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	WSTNSchemaVersion      = "wstn.route.v1"
	WSTNMsgTypeRouteReq    = "route_request"
	WSTNMsgTypeRouteResult = "route_result"

	WSTNStatusFound    = "FOUND"
	WSTNStatusFallback = "FALLBACK"

	WSTNSelectionModeRank   = "rank"
	WSTNSelectionModeSample = "sample"

	WSTNActionClick  = "click"
	WSTNActionInput  = "input"
	WSTNActionSelect = "select"
	WSTNActionScroll = "scroll"
)

var (
	ErrInvalidWSTNMessage = errors.New("invalid wstn message")
	ErrUnsupportedAction  = errors.New("unsupported wstn action")
)

// WSTNRouteLimits 定义寻径预算边界。
type WSTNRouteLimits struct {
	StartK  int `json:"start_k"`
	BranchK int `json:"branch_k"`
	MaxHops int `json:"max_hops"`
}

// DefaultWSTNLimits 提供契约默认推荐预算 (start_k=3, branch_k=6, max_hops=5)。
func DefaultWSTNLimits() WSTNRouteLimits {
	return WSTNRouteLimits{
		StartK:  3,
		BranchK: 6,
		MaxHops: 5,
	}
}

// WSTNRouteRequest 遵循 spec/wstn-routing-v1.schema.json route_request 定义。
type WSTNRouteRequest struct {
	MessageType    string                 `json:"message_type"`
	SchemaVersion  string                 `json:"schema_version"`
	CurrentStateID string                 `json:"current_state_id"`
	TargetStateID  string                 `json:"target_state_id"`
	TargetIntent   string                 `json:"target_intent"`
	Limits         WSTNRouteLimits        `json:"limits"`
	Extensions     map[string]interface{} `json:"extensions"`
}

// NewWSTNRouteRequest 构造标准路由请求对象。
func NewWSTNRouteRequest(currentID, targetID, intent string, limits *WSTNRouteLimits) WSTNRouteRequest {
	lim := DefaultWSTNLimits()
	if limits != nil {
		lim = *limits
	}
	return WSTNRouteRequest{
		MessageType:    WSTNMsgTypeRouteReq,
		SchemaVersion:  WSTNSchemaVersion,
		CurrentStateID: currentID,
		TargetStateID:  targetID,
		TargetIntent:   intent,
		Limits:         lim,
		Extensions:     make(map[string]interface{}),
	}
}

// Validate 校验请求合法性。
func (r *WSTNRouteRequest) Validate() error {
	if r.MessageType != WSTNMsgTypeRouteReq {
		return fmt.Errorf("%w: unexpected message_type '%s'", ErrInvalidWSTNMessage, r.MessageType)
	}
	if r.SchemaVersion != WSTNSchemaVersion {
		return fmt.Errorf("%w: unsupported schema_version '%s'", ErrInvalidWSTNMessage, r.SchemaVersion)
	}
	if r.CurrentStateID == "" {
		return fmt.Errorf("%w: current_state_id is required", ErrInvalidWSTNMessage)
	}
	if r.TargetStateID == "" {
		return fmt.Errorf("%w: target_state_id is required", ErrInvalidWSTNMessage)
	}
	if r.Limits.StartK < 1 || r.Limits.BranchK < 1 || r.Limits.MaxHops < 1 {
		return fmt.Errorf("%w: limits start_k/branch_k/max_hops must be >= 1", ErrInvalidWSTNMessage)
	}
	if r.Extensions == nil {
		return fmt.Errorf("%w: extensions must be an object", ErrInvalidWSTNMessage)
	}
	return nil
}

// WSTNRouteAction 遵循 route_action 定义。
type WSTNRouteAction struct {
	Type           string `json:"type"`
	TargetSemantic string `json:"target_semantic"`
	SourceState    string `json:"source_state"`
	TargetState    string `json:"target_state"`
}

func (a *WSTNRouteAction) Validate() error {
	switch a.Type {
	case WSTNActionClick, WSTNActionInput, WSTNActionSelect, WSTNActionScroll:
	default:
		return fmt.Errorf("%w: '%s'", ErrUnsupportedAction, a.Type)
	}
	if a.TargetSemantic == "" || a.SourceState == "" || a.TargetState == "" {
		return fmt.Errorf("%w: target_semantic, source_state and target_state must not be empty", ErrInvalidWSTNMessage)
	}
	return nil
}

// WSTNRouteResult 遵循 route_result 定义。
type WSTNRouteResult struct {
	MessageType         string                 `json:"message_type"`
	SchemaVersion       string                 `json:"schema_version"`
	Status              string                 `json:"status"` // FOUND | FALLBACK
	StatePath           []string               `json:"state_path"`
	Actions             []WSTNRouteAction      `json:"actions"`
	Confidence          float64                `json:"confidence"`
	Potential           *float64               `json:"potential"`
	Hops                int                    `json:"hops"`
	ExpandedStates      int                    `json:"expanded_states"`
	DecisiveEdgeIndexes []int                  `json:"decisive_edge_indexes"`
	SelectionMode       string                 `json:"selection_mode"` // rank | sample
	Reason              string                 `json:"reason"`
	Extensions          map[string]interface{} `json:"extensions"`
}

// Validate 校验结果对象是否符合 v1 契约规范。
func (res *WSTNRouteResult) Validate() error {
	if res.MessageType != WSTNMsgTypeRouteResult {
		return fmt.Errorf("%w: unexpected result message_type '%s'", ErrInvalidWSTNMessage, res.MessageType)
	}
	if res.SchemaVersion != WSTNSchemaVersion {
		return fmt.Errorf("%w: unsupported result schema_version '%s'", ErrInvalidWSTNMessage, res.SchemaVersion)
	}
	if res.Status != WSTNStatusFound && res.Status != WSTNStatusFallback {
		return fmt.Errorf("%w: invalid status '%s'", ErrInvalidWSTNMessage, res.Status)
	}
	if len(res.StatePath) == 0 {
		return fmt.Errorf("%w: state_path must contain at least 1 state", ErrInvalidWSTNMessage)
	}
	for i, a := range res.Actions {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("action[%d] validation failed: %w", i, err)
		}
	}
	if math.IsNaN(res.Confidence) || math.IsInf(res.Confidence, 0) || res.Confidence < 0.0 || res.Confidence > 1.0 {
		return fmt.Errorf("%w: confidence %f out of range [0, 1]", ErrInvalidWSTNMessage, res.Confidence)
	}
	if res.Potential != nil && (math.IsNaN(*res.Potential) || math.IsInf(*res.Potential, 0)) {
		return fmt.Errorf("%w: potential must be finite or null", ErrInvalidWSTNMessage)
	}
	if res.Hops < 0 || res.ExpandedStates < 0 {
		return fmt.Errorf("%w: hops and expanded_states must be non-negative", ErrInvalidWSTNMessage)
	}
	if res.Hops != len(res.Actions) || len(res.StatePath) != res.Hops+1 {
		return fmt.Errorf("%w: hops/actions/state_path lengths disagree", ErrInvalidWSTNMessage)
	}
	for i, action := range res.Actions {
		if action.SourceState != res.StatePath[i] || action.TargetState != res.StatePath[i+1] {
			return fmt.Errorf("%w: action[%d] is not continuous with state_path", ErrInvalidWSTNMessage, i)
		}
	}
	seenIndexes := make(map[int]struct{}, len(res.DecisiveEdgeIndexes))
	for _, index := range res.DecisiveEdgeIndexes {
		if index < 0 || index >= res.Hops {
			return fmt.Errorf("%w: decisive edge index %d is outside route", ErrInvalidWSTNMessage, index)
		}
		if _, exists := seenIndexes[index]; exists {
			return fmt.Errorf("%w: decisive edge index %d is duplicated", ErrInvalidWSTNMessage, index)
		}
		seenIndexes[index] = struct{}{}
	}
	if res.SelectionMode != WSTNSelectionModeRank && res.SelectionMode != WSTNSelectionModeSample {
		return fmt.Errorf("%w: invalid selection_mode '%s'", ErrInvalidWSTNMessage, res.SelectionMode)
	}
	if res.Extensions == nil {
		return fmt.Errorf("%w: extensions must be an object", ErrInvalidWSTNMessage)
	}
	return nil
}

func decodeStrictWSTN(data []byte, target interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// ParseWSTNRouteRequest 从 JSON 字节流严格反序列化并校验 WSTN 请求。
func ParseWSTNRouteRequest(data []byte) (*WSTNRouteRequest, error) {
	var req WSTNRouteRequest
	if err := decodeStrictWSTN(data, &req); err != nil {
		return nil, fmt.Errorf("failed to unmarshal wstn route_request: %w", err)
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return &req, nil
}

// ParseWSTNRouteResult 从 JSON 字节流安全反序列化并校验 WSTN 结果。
func ParseWSTNRouteResult(data []byte) (*WSTNRouteResult, error) {
	var res WSTNRouteResult
	if err := decodeStrictWSTN(data, &res); err != nil {
		return nil, fmt.Errorf("failed to unmarshal wstn route_result: %w", err)
	}
	if err := res.Validate(); err != nil {
		return nil, err
	}
	return &res, nil
}
