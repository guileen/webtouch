// Package knowledge 实现知识库双缓冲热加载、原子切换与 P0 自动回滚
// （spec/knowledge-versioning.md；RPC 见 proto/uws_service.proto ReloadKnowledge）。
// 注：spec/architecture 未单列该子包，为承载 TDD-KB-* 可测试性而设的引擎内边界。
package knowledge
