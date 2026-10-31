# pkg/classifier MEMORY —— 决策核 + 真实 ONNX（A 案 ort-go CGO）

> 模块专属经验；通用经验进 `GLOBAL.md` §5 裁决 #24。

## 真实 ONNX 接线（2026-08-20，USER_ACTION #1 闭环）

- **版本配对是硬约束**：ort-go 的 `ORT_API_VERSION`（onnxruntime_c_api.h）必须与
  libonnxruntime 共享库匹配。本机 = ort-go v1.23.0（ORT_API_VERSION 22）↔
  onnxruntime 1.23.0 dylib（从 .venv wheel 提取，`make ort-lib` → build/ort，
  gitignored）。升级任一侧都要先核对 API 版本，别只看 tag 号。
- **加载**：`ort.NewAdvancedSessionWithONNXData(modelBytes, ...)` 直接吃字节，
  无需临时文件；`InitializeEnvironment` 全进程一次（sync.Once + 错误缓存）。
- **并发**：AdvancedSession 复用同一 input/output tensor，`Run` 前必须持锁
  （并发写 input 竞态）；单次推理 µs 级，锁开销可忽略。
- **Sanity Check = OnnxReady 真实语义**：`NewOnnxScorer` 内跑一次空特征推理，
  成功才算模型就绪（不再是「文件存在即 ready」）。
- **特征对齐三个坑**（与 `train_onnx.py feature_vector` 逐位对拍，golden.json）：
  ① Python `len(str)` 是码点数，Go 必须 `utf8.RuneCountInString`（中文候选的
  log_len 用字节数会偏大——"登录" 2 vs 6）；
  ② token 正则必须与 `topology.py _TOKEN_RE` 一致（`[a-z0-9\p{Han}]{2,}`），
  复用 `matcher.Tokens`，禁止另写一套；
  ③ 空串语义：双双空 sim=1、单空 sim=0、jaccard 空并集=0。
- **决策带消费方**：引擎只对页面候选 Confidence=0 节点做真实打分
  （候选文本 × 目标语义）；语义-only 兜底路径保持 mock（无候选文本时
  pairwise 模型无意义）；L2 同义词离线背书不被 ONNX 覆盖。
- **黄金回归**：`testdata/classifier.onnx` + `golden.json` 由 Python 训练链
  生成并提交（11KB），Go 侧对拍容差 1e-4；ORT 库缺失时测试自动 skip
  （`ONNXRUNTIME_SHARED_LIBRARY_PATH` 或 build/ort 相对路径探测）。
