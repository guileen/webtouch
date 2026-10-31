package matcher

import (
	"encoding/json"
	"testing"
)

func TestWSTNRouteRequest_JSONParity(t *testing.T) {
	req := NewWSTNRouteRequest("checkout.cart", "checkout.complete", "完成结账", nil)
	if err := req.Validate(); err != nil {
		t.Fatalf("request validation failed: %v", err)
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	var parsed WSTNRouteRequest
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal request: %v", err)
	}

	if parsed.CurrentStateID != "checkout.cart" || parsed.TargetStateID != "checkout.complete" {
		t.Errorf("state mismatch: %+v", parsed)
	}
	if parsed.Limits.StartK != 3 || parsed.Limits.BranchK != 6 || parsed.Limits.MaxHops != 5 {
		t.Errorf("default limits mismatch: %+v", parsed.Limits)
	}
}

func TestWSTNRouteResult_SchemaExampleParity(t *testing.T) {
	rawExample := []byte(`{
		"message_type": "route_result",
		"schema_version": "wstn.route.v1",
		"status": "FOUND",
		"state_path": ["checkout.cart", "checkout.review", "checkout.complete"],
		"actions": [
			{
				"type": "click",
				"target_semantic": "继续结账",
				"source_state": "checkout.cart",
				"target_state": "checkout.review"
			},
			{
				"type": "click",
				"target_semantic": "提交订单",
				"source_state": "checkout.review",
				"target_state": "checkout.complete"
			}
		],
		"confidence": 0.91,
		"potential": 0.42,
		"hops": 2,
		"expanded_states": 4,
		"decisive_edge_indexes": [0],
		"selection_mode": "rank",
		"reason": "",
		"extensions": {}
	}`)

	res, err := ParseWSTNRouteResult(rawExample)
	if err != nil {
		t.Fatalf("failed to parse schema example: %v", err)
	}

	if res.Status != WSTNStatusFound {
		t.Errorf("status expected FOUND, got %s", res.Status)
	}
	if len(res.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(res.Actions))
	}
	if res.Actions[0].TargetSemantic != "继续结账" {
		t.Errorf("action 0 target mismatch: %s", res.Actions[0].TargetSemantic)
	}
	if res.Confidence != 0.91 {
		t.Errorf("confidence expected 0.91, got %f", res.Confidence)
	}
	if res.Potential == nil || *res.Potential != 0.42 {
		t.Errorf("potential expected 0.42, got %v", res.Potential)
	}
}

func TestWSTNRouteResult_FallbackValidation(t *testing.T) {
	fallbackJSON := []byte(`{
		"message_type": "route_result",
		"schema_version": "wstn.route.v1",
		"status": "FALLBACK",
		"state_path": ["checkout.cart"],
		"actions": [],
		"confidence": 0.0,
		"potential": null,
		"hops": 0,
		"expanded_states": 3,
		"decisive_edge_indexes": [],
		"selection_mode": "rank",
		"reason": "no path found to target",
		"extensions": {}
	}`)

	res, err := ParseWSTNRouteResult(fallbackJSON)
	if err != nil {
		t.Fatalf("failed to parse fallback result: %v", err)
	}

	if res.Status != WSTNStatusFallback {
		t.Errorf("expected FALLBACK, got %s", res.Status)
	}
	if len(res.Actions) != 0 {
		t.Errorf("expected 0 actions, got %d", len(res.Actions))
	}
}

func TestWSTNRouteResult_InvalidValidation(t *testing.T) {
	invalidJSON := []byte(`{
		"message_type": "route_result",
		"schema_version": "wstn.route.v999",
		"status": "UNKNOWN_STATUS",
		"state_path": []
	}`)

	_, err := ParseWSTNRouteResult(invalidJSON)
	if err == nil {
		t.Fatalf("expected error for invalid schema/status, got nil")
	}
}

func TestWSTNParsersFailClosedOnContractDrift(t *testing.T) {
	request := NewWSTNRouteRequest("start", "target", "", nil)
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseWSTNRouteRequest(rawRequest); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	unknownField := append(rawRequest[:len(rawRequest)-1], []byte(`,"surprise":true}`)...)
	if _, err := ParseWSTNRouteRequest(unknownField); err == nil {
		t.Fatal("unknown top-level field must be rejected")
	}

	brokenChain := []byte(`{
		"message_type":"route_result","schema_version":"wstn.route.v1","status":"FOUND",
		"state_path":["a","b"],"actions":[{"type":"click","target_semantic":"go","source_state":"a","target_state":"c"}],
		"confidence":0.8,"potential":0.2,"hops":1,"expanded_states":1,
		"decisive_edge_indexes":[0],"selection_mode":"rank","reason":"","extensions":{}
	}`)
	if _, err := ParseWSTNRouteResult(brokenChain); err == nil {
		t.Fatal("discontinuous action chain must be rejected")
	}
}
