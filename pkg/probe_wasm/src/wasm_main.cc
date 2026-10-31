// WASM 导出层（仅 UWS_BUILD_WASM 目标编译；standalone wasm，无 JS 胶水）。
//
// 协议：JS 侧把字段文本写入 scratch（uws_probe_scratch），然后调用
// uws_probe_node_field(field, off, len) 提交字段；最后 node_commit 落盘。
// 查询结果同样写入 scratch 由 JS 读取（uws_probe_copy_node /
// uws_probe_overflow_peek）。
#include "probe.h"

#include "ast_hash.h"

#include <cstdint>
#include <cstring>
#include <new>

using namespace uws;

namespace {

alignas(8) char g_arrow_buffer[kWasmArrowBufferBytes];
alignas(8) char g_scratch[kScratchBytes];
alignas(ProbeEngine) char g_engine_storage[sizeof(ProbeEngine)];
ProbeEngine* g_engine = nullptr;

// standalone WASM 实例化时 WebAssembly.instantiate 不会自动运行全局构造器
// （不调用 __wasm_call_ctors/_initialize），故采用惰性 placement-new 初始化，
// 宿主无需记忆初始化调用。
ProbeEngine& Engine() {
  if (!g_engine) {
    g_engine = new (g_engine_storage) ProbeEngine(g_arrow_buffer, kWasmArrowBufferBytes);
  }
  return *g_engine;
}

bool ScratchFits(size_t off, size_t len) {
  return off + len <= kScratchBytes;
}

}  // namespace

extern "C" {

void uws_probe_reset() { Engine().Reset(); }

int uws_probe_node_begin() { return static_cast<int>(Engine().BeginNode()); }

int uws_probe_node_field(int field, int off, int len) {
  if (off < 0 || len < 0 || !ScratchFits(static_cast<size_t>(off), static_cast<size_t>(len))) {
    return kErrScratch;
  }
  return static_cast<int>(
      Engine().SetField(field, g_scratch + off, static_cast<size_t>(len)));
}

int uws_probe_node_commit() { return static_cast<int>(Engine().CommitNode()); }

int uws_probe_nodes_bulk(int off, int len) {
  if (off < 0 || len < 0 || !ScratchFits(static_cast<size_t>(off), static_cast<size_t>(len))) {
    return kErrScratch;
  }
  return static_cast<int>(Engine().ParseBulk(g_scratch + off, static_cast<size_t>(len)));
}

void uws_probe_finalize() { Engine().Finalize(); }

int uws_probe_node_count() { return static_cast<int>(Engine().node_count()); }

int uws_probe_kept_count() { return static_cast<int>(Engine().kept_count()); }

int uws_probe_truncated() { return Engine().truncated() ? 1 : 0; }

int uws_probe_needs_fallback() { return Engine().needs_fallback() ? 1 : 0; }

int uws_probe_usage_bp() {
  return static_cast<int>(Engine().usage() * 10000.0 + 0.5);
}

int uws_probe_last_error() { return static_cast<int>(Engine().last_error()); }

uint64_t uws_probe_ast_hash(int off, int len) {
  if (off < 0 || len < 0 || !ScratchFits(static_cast<size_t>(off), static_cast<size_t>(len))) {
    return 0;
  }
  return AstSemanticHash(g_scratch + off, static_cast<size_t>(len));
}

int uws_probe_scratch() { return reinterpret_cast<intptr_t>(g_scratch); }

int uws_probe_arrow_buffer() { return reinterpret_cast<intptr_t>(g_arrow_buffer); }

int uws_probe_scratch_size() { return static_cast<int>(kScratchBytes); }

int uws_probe_copy_node(int index, int dst_off, int dst_cap) {
  if (index < 0 || dst_off < 0 || dst_cap < 0) return -1;
  const size_t sz = sizeof(SerializedNode);
  if (!ScratchFits(static_cast<size_t>(dst_off), sz) ||
      static_cast<size_t>(dst_cap) < sz) {
    return -1;
  }
  const SerializedNode* n = Engine().node(static_cast<size_t>(index));
  if (!n) return -1;
  std::memcpy(g_scratch + dst_off, n, sz);
  return static_cast<int>(sz);
}

int uws_probe_overflow_count() {
  const size_t n = Engine().overflow_count();
  return n > static_cast<size_t>(INT32_MAX) ? INT32_MAX : static_cast<int>(n);
}

int uws_probe_overflow_peek(int dst_off, int max) {
  if (max <= 0 || dst_off < 0) return 0;
  const size_t need = sizeof(uint32_t) * static_cast<size_t>(max);
  if (!ScratchFits(static_cast<size_t>(dst_off), need)) return -1;
  uint32_t* out = reinterpret_cast<uint32_t*>(g_scratch + dst_off);
  return static_cast<int>(Engine().OverflowPeek(out, static_cast<size_t>(max)));
}

// 新增：导出为 VLM 格式（精简字符串）写入 Scratch 区
// 返回写入的字节数；参数非法返回 -1，容量不足时截断到 max_len-1（NUL 结尾）
int uws_probe_export_vlm(int dst_off, int max_len) {
  if (dst_off < 0 || max_len <= 0) return -1;
  if (!ScratchFits(static_cast<size_t>(dst_off), static_cast<size_t>(max_len))) return -1;
  
  char* out_ptr = g_scratch + dst_off;
  size_t len = Engine().ExportToVLMFormat(out_ptr, static_cast<size_t>(max_len));
  return static_cast<int>(len);
}

}  // extern "C"
