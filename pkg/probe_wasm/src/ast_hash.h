// AST 降噪语义哈希（spec/algorithm-kernel.md §4.1 / §5「AST语义哈希」）。
//
// 与 Go/Python 侧 simhash64 同构（token 词频加权、逐位投票、64-bit LSH），
// 但哈希函数采用 FNV-1a（内部一致性要求，不承诺跨语言逐位对齐——L3
// SimHash 的跨语言对齐仍以 pkg/matcher ↔ pipeline/topology.py 为准）。
#ifndef UWS_AST_HASH_H
#define UWS_AST_HASH_H

#include <cstddef>
#include <cstdint>
#include <string>

namespace uws {

uint64_t SimHash64(const std::string& text);

// 降噪流程：剥离注释 / 字符串字面量 / 数字与十六进制字面量后，仅保留
// 操作符（长度≥2 的符号串）+ API 调用点（标识符后紧跟 '('）+ 控制关键字，
// 再对签名串计算 SimHash64。同一业务逻辑、不同动态字面量/变量名 → 一致。
uint64_t AstSemanticHash(const char* src, size_t len);

// Tier 1.5：[State:D] 结构签名内置（与 Go pkg/matcher.StructuralSimHash64 逐位对齐）。
// 输入为结构特征短语串（形如 "tag:button role:button sem-anc-role:navigation"），
// 采用与 Go 一致的分词 + md5 前 8 字节小端投票（与 ast_hash.cc 的 FNV 版 SimHash64
// 不同——后者不作跨语言对齐承诺，本函数专为 L3 双通道/拓扑剪枝门跨语言对拍而设）。
uint64_t GoStructuralSimHash64(const std::string& feature_phrase);

// 栈缓冲变体（WASM 热路径零堆分配，TDD-PERF-01 承诺依赖）。
uint64_t GoStructuralSimHash64(const char* data, size_t len);

// 便捷重载：由 tag/role/最近语义祖先 role 构造短语串并计算（ancestor_role 可空）。
uint64_t GoStructuralSimHash64Phrase(const std::string& tag, const std::string& role,
                                    const std::string& ancestor_role);

}  // namespace uws

#endif  // UWS_AST_HASH_H
