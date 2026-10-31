package fingerprint

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// StateBundleSchemaV1 is deliberately distinct from the fingerprint profile
// schema: a profile describes a device identity, while a bundle is portable
// authenticated browser state. A bundle never contains proxy credentials.
const StateBundleSchemaV1 = "https://webtouch.dev/schemas/state-bundle.v1.json"

const encryptedStateBundleSchemaV1 = "https://webtouch.dev/schemas/encrypted-state-bundle.v1.json"

// StateBundle is the portable, explicit state boundary. Cookies remain JSON so
// CDP cookie attributes not known to this version are not silently discarded.
// Callers must validate and transform them to the receiving browser protocol.
type StateBundle struct {
	Schema    string              `json:"$schema"`
	ProfileID string              `json:"profile_id"`
	CreatedAt string              `json:"created_at"`
	Cookies   json.RawMessage     `json:"cookies"`
	Origins   []StateBundleOrigin `json:"origins"`
}

type StateBundleOrigin struct {
	Origin         string              `json:"origin"`
	LocalStorage   []StateBundleKVPair `json:"local_storage"`
	SessionStorage []StateBundleKVPair `json:"session_storage,omitempty"`
}

type StateBundleKVPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// NewStateBundle creates a bundle with a canonical timestamp. Exporters fill
// Cookies and Origins, then validate before it crosses a process boundary.
func NewStateBundle(profileID string, now time.Time) (*StateBundle, error) {
	if err := ValidIdentity(profileID); err != nil {
		return nil, fmt.Errorf("state bundle: profile_id 非法: %w", err)
	}
	return &StateBundle{
		Schema:    StateBundleSchemaV1,
		ProfileID: profileID,
		CreatedAt: now.UTC().Format(time.RFC3339),
		Cookies:   json.RawMessage("[]"),
	}, nil
}

func ParseStateBundle(data []byte) (*StateBundle, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var bundle StateBundle
	if err := dec.Decode(&bundle); err != nil {
		return nil, fmt.Errorf("state bundle: 解析失败: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("state bundle: JSON 文档后不允许再有内容")
		}
		return nil, fmt.Errorf("state bundle: JSON 结尾非法: %w", err)
	}
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	return &bundle, nil
}

func (b *StateBundle) Validate() error {
	if b == nil {
		return fmt.Errorf("state bundle: 为空")
	}
	if b.Schema != StateBundleSchemaV1 {
		return fmt.Errorf("state bundle: 不支持的 $schema %q", b.Schema)
	}
	if err := ValidIdentity(b.ProfileID); err != nil {
		return fmt.Errorf("state bundle: profile_id 非法: %w", err)
	}
	if _, err := time.Parse(time.RFC3339, b.CreatedAt); err != nil {
		return fmt.Errorf("state bundle: created_at 不是 RFC3339 时间: %w", err)
	}
	if len(b.Cookies) == 0 {
		return fmt.Errorf("state bundle: cookies 不能为空；空列表请写 []")
	}
	var cookies []map[string]json.RawMessage
	if err := json.Unmarshal(b.Cookies, &cookies); err != nil {
		return fmt.Errorf("state bundle: cookies 必须是 JSON 数组: %w", err)
	}
	for i, cookie := range cookies {
		if cookie == nil {
			return fmt.Errorf("state bundle: cookies[%d] 必须是对象", i)
		}
	}
	for i, origin := range b.Origins {
		if strings.TrimSpace(origin.Origin) == "" {
			return fmt.Errorf("state bundle: origins[%d].origin 不能为空", i)
		}
		for j, kv := range origin.LocalStorage {
			if kv.Key == "" {
				return fmt.Errorf("state bundle: origins[%d].local_storage[%d].key 不能为空", i, j)
			}
		}
		for j, kv := range origin.SessionStorage {
			if kv.Key == "" {
				return fmt.Errorf("state bundle: origins[%d].session_storage[%d].key 不能为空", i, j)
			}
		}
	}
	return nil
}

func (b *StateBundle) Marshal() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(b)
}

type encryptedStateBundle struct {
	Schema     string `json:"$schema"`
	Algorithm  string `json:"algorithm"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// SealStateBundle returns an AES-256-GCM envelope. key must be exactly 32
// random bytes; accepting a password here would invite weak, undocumented KDF
// behavior. Key management belongs to the caller's secret store/key file.
func SealStateBundle(bundle *StateBundle, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("state bundle: AES-256 密钥必须恰好 32 字节")
	}
	plain, err := bundle.Marshal()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("state bundle: 初始化 AES: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("state bundle: 初始化 GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("state bundle: 生成 nonce: %w", err)
	}
	// Schema 绑定为 additional authenticated data, so an envelope cannot be
	// reinterpreted as an unrelated encrypted document.
	ciphertext := gcm.Seal(nil, nonce, plain, []byte(encryptedStateBundleSchemaV1))
	return json.Marshal(encryptedStateBundle{
		Schema: encryptedStateBundleSchemaV1, Algorithm: "AES-256-GCM",
		Nonce: base64.StdEncoding.EncodeToString(nonce), Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	})
}

func OpenStateBundle(data, key []byte) (*StateBundle, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("state bundle: AES-256 密钥必须恰好 32 字节")
	}
	var envelope encryptedStateBundle
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("state bundle: 解析加密信封失败: %w", err)
	}
	if envelope.Schema != encryptedStateBundleSchemaV1 || envelope.Algorithm != "AES-256-GCM" {
		return nil, fmt.Errorf("state bundle: 不支持的加密信封")
	}
	nonce, err := base64.StdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return nil, fmt.Errorf("state bundle: nonce 非法: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("state bundle: ciphertext 非法: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("state bundle: 初始化 AES: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("state bundle: 初始化 GCM: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, []byte(encryptedStateBundleSchemaV1))
	if err != nil {
		return nil, fmt.Errorf("state bundle: 解密或完整性校验失败: %w", err)
	}
	return ParseStateBundle(plain)
}
