// UWS WASM 探针 native 测试（TDD-WASM-01..08，docs/tdd-checklist.md §1）。
// 性能红线 TDD-WASM-01 的正式断言在 benchmarks/bench_probe_wasm.py
// （WASM 目标 + Node 实测 P99 < 15ms）；本文件覆盖逻辑/内存/哈希/截断。

#include "probe.h"
#include "ast_hash.h"

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

using namespace uws;

static int g_failures = 0;

#define CHECK(cond)                                                       \
  do {                                                                    \
    if (!(cond)) {                                                        \
      std::fprintf(stderr, "FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond); \
      ++g_failures;                                                       \
    }                                                                     \
  } while (0)

static void SetNode(ProbeEngine& e, const char* tag, const char* role,
                    const char* text, bool viewport, const char* ast = "",
                    const char* cls = "", const char* id_ = "") {
  CHECK(e.BeginNode() == kOk);
  CHECK(e.SetField(kFieldTag, tag, std::strlen(tag)) == kOk);
  CHECK(e.SetField(kFieldRole, role, std::strlen(role)) == kOk);
  CHECK(e.SetField(kFieldSemanticText, text, std::strlen(text)) == kOk);
  CHECK(e.SetField(kFieldInViewport, viewport ? "1" : "0", 1) == kOk);
  CHECK(e.SetField(kFieldAstSource, ast, std::strlen(ast)) == kOk);
  CHECK(e.SetField(kFieldClassNames, cls, std::strlen(cls)) == kOk);
  CHECK(e.SetField(kFieldId, id_, std::strlen(id_)) == kOk);
  CHECK(e.CommitNode() == kOk);
}

// 批量输入协议（'\x1e' 记录分隔、'\x1f' 字段分隔，19 字段固定序）。
static std::string BulkNode(const std::vector<std::string>& fields) {
  std::string s;
  for (size_t i = 0; i < fields.size(); ++i) {
    if (i) s += '\x1f';
    s += fields[i];
  }
  s += '\x1e';
  return s;
}

static std::string MakeBulkNode(int i) {
  return BulkNode({
      "button", "button", i % 2 ? "登录" : "提交", "", "", "", "", "", "", "0",
      "", "0", "0", "1", "", "function f(){fetch('/x');}", "", "btn",
      "id" + std::to_string(i),
  });
}

// TDD-WASM-01 冒烟：100 个交互节点全量提取，节点数与特征字段正确。
static void TestWasm01Smoke100Nodes() {
  ProbeEngine e;
  for (int i = 0; i < 100; ++i) {
    SetNode(e, "button", "button", "提交", true,
            "function f(){ fetch('/api'); }", "btn", "submit");
  }
  e.Finalize();
  CHECK(e.node_count() == 100);
  CHECK(e.kept_count() == 100);
  CHECK(!e.truncated());
  const SerializedNode* n = e.node(0);
  CHECK(n != nullptr);
  CHECK(std::strcmp(n->tag, "button") == 0);
  CHECK(std::strcmp(n->role, "button") == 0);
  CHECK(std::strcmp(n->semantic_text, "提交") == 0);
  CHECK(std::strcmp(n->composite_key, "[ROLE:button][TEXT:提交]") == 0);
  CHECK(n->flags & 4u);  // in_viewport
  CHECK(n->ast_semantic_hash != 0);
  // Tier 1.5：WASM 内置结构签名 = Go StructuralSimHash64({button,button})
  // = 0x980A324380A050CC（跨语言黄金值，见 TestWasm11）。
  CHECK(n->structural_hash == 0x980A324380A050CCULL);
}

// TDD-WASM-02：序列化后超过 16MB 的页面 → 安全截断，不 OOM、不越界。
static void TestWasm02Truncation16Mb() {
  ProbeEngine e(kArrowBufferBytes);
  constexpr int kNodes = 20000;  // 20000 × sizeof(SerializedNode) ≈ 25MB > 16MB
  for (int i = 0; i < kNodes; ++i) {
    SetNode(e, "button", "button", "登录", true, "function f(){ fetch('/x'); }");
  }
  e.Finalize();
  CHECK(e.truncated());
  CHECK(e.needs_fallback());
  CHECK(e.node_count() == static_cast<size_t>(kNodes));
  CHECK(e.kept_count() > 0);
  CHECK(e.overflow_count() == static_cast<size_t>(kNodes) - e.kept_count());
  CHECK(e.usage_bytes() <= e.capacity());
  CHECK(e.last_error() == kOk);
}

// TDD-WASM-03：截断按优先级保留（视口内 > 交互 > a > div[role] > 其他），
// 高优先级节点可驱逐更早到达的低优先级节点；溢出队列保留 BFS 序。
static void TestWasm03PriorityTruncation() {
  ProbeEngine e(3 * sizeof(SerializedNode) + 1);
  SetNode(e, "div", "group", "广告1", false, "", "ad");           // id0 tier3
  SetNode(e, "div", "group", "广告2", false, "", "ad");           // id1 tier3
  SetNode(e, "a", "", "详情", false, "", "link");                 // id2 tier2
  SetNode(e, "button", "button", "登录", true, "", "btn");        // id3 tier0
  SetNode(e, "div", "", "尾注", false, "", "foot");               // id4 tier4
  e.Finalize();
  CHECK(e.truncated());
  // 容量 3：id0 被 id3（视口内登录）驱逐，id4（tier4）无法驱逐更差者 → 溢出。
  // kept = [id1(t3), id2(t2), id3(t0)]；溢出 = [id0(t3), id4(t4)]（BFS 序）。
  CHECK(e.kept_count() == 3);
  CHECK(e.overflow_count() == 2);
  CHECK(e.node(0)->tier == 3);
  CHECK(e.node(1)->tier == 2);
  CHECK(e.node(2)->tier == 0);  // 视口内登录按钮必然保留
  // 溢出队列按 BFS 序（先被驱逐的 id 更小）
  uint32_t ov[2] = {0, 0};
  CHECK(e.OverflowPeek(ov, 2) == 2);
  CHECK(ov[0] == 0);
  CHECK(ov[1] == 4);
  CHECK(ov[0] < ov[1]);
}

// TDD-WASM-04：AST 降噪哈希对十六进制地址/动态字符串免疫 → 完全一致。
static void TestWasm04AstHashLiteralImmunity() {
  const char* a =
      "function onClick(){ const p = fetch('/api/0x1f2a3b4c', {cache:'no-store'}); "
      "p.preventDefault(); return 0xdeadbeef; }";
  const char* b =
      "function onClick(){ const p = fetch('/api/0x9f8e7d6c', {cache:'no-store'}); "
      "p.preventDefault(); return 0xcafebabe; }";
  CHECK(ProbeEngine::AstHash(a, std::strlen(a)) ==
        ProbeEngine::AstHash(b, std::strlen(b)));
}

// TDD-WASM-05：剥离动态属性后，相同的 API 调用链（fetch + preventDefault）
// 即使变量名/字面量不同，签名一致；缺失调用链则不同。
static void TestWasm05ApiChainRecognition() {
  const char* a =
      "function submitA(ev){ ev.preventDefault(); const data = {k:'v'}; "
      "fetch('/a/0x11', data); }";
  const char* b =
      "function submitB(ev2){ ev2.preventDefault(); const payload = {k2:'v2'}; "
      "fetch('/b/0x22', payload); }";
  const char* c =
      "function submitC(ev3){ ev3.stopPropagation(); const payload = {k3:'v3'}; "
      "fetch('/c/0x33', payload); }";  // 无 preventDefault → 签名不同
  const uint64_t ha = ProbeEngine::AstHash(a, std::strlen(a));
  const uint64_t hb = ProbeEngine::AstHash(b, std::strlen(b));
  const uint64_t hc = ProbeEngine::AstHash(c, std::strlen(c));
  CHECK(ha == hb);
  CHECK(ha != hc);
}

// TDD-WASM-06：1000 对「同业务逻辑、不同随机字面量」降噪哈希一致率 > 95%。
static void TestWasm06CollisionRate() {
  constexpr int kPairs = 1000;
  uint32_t seed = 0x5eed1234u;
  auto rnd = [&seed]() {
    seed = seed * 1664525u + 1013904223u;
    return seed;
  };
  int same = 0;
  for (int i = 0; i < kPairs; ++i) {
    const char* hex1 = "0x1a2b3c4d";
    const char* hex2 = "0x5e6f7081";
    char buf1[256], buf2[256];
    std::snprintf(buf1, sizeof(buf1),
                  "function tick(){ const r = fetch('/api/%08x', {v:'%08x'}); "
                  "r.preventDefault(); return %s; }",
                  rnd(), rnd(), hex1);
    std::snprintf(buf2, sizeof(buf2),
                  "function tick(){ const r = fetch('/api/%08x', {v:'%08x'}); "
                  "r.preventDefault(); return %s; }",
                  rnd(), rnd(), hex2);
    if (ProbeEngine::AstHash(buf1, std::strlen(buf1)) ==
        ProbeEngine::AstHash(buf2, std::strlen(buf2))) {
      ++same;
    }
  }
  CHECK(same > static_cast<int>(kPairs * 0.95));
}

// TDD-WASM-07（核心）：溢出队列保留 BFS 序，JS 降级补偿可取前 200 个。
static void TestWasm07OverflowBfsPeek() {
  ProbeEngine e(sizeof(SerializedNode) + 1);  // 只保留 1 个
  constexpr int kNodes = 250;
  for (int i = 0; i < kNodes; ++i) {
    SetNode(e, "button", "button", "项", false, "", "item");
  }
  e.Finalize();
  CHECK(e.truncated());
  CHECK(e.overflow_count() == static_cast<size_t>(kNodes - 1));
  std::vector<uint32_t> ids(200, 0);
  const size_t got = e.OverflowPeek(ids.data(), ids.size());
  CHECK(got == 200);
  CHECK(ids[0] == 1);      // BFS 序：id0 保留，id1.. 依次溢出
  CHECK(ids[199] == 200);
}

// TDD-WASM-08：usage > 95% 触发降级标记；16MB 硬边界越界受控报错不崩溃。
static void TestWasm08HardBoundary() {
  ProbeEngine e(3 * sizeof(SerializedNode));
  for (int i = 0; i < 3; ++i) {
    SetNode(e, "button", "button", "登录", false, "", "btn");
  }
  CHECK(e.usage() > kFallbackThreshold);
  CHECK(e.needs_fallback());
  CHECK(e.usage_bytes() == e.capacity());
  // 第 4 个节点：越界 → 受控进溢出队列，不崩溃、不越界写
  SetNode(e, "button", "button", "注册", false, "", "btn");
  CHECK(e.truncated());
  CHECK(e.overflow_count() == 1);
  CHECK(e.usage_bytes() <= e.capacity());
  CHECK(e.last_error() == kOk);
  // 参数边界
  CHECK(e.BeginNode() == kOk);
  CHECK(e.SetField(999, "x", 1) == kErrInvalidField);
  CHECK(e.CommitNode() == kOk);
  ProbeEngine e2;
  CHECK(e2.CommitNode() == kErrNoNode);
}

// 批量输入路径：单次 bulk 调用完成多节点解析（真实页内注入形态）。
static void TestWasmBulkParse() {
  ProbeEngine e;
  std::string payload;
  for (int i = 0; i < 3; ++i) payload += MakeBulkNode(i);
  // 分块输入：按记录边界切成两块，模拟超大页面的分块注入。
  size_t split = payload.find('\x1e', payload.size() / 2) + 1;
  CHECK(e.ParseBulk(payload.data(), split) == kOk);
  CHECK(e.node_count() == 2);
  CHECK(e.ParseBulk(payload.data() + split, payload.size() - split) == kOk);
  e.Finalize();
  CHECK(e.node_count() == 3);
  CHECK(e.kept_count() == 3);
  const SerializedNode* n0 = e.node(0);
  CHECK(n0 != nullptr);
  CHECK(std::strcmp(n0->semantic_text, "提交") == 0);
  CHECK(std::strcmp(n0->composite_key, "[ROLE:button][TEXT:提交]") == 0);
  CHECK(n0->flags & 4u);  // in_viewport
  CHECK(n0->ast_semantic_hash != 0);
  const SerializedNode* n2 = e.node(2);
  CHECK(n2 != nullptr);
  CHECK(std::strcmp(n2->id, "id2") == 0);
  CHECK(std::strcmp(n2->semantic_text, "提交") == 0);
  CHECK(std::strcmp(e.node(1)->semantic_text, "登录") == 0);
}

// TDD-WASM-09：VLM 极简 AXTree 导出（ExportToVLMFormat）。
// 格式 = [node_id] tag(role) "semantic_text" [offscreen]，保留 node_id 供
// VLM 精确指认；剔除 tier>3 且无语义文本的装饰节点；视口外节点打 [offscreen]。
static void TestWasm09ExportVlmFormat() {
  ProbeEngine e;
  SetNode(e, "button", "button", "立即购买", true);   // id0：视口内按钮
  SetNode(e, "input", "textbox", "验证码", true);     // id1：视口内输入框
  SetNode(e, "a", "", "隐私条款", false);             // id2：视口外链接
  SetNode(e, "div", "", "", false);                   // id3：装饰 div（无 role/文本）→ 剔除
  SetNode(e, "div", "group", "广告位", false);        // id4：div[role] 有文本 → 保留
  e.Finalize();
  CHECK(e.kept_count() == 5);  // 5 节点全部保留；id3 仅在导出输出层被过滤

  const std::string out = e.ExportToVLMFormat();
  const std::string expected =
      "[0] button(button) \"立即购买\"\n"
      "[1] input(textbox) \"验证码\"\n"
      "[2] a \"隐私条款\" [offscreen]\n"
      "[4] div(group) \"广告位\" [offscreen]\n";
  CHECK(out == expected);

  // 导出不应改变引擎状态：重复导出结果一致。
  CHECK(e.ExportToVLMFormat() == expected);

  // 缓冲区变体：容量充足 → 返回全文长度且以 '\0' 结尾。
  char buf[256];
  std::memset(buf, 0x7f, sizeof(buf));
  const size_t len = e.ExportToVLMFormat(buf, sizeof(buf));
  CHECK(len == expected.size());
  CHECK(std::memcmp(buf, expected.data(), len) == 0);
  CHECK(buf[len] == '\0');

  // 缓冲区变体：容量不足 → 截断到 cap-1，仍以 '\0' 结尾，不越界写。
  char small[16];
  std::memset(small, 0x7f, sizeof(small));
  const size_t small_len = e.ExportToVLMFormat(small, 8);  // 传入容量 8，物理缓冲 16
  CHECK(small_len == 7);
  CHECK(small[7] == '\0');
  CHECK(std::memcmp(small, expected.data(), 7) == 0);
  // cap 之外的物理字节保持哨兵（未越界写）。
  for (size_t i = 8; i < sizeof(small); ++i) CHECK(small[i] == 0x7f);

  // 边界：cap==0 不写、cap==1 只写 '\0'。
  char zero[1] = {0x7f};
  CHECK(e.ExportToVLMFormat(zero, 0) == 0);
  CHECK(zero[0] == 0x7f);
  CHECK(e.ExportToVLMFormat(zero, 1) == 0);
  CHECK(zero[0] == '\0');
}

// TDD-WASM-10（Tier 1.5 接线）：结构特征位图载荷布局（[State:D] ≤64B、无 JSON/字符串）。
// 校验 PackedStructuralPayload 恒 64B，tag/role 词表条目数与 Go structcodec 字面一致，
// 且载荷字段偏移与 Go PackStructFeature 布局一致（跨语言对拍的静态契约）。
static void TestWasm10StructuralPayloadLayout() {
  CHECK(sizeof(PackedStructuralPayload) == 64);
  CHECK(offsetof(PackedStructuralPayload, version) == 0);
  CHECK(offsetof(PackedStructuralPayload, flags) == 1);
  CHECK(offsetof(PackedStructuralPayload, structural_hash) == 2);
  CHECK(offsetof(PackedStructuralPayload, tag_code) == 10);
  CHECK(offsetof(PackedStructuralPayload, role_code) == 12);
  CHECK(offsetof(PackedStructuralPayload, ancestor_role_code) == 14);
  CHECK(offsetof(PackedStructuralPayload, attr_bits) == 16);
  CHECK(offsetof(PackedStructuralPayload, relative_depth) == 24);

  // 词表条目数与 Go tagVocab=30 / roleVocab=29 对齐（索引即 code，跨语言同构）。
  const size_t n_tags = sizeof(kTagVocab) / sizeof(kTagVocab[0]);
  const size_t n_roles = sizeof(kRoleVocab) / sizeof(kRoleVocab[0]);
  CHECK(n_tags == 30);
  CHECK(n_roles == 29);
  CHECK(kTagVocab[0][0] == 'b');   // "button"
  CHECK(kRoleVocab[0][0] == 'b');  // "button"（索引 0 = button，与 Go 一致）
}

// TDD-WASM-11（Tier 1.5 接线）：WASM 内置结构签名与 Go StructuralSimHash64 逐位对齐。
// 黄金值由 Go pkg/matcher.StructuralSimHash64 计算（无 attrs 的候选侧语义）。
static void TestWasm11StructuralHashGoAligned() {
  // 空短语 → 0（无 tag/role/祖先，无结构信号）。
  CHECK(GoStructuralSimHash64("", 0) == 0);
  // tag=button + role=button → 0x980A324380A050CC。
  CHECK(GoStructuralSimHash64Phrase("button", "button", "") == 0x980A324380A050CCULL);
  // tag=button + role=button + 最近语义祖先 role=navigation → 0x98CA724790A090AD。
  CHECK(GoStructuralSimHash64Phrase("button", "button", "navigation") == 0x98CA724790A090ADULL);
  // tag=a + role=link + 祖先 role=navigation → 0x8DDAF64514689329。
  CHECK(GoStructuralSimHash64Phrase("a", "link", "navigation") == 0x8DDAF64514689329ULL);
}

int main() {
  TestWasm01Smoke100Nodes();
  TestWasm02Truncation16Mb();
  TestWasm03PriorityTruncation();
  TestWasm04AstHashLiteralImmunity();
  TestWasm05ApiChainRecognition();
  TestWasm06CollisionRate();
  TestWasm07OverflowBfsPeek();
  TestWasm08HardBoundary();
  TestWasmBulkParse();
  TestWasm09ExportVlmFormat();
  TestWasm10StructuralPayloadLayout();
  TestWasm11StructuralHashGoAligned();
  if (g_failures == 0) {
    std::printf("probe_wasm_test: all TDD-WASM native cases passed\n");
    return 0;
  }
  std::fprintf(stderr, "probe_wasm_test: %d failure(s)\n", g_failures);
  return 1;
}
