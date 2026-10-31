#include "ast_hash.h"

#include <array>
#include <cctype>
#include <cstdint>
#include <cstring>
#include <string>
#include <vector>

namespace uws {

namespace {

constexpr uint64_t kFnvOffset = 1469598103934665603ull;
constexpr uint64_t kFnvPrime = 1099511628211ull;

enum ByteClass : uint8_t {
  kClsOther = 0,   // 空白/符号（操作符）
  kClsQuote = 1,   // ' " `
  kClsDigit = 2,   // 0-9
  kClsIdent = 3,   // [A-Za-z_$]
};

// 256 项字节分类表（扫描热路径免去逐字符多分支判断）。
constexpr std::array<uint8_t, 256> MakeByteClass() {
  std::array<uint8_t, 256> t{};
  for (int c = 0; c < 256; ++c) {
    const unsigned char u = static_cast<unsigned char>(c);
    if (u >= '0' && u <= '9') {
      t[c] = kClsDigit;
    } else if ((u >= 'a' && u <= 'z') || (u >= 'A' && u <= 'Z') || u == '_' || u == '$') {
      t[c] = kClsIdent;
    } else if (u == '\'' || u == '"' || u == '`') {
      t[c] = kClsQuote;
    } else {
      t[c] = kClsOther;
    }
  }
  return t;
}
constexpr std::array<uint8_t, 256> kByteClass = MakeByteClass();

bool IsAsciiAlnum(unsigned char c) {
  return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9');
}

bool IsControlKeyword(const char* data, size_t len) {
  static const char* kKeywords[] = {
      "if", "else", "for", "while", "do", "switch", "case", "default", "break",
      "continue", "return", "try", "catch", "finally", "throw", "async", "await",
      "function", "const", "let", "var", "new", "typeof", "instanceof", "in",
      "of", "delete", "void", "class", "extends", "super", "this",
  };
  if (len < 2 || len > 10) return false;  // 最短 "in"，最长 "instanceof"
  for (const char* k : kKeywords) {
    size_t n = 0;
    while (k[n] != '\0') ++n;
    if (n == len && std::memcmp(k, data, len) == 0) return true;
  }
  return false;
}

// 对单个 token 做 FNV-1a（ASCII 小写归一）并投票进 v[64]。
// 重复 token 连续投票 = 词频加权（线性可加，与原 SimHash 语义一致）。
void VoteToken(int64_t v[64], const char* data, size_t len) {
  uint64_t h = kFnvOffset;
  for (size_t i = 0; i < len; ++i) {
    unsigned char c = static_cast<unsigned char>(data[i]);
    if (c >= 'A' && c <= 'Z') c = static_cast<unsigned char>(c - 'A' + 'a');
    h ^= c;
    h *= kFnvPrime;
  }
  for (int i = 0; i < 64; ++i) {
    v[i] += (h & (1ull << i)) ? 1 : -1;
  }
}

// 小型直接映射缓存：重复 token（如 function/fetch/return）跳过重复的 64 位投票。
struct TokenVoteCache {
  uint64_t key[8] = {0, 0, 0, 0, 0, 0, 0, 0};
  int64_t vec[8][64];
};

TokenVoteCache g_token_cache;

void VoteTokenCached(TokenVoteCache& cache, int64_t v[64], const char* data, size_t len) {
  uint64_t h = kFnvOffset;
  for (size_t i = 0; i < len; ++i) {
    unsigned char c = static_cast<unsigned char>(data[i]);
    if (c >= 'A' && c <= 'Z') c = static_cast<unsigned char>(c - 'A' + 'a');
    h ^= c;
    h *= kFnvPrime;
  }
  const unsigned slot = static_cast<unsigned>(h) & 7u;
  if (cache.key[slot] == h) {
    for (int i = 0; i < 64; ++i) v[i] += cache.vec[slot][i];
    return;
  }
  cache.key[slot] = h;
  for (int i = 0; i < 64; ++i) {
    const int64_t d = (h & (1ull << i)) ? 1 : -1;
    cache.vec[slot][i] = d;
    v[i] += d;
  }
}

uint64_t FinalizeVote(const int64_t v[64]) {
  uint64_t out = 0;
  for (int i = 0; i < 64; ++i) {
    if (v[i] > 0) out |= (1ull << i);
  }
  return out;
}

}  // namespace

uint64_t SimHash64(const std::string& text) {
  int64_t v[64] = {0};
  size_t i = 0;
  while (i < text.size()) {
    const unsigned char c = static_cast<unsigned char>(text[i]);
    if (IsAsciiAlnum(c) || c >= 0x80) {
      const size_t start = i;
      while (i < text.size()) {
        const unsigned char c2 = static_cast<unsigned char>(text[i]);
        if (!(IsAsciiAlnum(c2) || c2 >= 0x80)) break;
        ++i;
      }
      if (i - start >= 2) VoteToken(v, text.data() + start, i - start);
    } else {
      ++i;
    }
  }
  return FinalizeVote(v);
}

// 轻量单遍扫描：去注释/字符串/数字/十六进制字面量，保留操作符（长度≥2）、
// API 调用点（标识符后紧跟 '('）、控制关键字；普通标识符与声明名（function/class
// 后的名字）不参与签名。全程零堆分配（TDD-PERF-01 5000 节点达标依赖此路径）。
uint64_t AstSemanticHash(const char* src, size_t len) {
  if (!src || len == 0) return 0;
  int64_t v[64] = {0};
  size_t i = 0;
  bool prev_was_decl_keyword = false;

  auto vote_if_kept = [&](int kind, const char* data, size_t n) {
    const bool keep = (kind == 1) || (kind == 2) || (kind == 3 && n >= 2);
    if (keep) VoteTokenCached(g_token_cache, v, data, n);
  };

  while (i < len) {
    const unsigned char c = static_cast<unsigned char>(src[i]);
    const uint8_t cls = kByteClass[c];
    if (cls == kClsQuote) {
      const char q = src[i];
      ++i;
      while (i < len && src[i] != q) {
        if (src[i] == '\\') ++i;
        ++i;
      }
      if (i < len) ++i;
      continue;
    }
    if (cls == kClsDigit ||
        (src[i] == '0' && i + 1 < len && (src[i + 1] == 'x' || src[i + 1] == 'X'))) {
      while (i < len && (kByteClass[static_cast<unsigned char>(src[i])] == kClsDigit ||
                         src[i] == '.' || src[i] == '_')) {
        ++i;
      }
      continue;
    }
    if (cls == kClsIdent) {
      const size_t start = i;
      while (i < len && kByteClass[static_cast<unsigned char>(src[i])] == kClsIdent) ++i;
      const size_t n = i - start;
      const bool decl_name = prev_was_decl_keyword;
      prev_was_decl_keyword = false;
      if (decl_name) continue;
      const bool call = i < len && src[i] == '(';
      const bool kw = IsControlKeyword(src + start, n);
      if (call) {
        vote_if_kept(1, src + start, n);
      } else if (kw) {
        vote_if_kept(2, src + start, n);
        prev_was_decl_keyword = (n == 8 && std::memcmp(src + start, "function", 8) == 0) ||
                                (n == 5 && std::memcmp(src + start, "class", 5) == 0);
      }
      continue;
    }
    // 其余字节：空白 / 注释 / 操作符
    const bool ws = c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' ||
                    c == '\v';
    if (ws) {
      ++i;
      continue;
    }
    if (src[i] == '/' && i + 1 < len && src[i + 1] == '/') {
      while (i < len && src[i] != '\n') ++i;
      continue;
    }
    if (src[i] == '/' && i + 1 < len && src[i + 1] == '*') {
      i += 2;
      while (i + 1 < len && !(src[i] == '*' && src[i + 1] == '/')) ++i;
      i = (i + 1 < len) ? i + 2 : len;
      continue;
    }
    const size_t start = i;
    // 操作符游程：空白分隔 token；'/' 可能是注释起点，交由主循环处理。
    while (i < len) {
      const unsigned char c2 = static_cast<unsigned char>(src[i]);
      if (c2 == '/' || kByteClass[c2] != kClsOther) break;
      ++i;
    }
    vote_if_kept(3, src + start, i - start);  // 操作符（长度≥2 才保留）
    prev_was_decl_keyword = false;
  }
  return FinalizeVote(v);
}

// ── Tier 1.5：[State:D] 结构签名内置（逐位对齐 Go StructuralSimHash64） ──
//
// 算法与 Go pkg/matcher.SimHash64 完全一致：
//   token = regex `[a-z0-9\p{Han}]{2,}` 对整体小写后提取；
//   hash(token) = md5(token) 前 8 字节小端 → 逐位投票（命中 +1 / 未命中 -1）；
//   v[i] > 0 置位。本实现只处理 ASCII token（结构短语全为 ASCII），
//   对 >=0x80 字节按整体连续游程处理（与 Go \p{Han} 一致，结构短语不含 CJK）。
namespace {

// RFC1321 MD5（自包含，用于结构签名 token 哈希；PNG/MPEG 等场景不依赖）。
void Rfc1321Md5(const uint8_t* data, size_t len, uint8_t out[16]) {
  constexpr uint32_t kShift[64] = {
      7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
      5, 9,  14, 20, 5, 9,  14, 20, 5, 9,  14, 20, 5, 9,  14, 20,
      4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
      6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21};
  constexpr uint32_t kK[64] = {
      0xd76aa478, 0xe8c7b756, 0x242070db, 0xc1bdceee, 0xf57c0faf, 0x4787c62a,
      0xa8304613, 0xfd469501, 0x698098d8, 0x8b44f7af, 0xffff5bb1, 0x895cd7be,
      0x6b901122, 0xfd987193, 0xa679438e, 0x49b40821, 0xf61e2562, 0xc040b340,
      0x265e5a51, 0xe9b6c7aa, 0xd62f105d, 0x02441453, 0xd8a1e681, 0xe7d3fbc8,
      0x21e1cde6, 0xc33707d6, 0xf4d50d87, 0x455a14ed, 0xa9e3e905, 0xfcefa3f8,
      0x676f02d9, 0x8d2a4c8a, 0xfffa3942, 0x8771f681, 0x6d9d6122, 0xfde5380c,
      0xa4beea44, 0x4bdecfa9, 0xf6bb4b60, 0xbebfbc70, 0x289b7ec6, 0xeaa127fa,
      0xd4ef3085, 0x04881d05, 0xd9d4d039, 0xe6db99e5, 0x1fa27cf8, 0xc4ac5665,
      0xf4292244, 0x432aff97, 0xab9423a7, 0xfc93a039, 0x655b59c3, 0x8f0ccc92,
      0xffeff47d, 0x85845dd1, 0x6fa87e4f, 0xfe2ce6e0, 0xa3014314, 0x4e0811a1,
      0xf7537e82, 0xbd3af235, 0x2ad7d2bb, 0xeb86d391};

  uint32_t a0 = 0x67452301u, b0 = 0xefcdab89u, c0 = 0x98badcfeu, d0 = 0x10325476u;

  // 填充：补 0x80，再补 0x00 到 len%64==56，最后 8 字节 = 原始比特数 LE。
  const uint64_t bit_len = static_cast<uint64_t>(len) * 8u;
  std::vector<uint8_t> msg(data, data + len);
  msg.push_back(0x80);
  while (msg.size() % 64 != 56) msg.push_back(0x00);
  for (int i = 0; i < 8; ++i) {
    msg.push_back(static_cast<uint8_t>((bit_len >> (8 * i)) & 0xff));
  }

  auto f = [](uint32_t x, uint32_t y, uint32_t z) { return (x & y) | (~x & z); };
  auto g = [](uint32_t x, uint32_t y, uint32_t z) { return (x & z) | (y & ~z); };
  auto h_ = [](uint32_t x, uint32_t y, uint32_t z) { return x ^ y ^ z; };
  auto i_ = [](uint32_t x, uint32_t y, uint32_t z) { return y ^ (x | ~z); };
  auto rotl = [](uint32_t x, int c) { return (x << c) | (x >> (32 - c)); };

  for (size_t off = 0; off < msg.size(); off += 64) {
    uint32_t m[16];
    for (int i = 0; i < 16; ++i) {
      const size_t p = off + static_cast<size_t>(i) * 4;
      m[i] = static_cast<uint32_t>(msg[p]) |
             (static_cast<uint32_t>(msg[p + 1]) << 8) |
             (static_cast<uint32_t>(msg[p + 2]) << 16) |
             (static_cast<uint32_t>(msg[p + 3]) << 24);
    }
    uint32_t A = a0, B = b0, C = c0, D = d0;
    for (int i = 0; i < 64; ++i) {
      uint32_t Fn = 0;
      int gi = 0;
      if (i < 16) {
        Fn = f(B, C, D);
        gi = i;
      } else if (i < 32) {
        Fn = g(B, C, D);
        gi = (5 * i + 1) % 16;
      } else if (i < 48) {
        Fn = h_(B, C, D);
        gi = (3 * i + 5) % 16;
      } else {
        Fn = i_(B, C, D);
        gi = (7 * i) % 16;
      }
      const uint32_t tmp = D;
      D = C;
      C = B;
      B = B + rotl(A + Fn + kK[i] + m[gi], static_cast<int>(kShift[i]));
      A = tmp;
    }
    a0 += A;
    b0 += B;
    c0 += C;
    d0 += D;
  }
  uint32_t out32[4] = {a0, b0, c0, d0};
  for (int i = 0; i < 4; ++i) {
    out[i * 4 + 0] = static_cast<uint8_t>(out32[i] & 0xff);
    out[i * 4 + 1] = static_cast<uint8_t>((out32[i] >> 8) & 0xff);
    out[i * 4 + 2] = static_cast<uint8_t>((out32[i] >> 16) & 0xff);
    out[i * 4 + 3] = static_cast<uint8_t>((out32[i] >> 24) & 0xff);
  }
}

void VoteTokenMd5(int64_t v[64], const char* data, size_t len) {
  uint8_t digest[16];
  Rfc1321Md5(reinterpret_cast<const uint8_t*>(data), len, digest);
  // Go: md5.Sum(token)[0:8] 小端 → 64-bit；逐位投票。
  uint64_t h = 0;
  for (int i = 0; i < 8; ++i) h |= static_cast<uint64_t>(digest[i]) << (8 * i);
  for (int i = 0; i < 64; ++i) v[i] += (h & (1ull << i)) ? 1 : -1;
}

}  // namespace

uint64_t GoStructuralSimHash64(const char* data, size_t len) {
  int64_t v[64] = {0};
  size_t i = 0;
  while (i < len) {
    const unsigned char c = static_cast<unsigned char>(data[i]);
    const bool is_alnum = (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9');
    const bool is_han = c >= 0x80;  // 多字节 CJK 均视作 token 字符（与 Go \p{Han} 一致）
    if (is_alnum || is_han) {
      const size_t start = i;
      while (i < len) {
        const unsigned char c2 = static_cast<unsigned char>(data[i]);
        const bool a = (c2 >= 'a' && c2 <= 'z') || (c2 >= '0' && c2 <= '9');
        const bool h2 = c2 >= 0x80;
        if (!(a || h2)) break;
        ++i;
      }
      if (i - start >= 2) VoteTokenMd5(v, data + start, i - start);
    } else {
      ++i;
    }
  }
  return FinalizeVote(v);
}

uint64_t GoStructuralSimHash64(const std::string& feature_phrase) {
  return GoStructuralSimHash64(feature_phrase.data(), feature_phrase.size());
}

uint64_t GoStructuralSimHash64Phrase(const std::string& tag, const std::string& role,
                                    const std::string& ancestor_role) {
  std::string phrase;
  if (!tag.empty()) phrase += "tag:" + tag + " ";
  if (!role.empty()) phrase += "role:" + role + " ";
  if (!ancestor_role.empty()) phrase += "sem-anc-role:" + ancestor_role;
  // 末尾空格裁掉（Go strings.Join 无尾随空格；短语空时返回 0）。
  if (!phrase.empty() && phrase.back() == ' ') phrase.pop_back();
  return GoStructuralSimHash64(phrase);
}

}  // namespace uws
