#include "probe.h"

#include "ast_hash.h"

#include <algorithm>
#include <cctype>
#include <cstdio>
#include <cstring>

namespace uws {

namespace {

// 每个字段的固定槽位上限（字节，含 NUL 边界）；超长输入受控截断，
// 防止 WASM 线性内存被巨型字段撑爆。
constexpr size_t kFieldMax[kFieldCount] = {
    15,    // tag
    31,    // role
    127,   // semantic_text
    127,   // parent_semantic_text
    31,    // zonal_keyword_0
    31,    // zonal_keyword_1
    31,    // zonal_keyword_2
    15,    // parent_tag
    31,    // parent_role
    7,     // in_form
    15,    // input_type
    3,     // dom_depth
    3,     // is_leaf
    3,     // in_viewport
    255,   // attributes
    1023,  // ast_source（仅参与哈希）
    255,   // xpath
    127,   // class_names
    63,    // id
    31,    // nearest_semantic_ancestor_role
};

bool IsInteractiveTag(const char* tag) {
  return std::strcmp(tag, "button") == 0 || std::strcmp(tag, "input") == 0 ||
         std::strcmp(tag, "select") == 0 || std::strcmp(tag, "textarea") == 0;
}

bool IsInteractiveRole(const char* role) {
  static const char* kRoles[] = {
      "button", "link",  "menuitem", "checkbox", "radio",
      "textbox", "combobox", "option", "tab", "switch",
  };
  for (const char* r : kRoles) {
    if (std::strcmp(role, r) == 0) return true;
  }
  return false;
}

}  // namespace

ProbeEngine::ProbeEngine(size_t capacity) : capacity_(capacity) {
  buffer_ = new char[capacity_ ? capacity_ : 1];
  owns_buffer_ = true;
}

ProbeEngine::ProbeEngine(char* static_buffer, size_t capacity)
    : buffer_(static_buffer), capacity_(capacity), owns_buffer_(false) {}

ProbeEngine::~ProbeEngine() {
  if (owns_buffer_) delete[] buffer_;
}

void ProbeEngine::Reset() {
  used_ = 0;
  kept_count_ = 0;
  node_count_ = 0;
  next_id_ = 0;
  in_node_ = false;
  truncated_ = false;
  needs_fallback_ = false;
  last_error_ = kOk;
  worst_kept_tier_ = 0;
  for (int& c : tier_counts_) c = 0;
  overflow_.clear();
  for (size_t& l : scratch_.field_len) l = 0;
}

ProbeError ProbeEngine::BeginNode() {
  in_node_ = true;
  for (size_t& l : scratch_.field_len) l = 0;
  return kOk;
}

ProbeError ProbeEngine::SetField(int field, const char* data, size_t len) {
  if (field < 0 || field >= kFieldCount) {
    last_error_ = kErrInvalidField;
    return last_error_;
  }
  if (!data) {
    data = "";
    len = 0;
  }
  const size_t cap = kFieldMax[static_cast<size_t>(field)];
  const size_t n = std::min(len, cap);
  std::memcpy(scratch_.fields[field], data, n);
  scratch_.field_len[field] = n;
  return kOk;
}

void ProbeEngine::CopyBounded(char* dst, size_t dst_cap, const char* src, size_t src_len) {
  if (dst_cap == 0) return;
  const size_t n = std::min(src_len, dst_cap - 1);
  std::memcpy(dst, src, n);
  dst[n] = '\0';
}

bool ProbeEngine::ParseFlag(const std::string& s) {
  return ParseFlag(s.data(), s.size());
}

bool ProbeEngine::ParseFlag(const char* data, size_t len) {
  if (!data || len == 0) return false;
  const char c = static_cast<char>(std::tolower(static_cast<unsigned char>(data[0])));
  return c == '1' || c == 't';
}

uint32_t ProbeEngine::ParseUint(const std::string& s, uint32_t def) {
  return ParseUint(s.data(), s.size(), def);
}

uint32_t ProbeEngine::ParseUint(const char* data, size_t len, uint32_t def) {
  if (!data || len == 0) return def;
  uint32_t v = 0;
  for (size_t i = 0; i < len; ++i) {
    const unsigned char c = static_cast<unsigned char>(data[i]);
    if (c < '0' || c > '9') return def;
    v = v * 10u + static_cast<uint32_t>(c - '0');
  }
  return v;
}

int ProbeEngine::ComputeTier(const SerializedNode& n) {
  if ((n.flags & 4u) != 0) return 0;  // 视口内
  if (IsInteractiveTag(n.tag) || IsInteractiveRole(n.role)) return 1;
  if (std::strcmp(n.tag, "a") == 0) return 2;
  if (std::strcmp(n.tag, "div") == 0 && n.role[0] != '\0') return 3;
  return 4;
}

void ProbeEngine::BuildCompositeKey(SerializedNode& n) {
  // 手工拼接（snprintf 的 %s 格式化在 5000 节点热路径上是可测开销）。
  char* p = n.composite_key;
  char* const end = n.composite_key + sizeof(n.composite_key) - 1;
  auto put = [&p, end](const char* s) {
    while (*s && p < end) *p++ = *s++;
  };
  put("[ROLE:");
  put(n.role);
  put("][TEXT:");
  put(n.semantic_text);
  put("]");
  *p = '\0';
}

void ProbeEngine::FillFromScratch(SerializedNode& n) {
  CopyBounded(n.tag, sizeof(n.tag), scratch_.fields[kFieldTag], scratch_.field_len[kFieldTag]);
  CopyBounded(n.role, sizeof(n.role), scratch_.fields[kFieldRole], scratch_.field_len[kFieldRole]);
  CopyBounded(n.semantic_text, sizeof(n.semantic_text), scratch_.fields[kFieldSemanticText],
              scratch_.field_len[kFieldSemanticText]);
  CopyBounded(n.parent_semantic_text, sizeof(n.parent_semantic_text),
              scratch_.fields[kFieldParentSemanticText],
              scratch_.field_len[kFieldParentSemanticText]);
  for (int i = 0; i < 3; ++i) {
    CopyBounded(n.zonal_keywords[i], sizeof(n.zonal_keywords[i]),
                scratch_.fields[kFieldZonalKeyword0 + i],
                scratch_.field_len[kFieldZonalKeyword0 + i]);
  }
  CopyBounded(n.parent_tag, sizeof(n.parent_tag), scratch_.fields[kFieldParentTag],
              scratch_.field_len[kFieldParentTag]);
  CopyBounded(n.parent_role, sizeof(n.parent_role), scratch_.fields[kFieldParentRole],
              scratch_.field_len[kFieldParentRole]);
  CopyBounded(n.input_type, sizeof(n.input_type), scratch_.fields[kFieldInputType],
              scratch_.field_len[kFieldInputType]);
  CopyBounded(n.attributes, sizeof(n.attributes), scratch_.fields[kFieldAttributes],
              scratch_.field_len[kFieldAttributes]);
  CopyBounded(n.xpath, sizeof(n.xpath), scratch_.fields[kFieldXpath],
              scratch_.field_len[kFieldXpath]);
  CopyBounded(n.class_names, sizeof(n.class_names), scratch_.fields[kFieldClassNames],
              scratch_.field_len[kFieldClassNames]);
  CopyBounded(n.id, sizeof(n.id), scratch_.fields[kFieldId], scratch_.field_len[kFieldId]);

  n.flags = static_cast<uint8_t>(
      (ParseFlag(scratch_.fields[kFieldInForm], scratch_.field_len[kFieldInForm]) ? 1u : 0u) |
      (ParseFlag(scratch_.fields[kFieldIsLeaf], scratch_.field_len[kFieldIsLeaf]) ? 2u : 0u) |
      (ParseFlag(scratch_.fields[kFieldInViewport], scratch_.field_len[kFieldInViewport]) ? 4u : 0u));
  n.dom_depth = static_cast<uint8_t>(
      ParseUint(scratch_.fields[kFieldDomDepth], scratch_.field_len[kFieldDomDepth], 0));
  n.ast_semantic_hash =
      AstSemanticHash(scratch_.fields[kFieldAstSource], scratch_.field_len[kFieldAstSource]);
  n.tier = static_cast<uint8_t>(ComputeTier(n));
  BuildCompositeKey(n);

  // Tier 1.5：[State:D] WASM 内置结构签名（Go StructuralSimHash64 逐位对齐）。
  // 只用 tag/role/最近语义祖先 role（跳过 generic/none 幽灵包装由 JS 侧保证），
  // 不含文本/class/绝对深度。栈缓冲构造短语串，WASM 热路径零堆分配。
  {
    char phrase[256];
    char* p = phrase;
    const char* const end = phrase + sizeof(phrase) - 1;
    auto put = [&p, end](const char* s) {
      while (*s && p < end) *p++ = *s++;
    };
    if (n.tag[0] != '\0') {
      put("tag:");
      put(n.tag);
      put(" ");
    }
    if (n.role[0] != '\0') {
      put("role:");
      put(n.role);
      put(" ");
    }
    const char* anc = scratch_.fields[kFieldNearestSemanticAncestorRole];
    const size_t anc_len = scratch_.field_len[kFieldNearestSemanticAncestorRole];
    if (anc_len > 0) {
      put("sem-anc-role:");
      for (size_t k = 0; k < anc_len && p < end; ++k) *p++ = anc[k];
    }
    if (p > phrase && p[-1] == ' ') --p;
    *p = '\0';
    if (p > phrase) {
      n.structural_hash = GoStructuralSimHash64(phrase, static_cast<size_t>(p - phrase));
    }
  }
}

void ProbeEngine::Append(const SerializedNode& n) {
  std::memcpy(buffer_ + used_, &n, sizeof(n));
  used_ += sizeof(n);
}

bool ProbeEngine::EvictWorstWorseThan(int tier) {
  if (tier >= worst_kept_tier_) return false;
  for (size_t i = 0; i < kept_count_; ++i) {
    const SerializedNode* n =
        reinterpret_cast<const SerializedNode*>(buffer_ + i * sizeof(SerializedNode));
    if (n->tier > tier) {
      // 先取 victim 元数据再 memmove——memmove 后 n 指向的是被移位过来的后一条记录。
      const uint32_t victim_id = n->node_id;
      const int victim_tier = n->tier;
      const size_t victim_off = i * sizeof(SerializedNode);
      const size_t tail = used_ - victim_off - sizeof(SerializedNode);
      std::memmove(buffer_ + victim_off, buffer_ + victim_off + sizeof(SerializedNode), tail);
      used_ -= sizeof(SerializedNode);
      --kept_count_;
      --tier_counts_[victim_tier];
      if (tier_counts_[victim_tier] == 0 && worst_kept_tier_ == victim_tier) {
        int w = 0;
        for (int t = 0; t < 5; ++t) {
          if (tier_counts_[t] > 0) w = t;
        }
        worst_kept_tier_ = w;
      }
      if (overflow_.size() < kMaxOverflowNodes) overflow_.push_back(victim_id);
      return true;
    }
  }
  return false;
}

ProbeError ProbeEngine::CommitNode() {
  if (!in_node_) {
    last_error_ = kErrNoNode;
    return last_error_;
  }
  SerializedNode n;
  std::memset(&n, 0, sizeof(n));
  FillFromScratch(n);
  n.node_id = next_id_++;
  in_node_ = false;

  if (used_ + sizeof(n) <= capacity_) {
    Append(n);
    ++kept_count_;
    ++tier_counts_[n.tier];
    if (n.tier > worst_kept_tier_) worst_kept_tier_ = n.tier;
    if (usage() > kFallbackThreshold) needs_fallback_ = true;
  } else {
    // 缓冲满：尝试驱逐更差节点；失败则直接进溢出队列（均保留 BFS 序）。
    truncated_ = true;
    needs_fallback_ = true;
    if (EvictWorstWorseThan(n.tier)) {
      Append(n);
      ++kept_count_;
      ++tier_counts_[n.tier];
      if (n.tier > worst_kept_tier_) worst_kept_tier_ = n.tier;
    } else {
      if (overflow_.size() < kMaxOverflowNodes) overflow_.push_back(n.node_id);
    }
  }
  ++node_count_;
  return kOk;
}

ProbeError ProbeEngine::ParseBulk(const char* data, size_t len) {
  if (!data || len == 0) return kOk;
  size_t pos = 0;
  while (pos < len) {
    size_t rec_end = pos;
    while (rec_end < len && data[rec_end] != '\x1e') ++rec_end;
    if (rec_end > pos) {
      BeginNode();
      size_t fpos = pos;
      int field = 0;
      while (fpos < rec_end && field < kFieldCount) {
        size_t fend = fpos;
        while (fend < rec_end && data[fend] != '\x1f') ++fend;
        SetField(field, data + fpos, fend - fpos);
        fpos = fend + 1;  // 跳过 '\x1f'
        ++field;
      }
      CommitNode();
    }
    pos = rec_end + 1;  // 跳过 '\x1e'
  }
  return last_error_;
}

void ProbeEngine::Finalize() {
  // 当前实现：kept 记录经驱逐后已就地紧凑；仅清理中间状态。
  in_node_ = false;
  for (size_t& l : scratch_.field_len) l = 0;
}

const SerializedNode* ProbeEngine::node(size_t index) const {
  if (index >= kept_count_) return nullptr;
  return reinterpret_cast<const SerializedNode*>(buffer_ + index * sizeof(SerializedNode));
}

size_t ProbeEngine::OverflowPeek(uint32_t* out, size_t max) const {
  if (!out || max == 0) return 0;
  const size_t n = std::min(max, overflow_.size());
  std::memcpy(out, overflow_.data(), n * sizeof(uint32_t));
  return n;
}

uint64_t ProbeEngine::AstHash(const char* src, size_t len) {
  return AstSemanticHash(src, len);
}

std::string ProbeEngine::ExportToVLMFormat() const {
  std::string out;
  // 预分配以避免频繁拷贝（假设每个节点平均 64 字节）
  out.reserve(kept_count_ * 64);

  for (size_t i = 0; i < kept_count_; ++i) {
    const SerializedNode* n = node(i);
    if (!n) continue;

    // 仅保留有意义的节点 (tier <= 3) 或包含文本的节点
    if (n->tier > 3 && n->semantic_text[0] == '\0') {
      continue; // 跳过无语义的纯装饰节点
    }

    // [id] tag
    out += "[";
    out += std::to_string(n->node_id);
    out += "] ";
    out += n->tag;

    // (role) - 如果有 role
    if (n->role[0] != '\0') {
      out += "(";
      out += n->role;
      out += ")";
    }

    // "semantic_text" - 如果有文本
    if (n->semantic_text[0] != '\0') {
      out += " \"";
      out += n->semantic_text;
      out += "\"";
    }

    // [offscreen] 标记 (标志位 bit2为全视口内)
    // flag & 0x04 == 1 为 in_viewport。反之则为不在视口。
    bool in_viewport = (n->flags & 0x04) != 0;
    if (!in_viewport) {
      out += " [offscreen]";
    }

    out += "\n";
  }
  return out;
}

size_t ProbeEngine::ExportToVLMFormat(char* out_buffer, size_t out_capacity) const {
  if (!out_buffer || out_capacity == 0) return 0;

  std::string text = ExportToVLMFormat();
  size_t copy_len = std::min(text.size(), out_capacity - 1);
  std::memcpy(out_buffer, text.data(), copy_len);
  out_buffer[copy_len] = '\0';
  
  return copy_len;
}

}  // namespace uws
