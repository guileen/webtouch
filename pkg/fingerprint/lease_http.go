package fingerprint

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LeaseHTTPHandlerConfig turns a strongly consistent LeaseBackend into a
// small, authenticated service. Terminate TLS before this handler, or bind it
// only to a private loopback/network listener. The bearer token authorizes
// lease coordination only; it never carries browser state or Cookie values.
type LeaseHTTPHandlerConfig struct {
	Store       LeaseBackend
	BearerToken string
	MaxTTL      time.Duration
}

type leaseHTTPHandler struct {
	store  LeaseBackend
	token  string
	maxTTL time.Duration
}

type leaseHTTPRequest struct {
	HolderID string `json:"holder_id"`
	Token    string `json:"token"`
	TTLMS    int64  `json:"ttl_ms"`
}

type leaseHTTPResponse struct {
	Lease *Lease `json:"lease,omitempty"`
	Error string `json:"error,omitempty"`
}

// NewLeaseHTTPHandler returns a handler which intentionally exposes only
// acquire/renew/release. An empty token is rejected rather than silently
// publishing an unauthenticated distributed lock service.
func NewLeaseHTTPHandler(config LeaseHTTPHandlerConfig) (http.Handler, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("lease http: Store 不能为空")
	}
	if strings.TrimSpace(config.BearerToken) == "" {
		return nil, fmt.Errorf("lease http: BearerToken 不能为空")
	}
	if config.MaxTTL <= 0 {
		config.MaxTTL = 5 * time.Minute
	}
	return &leaseHTTPHandler{store: config.Store, token: config.BearerToken, maxTTL: config.MaxTTL}, nil
}

func (h *leaseHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		writeLeaseHTTP(w, http.StatusUnauthorized, leaseHTTPResponse{Error: "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		writeLeaseHTTP(w, http.StatusMethodNotAllowed, leaseHTTPResponse{Error: "method_not_allowed"})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "leases" {
		writeLeaseHTTP(w, http.StatusNotFound, leaseHTTPResponse{Error: "not_found"})
		return
	}
	profileID, operation := parts[2], parts[3]
	if err := ValidIdentity(profileID); err != nil {
		writeLeaseHTTP(w, http.StatusBadRequest, leaseHTTPResponse{Error: "invalid_profile_id"})
		return
	}
	req, err := decodeLeaseHTTPRequest(r.Body)
	if err != nil {
		writeLeaseHTTP(w, http.StatusBadRequest, leaseHTTPResponse{Error: "invalid_request"})
		return
	}
	switch operation {
	case "acquire":
		if err := h.validTTL(req.TTLMS); err != nil || strings.TrimSpace(req.HolderID) == "" {
			writeLeaseHTTP(w, http.StatusBadRequest, leaseHTTPResponse{Error: "invalid_request"})
			return
		}
		lease, err := h.store.Acquire(profileID, req.HolderID, time.Duration(req.TTLMS)*time.Millisecond)
		h.writeResult(w, lease, err)
	case "renew":
		if err := h.validTTL(req.TTLMS); err != nil || strings.TrimSpace(req.HolderID) == "" || strings.TrimSpace(req.Token) == "" {
			writeLeaseHTTP(w, http.StatusBadRequest, leaseHTTPResponse{Error: "invalid_request"})
			return
		}
		lease, err := h.store.Renew(&Lease{ProfileID: profileID, HolderID: req.HolderID, Token: req.Token}, time.Duration(req.TTLMS)*time.Millisecond)
		h.writeResult(w, lease, err)
	case "release":
		if strings.TrimSpace(req.HolderID) == "" || strings.TrimSpace(req.Token) == "" {
			writeLeaseHTTP(w, http.StatusBadRequest, leaseHTTPResponse{Error: "invalid_request"})
			return
		}
		h.writeResult(w, nil, h.store.Release(&Lease{ProfileID: profileID, HolderID: req.HolderID, Token: req.Token}))
	default:
		writeLeaseHTTP(w, http.StatusNotFound, leaseHTTPResponse{Error: "not_found"})
	}
}

func (h *leaseHTTPHandler) authorized(r *http.Request) bool {
	got := r.Header.Get("Authorization")
	want := "Bearer " + h.token
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (h *leaseHTTPHandler) validTTL(milliseconds int64) error {
	if milliseconds <= 0 || time.Duration(milliseconds)*time.Millisecond > h.maxTTL {
		return fmt.Errorf("ttl out of range")
	}
	return nil
}

func (h *leaseHTTPHandler) writeResult(w http.ResponseWriter, lease *Lease, err error) {
	if err == nil {
		writeLeaseHTTP(w, http.StatusOK, leaseHTTPResponse{Lease: lease})
		return
	}
	switch {
	case errors.Is(err, ErrLeaseHeld):
		writeLeaseHTTP(w, http.StatusConflict, leaseHTTPResponse{Error: "lease_held"})
	case errors.Is(err, ErrLeaseLost):
		writeLeaseHTTP(w, http.StatusConflict, leaseHTTPResponse{Error: "lease_lost"})
	case errors.Is(err, ErrLeaseGateBusy):
		writeLeaseHTTP(w, http.StatusServiceUnavailable, leaseHTTPResponse{Error: "lease_gate_busy"})
	default:
		writeLeaseHTTP(w, http.StatusInternalServerError, leaseHTTPResponse{Error: "internal"})
	}
}

func decodeLeaseHTTPRequest(body io.Reader) (leaseHTTPRequest, error) {
	var req leaseHTTPRequest
	decoder := json.NewDecoder(io.LimitReader(body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return leaseHTTPRequest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return leaseHTTPRequest{}, fmt.Errorf("trailing request data")
	}
	return req, nil
}

func writeLeaseHTTP(w http.ResponseWriter, statusCode int, response leaseHTTPResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(response)
}

// HTTPLeaseBackend talks to NewLeaseHTTPHandler. It refuses cleartext remote
// endpoints; http is allowed only for loopback, which keeps local tests and
// same-host development ergonomic without normalizing an unsafe deployment.
type HTTPLeaseBackend struct {
	Endpoint string
	Token    string
	Client   *http.Client
}

func NewHTTPLeaseBackend(endpoint, token string, client *http.Client) (*HTTPLeaseBackend, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("lease http client: endpoint 非法")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return nil, fmt.Errorf("lease http client: 远程 endpoint 必须使用 https")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("lease http client: token 不能为空")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPLeaseBackend{Endpoint: strings.TrimRight(endpoint, "/"), Token: token, Client: client}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (b *HTTPLeaseBackend) Acquire(profileID, holderID string, ttl time.Duration) (*Lease, error) {
	return b.call(context.Background(), profileID, "acquire", leaseHTTPRequest{HolderID: holderID, TTLMS: ttl.Milliseconds()})
}

func (b *HTTPLeaseBackend) Renew(lease *Lease, ttl time.Duration) (*Lease, error) {
	if lease == nil {
		return nil, fmt.Errorf("lease http client: lease 不能为空")
	}
	return b.call(context.Background(), lease.ProfileID, "renew", leaseHTTPRequest{HolderID: lease.HolderID, Token: lease.Token, TTLMS: ttl.Milliseconds()})
}

func (b *HTTPLeaseBackend) Release(lease *Lease) error {
	if lease == nil {
		return fmt.Errorf("lease http client: lease 不能为空")
	}
	_, err := b.call(context.Background(), lease.ProfileID, "release", leaseHTTPRequest{HolderID: lease.HolderID, Token: lease.Token})
	return err
}

func (b *HTTPLeaseBackend) call(ctx context.Context, profileID, operation string, payload leaseHTTPRequest) (*Lease, error) {
	if b == nil || b.Client == nil || strings.TrimSpace(b.Endpoint) == "" || strings.TrimSpace(b.Token) == "" {
		return nil, fmt.Errorf("lease http client: 未初始化")
	}
	if err := ValidIdentity(profileID); err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Endpoint+"/v1/leases/"+url.PathEscape(profileID)+"/"+operation, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("lease http client: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
	req.Header.Set("Content-Type", "application/json")
	response, err := b.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lease http client: request failed: %w", err)
	}
	defer response.Body.Close()
	var decoded leaseHTTPResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8192))
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("lease http client: invalid response: %w", err)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if operation == "release" {
			return nil, nil
		}
		if decoded.Lease == nil {
			return nil, fmt.Errorf("lease http client: missing lease")
		}
		return decoded.Lease, nil
	}
	switch decoded.Error {
	case "lease_held":
		return nil, ErrLeaseHeld
	case "lease_lost":
		return nil, ErrLeaseLost
	case "lease_gate_busy":
		return nil, ErrLeaseGateBusy
	default:
		return nil, fmt.Errorf("lease http client: server rejected request (%s)", decoded.Error)
	}
}
