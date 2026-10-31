// UWS WASM 探针 —— 特征提取核心（M2，TDD-WASM-01..08）。
//
// 规格依据：spec/algorithm-kernel.md §4.1（NodeFeature）/ §5（WASM 探针参数）
// 与 spec/physical-patches.md §4（16MB 截断 + JS 慢速遍历降级补偿）。
//
// 设计裁决（GLOBAL.md §5 #21）：
// - 16MB 为 WASM 线性内存硬边界（-sINITIAL_MEMORY=16777216，禁止增长）；
//   Arrow 缓冲区取 15MB（16MB 内预留 1MB 给程序/栈/scratch/溢出队列，
//   对应规格「预留 5% 余量防止边界溢出」）；native 测试默认 16MB 全量容量。
// - 节点按「BFS 序输入 + 分级准入」落盘：缓冲满时允许更高优先级节点驱逐
//   最差已存节点（视口内 > 按钮/输入/select > a > div[role] > 其他），
//   被驱逐节点进入溢出队列（保留 BFS 序），供 JS 慢速遍历降级补偿。
// - 序列化为固定宽度二进制记录（Arrow 语义的降级实现；真实列式消费端
//   就绪后再定 Arrow 布局）。
#ifndef UWS_PROBE_H
#define UWS_PROBE_H

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace uws {

inline constexpr size_t kArrowBufferBytes = 16u * 1024u * 1024u;   // native 默认
inline constexpr size_t kWasmArrowBufferBytes = 15u * 1024u * 1024u;  // WASM 内预留
inline constexpr size_t kScratchBytes = 64u * 1024u;
inline constexpr double kFallbackThreshold = 0.95;
inline constexpr int kMaxFallbackNodes = 200;
inline constexpr size_t kMaxOverflowNodes = 300000u;
inline constexpr size_t kFieldMaxBytes = 1024u;  // 单字段硬上限（ast_source 最长）

enum ProbeError : int {
  kOk = 0,
  kErrOverflow = 1,       // 写入越过硬边界（受控，不崩溃）
  kErrInvalidField = 2,   // field id 越界
  kErrNoNode = 3,         // 无进行中的节点
  kErrScratch = 4,        // scratch 空间不足
};

enum Field : int {
  kFieldTag = 0,
  kFieldRole = 1,
  kFieldSemanticText = 2,
  kFieldParentSemanticText = 3,
  kFieldZonalKeyword0 = 4,
  kFieldZonalKeyword1 = 5,
  kFieldZonalKeyword2 = 6,
  kFieldParentTag = 7,
  kFieldParentRole = 8,
  kFieldInForm = 9,
  kFieldInputType = 10,
  kFieldDomDepth = 11,
  kFieldIsLeaf = 12,
  kFieldInViewport = 13,
  kFieldAttributes = 14,
  kFieldAstSource = 15,
  kFieldXpath = 16,
  kFieldClassNames = 17,
  kFieldId = 18,
  // Tier 1.5：[State:D] WASM 内置结构签名——JS 侧按真实 DOM 计算「最近语义祖先
  // role」（跳过 generic/none 幽灵包装），随 bulk 一块送入，探针据此计算
  // structural_hash（与 Go matcher.StructuralSimHash64 逐位对齐）。
  kFieldNearestSemanticAncestorRole = 19,
  kFieldCount = 20,
};

// 固定宽度序列化节点（Arrow 语义降级；字段有硬上限，超长安全截断）。
struct SerializedNode {
  char tag[16];
  char role[32];
  char semantic_text[128];
  char parent_semantic_text[128];
  char zonal_keywords[3][32];
  char parent_tag[16];
  char parent_role[32];
  char input_type[16];
  char attributes[256];
  uint64_t ast_semantic_hash;
  char composite_key[96];
  char xpath[256];
  char class_names[128];
  char id[64];
  uint8_t flags;        // bit0 in_form, bit1 is_leaf, bit2 in_viewport
  uint8_t dom_depth;
  uint8_t tier;         // 0 视口内 / 1 交互 / 2 a / 3 div[role] / 4 其他
  uint8_t reserved;
  uint32_t node_id;     // BFS 序编号（溢出队列引用）
  // Tier 1.5：[State:D] WASM 内置结构签名（Go StructuralSimHash64 逐位对齐）。
  // 由 FillFromScratch 从 tag/role/最近语义祖先 role 计算；0 = 未提供（Go 侧回退重建）。
  uint64_t structural_hash;
};
static_assert(sizeof(SerializedNode) < 2048, "SerializedNode too large");

// ── 结构特征位图载荷（[State:D] RPC 通道：≤64B、无 JSON/明文字符串跨边界） ──
// 与 Go 侧 pkg/matcher/structcodec.go 的 PackStructFeature 布局逐字节对齐：
//   [0]    version
//   [1]    flags（bit0 has_tag / bit1 has_role / bit2 has_anc / bit3 has_anc_role /
//          bit4 has_identity）
//   [2:10] structural_hash（uint64 LE，由 Go 侧 StructuralSimHash64 计算）
//   [10:12] tag_code（uint16 LE，词表索引；0xFFF = 未知）
//   [12:14] role_code（uint16 LE）
//   [14:16] ancestor_role_code（uint16 LE）
//   [16:24] attr_bits（uint64 LE，稳定属性白名单位图）
//   [24]    relative_depth（uint8）
//   [25:64] 保留（清零）
#pragma pack(push, 1)
struct PackedStructuralPayload {
  uint8_t version;
  uint8_t flags;
  uint64_t structural_hash;
  uint16_t tag_code;
  uint16_t role_code;
  uint16_t ancestor_role_code;
  uint64_t attr_bits;
  uint8_t relative_depth;
  uint8_t reserved[39];
};
#pragma pack(pop)
static_assert(sizeof(PackedStructuralPayload) == 64, "PackedStructuralPayload must be 64B");

inline constexpr size_t kStructuralPayloadCodeMask = 0x0FFF;  // 未知/缺省 code

// tag/role 词表（与 Go tagVocab / roleVocab 对齐，索引一致）。
inline const char* const kTagVocab[] = {
    "button", "a", "input", "select", "textarea", "div", "span", "ul", "li",
    "nav", "aside", "main", "header", "footer", "section", "article", "form",
    "label", "img", "h1", "h2", "h3", "p", "table", "td", "tr", "option",
    "menu", "dialog", "iframe"};
inline const char* const kRoleVocab[] = {
    "button", "link", "navigation", "list", "listitem", "banner", "complementary",
    "main", "region", "form", "search", "tab", "tablist", "tabpanel", "checkbox",
    "radio", "menuitem", "combobox", "textbox", "heading", "status", "alert",
    "dialog", "grid", "cell", "row", "columnheader", "none", "generic"};

class ProbeEngine {
 public:
  explicit ProbeEngine(size_t capacity = kArrowBufferBytes);
  ProbeEngine(char* static_buffer, size_t capacity);  // WASM 静态存储
  ~ProbeEngine();

  ProbeEngine(const ProbeEngine&) = delete;
  ProbeEngine& operator=(const ProbeEngine&) = delete;

  void Reset();

  // 节点写入协议：BeginNode → SetField×N → CommitNode（可重复）。
  ProbeError BeginNode();
  ProbeError SetField(int field, const char* data, size_t len);
  ProbeError CommitNode();
  // 批量输入：records 以 '\x1e' 分隔、字段以 '\x1f' 分隔（19 字段固定序，
  // 缺省为空）。真实页内注入路径：JS 一次性序列化 → 单次 bulk 调用，
  // 避免逐字段 JS↔WASM 往返（TDD-PERF-01 <15ms 依赖此路径）。
  ProbeError ParseBulk(const char* data, size_t len);
  void Finalize();  // 幂等；当前仅清理中间状态

  // 只读查询
  size_t node_count() const { return node_count_; }      // 含溢出
  size_t kept_count() const { return kept_count_; }
  size_t overflow_count() const { return overflow_.size(); }
  bool truncated() const { return truncated_; }
  bool needs_fallback() const { return needs_fallback_; }
  size_t usage_bytes() const { return used_; }
  double usage() const {
    return capacity_ ? static_cast<double>(used_) / static_cast<double>(capacity_) : 0.0;
  }
  ProbeError last_error() const { return last_error_; }
  const SerializedNode* node(size_t index) const;        // kept 序
  size_t OverflowPeek(uint32_t* out, size_t max) const;  // BFS 序前 max 个
  const char* buffer() const { return buffer_; }
  size_t capacity() const { return capacity_; }

  static uint64_t AstHash(const char* src, size_t len);

  // 导出为 VLM 多模态推理专用的极简格式（剔除无关内容，附带 node_id）。
  // 输出格式：[node_id] tag(role) "semantic_text" [offscreen]
  std::string ExportToVLMFormat() const;
  size_t ExportToVLMFormat(char* out_buffer, size_t out_capacity) const;

 private:
  struct NodeScratch {
    // 固定 char 槽 + 长度：热路径零堆分配（WASM dlmalloc 小分配慢，
    // TDD-PERF-01 5000 节点 P99 达标依赖此路径）。
    char fields[kFieldCount][kFieldMaxBytes];
    size_t field_len[kFieldCount];
  };

  static int ComputeTier(const SerializedNode& n);
  static void BuildCompositeKey(SerializedNode& n);
  static bool ParseFlag(const std::string& s);
  static bool ParseFlag(const char* data, size_t len);
  static uint32_t ParseUint(const std::string& s, uint32_t def);
  static uint32_t ParseUint(const char* data, size_t len, uint32_t def);
  void CopyBounded(char* dst, size_t dst_cap, const char* src, size_t src_len);
  void FillFromScratch(SerializedNode& n);
  void Append(const SerializedNode& n);
  // 缓冲满时尝试驱逐「比新节点更差」的最差已存节点；被驱逐节点进溢出队列。
  bool EvictWorstWorseThan(int tier);

  char* buffer_ = nullptr;
  bool owns_buffer_ = true;
  size_t capacity_ = 0;
  size_t used_ = 0;
  size_t kept_count_ = 0;
  size_t node_count_ = 0;
  uint32_t next_id_ = 0;
  bool in_node_ = false;
  bool truncated_ = false;
  bool needs_fallback_ = false;
  ProbeError last_error_ = kOk;
  int worst_kept_tier_ = 0;
  int tier_counts_[5] = {0, 0, 0, 0, 0};
  NodeScratch scratch_;
  std::vector<uint32_t> overflow_;
};

}  // namespace uws

#endif  // UWS_PROBE_H
