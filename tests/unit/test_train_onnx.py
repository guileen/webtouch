"""真实 ONNX 训练器单元测试（TDD-ONNX-02/03 真实侧，USER_ACTION #1/#3 闭环）。"""

import onnxruntime

from pipeline.exporter.train_onnx import (
    FEATURE_NAMES,
    N_FEATURES,
    SEED_NEGATIVES,
    SEED_POSITIVES,
    TrainError,
    build_training_set,
    feature_vector,
    train_classifier_onnx,
    training_auc,
)


def test_feature_vector_contract():
    """特征契约：10 维、确定性、数值在合理区间（规格 algorithm-kernel §5.0.18）。"""
    assert len(FEATURE_NAMES) == 10
    f = feature_vector("立即登录", "登录")
    assert len(f) == N_FEATURES == 10
    assert feature_vector("立即登录", "登录") == f  # 确定性
    assert f[2] == 0.0  # 非完全相等
    assert f[3] == 1.0  # 子串命中
    assert 0.0 <= f[0] <= 1.0
    assert 0.0 <= f[5] <= 1.0  # role_compat
    assert 0.0 <= f[6] <= 1.0  # visual_prominence
    assert 0.0 <= f[7] <= 1.0  # context_echo
    assert 0.0 <= f[8] <= 1.0  # landmark_zone
    assert 0.0 <= f[9] <= 1.0  # spatial_distance_decay


def test_build_training_set_deterministic_and_balanced():
    """训练集含正样本 + keyword 负样本 + 困难负样本，双跑一致。"""
    a = build_training_set(SEED_POSITIVES, SEED_NEGATIVES, seed=42)
    b = build_training_set(SEED_POSITIVES, SEED_NEGATIVES, seed=42)
    assert [(s.candidate, s.target, s.label) for s in a] == [
        (s.candidate, s.target, s.label) for s in b
    ]
    positives = [s for s in a if s.label == 1]
    negatives = [s for s in a if s.label == 0]
    assert len(positives) == len(SEED_POSITIVES)
    assert len(negatives) >= len(SEED_NEGATIVES)  # keyword 负样本 + 困难负样本


def test_train_classifier_onnx_real_model():
    """真实 LightGBM→ONNX：<1MB、onnxruntime 可推理、parity 已内置校验。"""
    model = train_classifier_onnx(SEED_POSITIVES, SEED_NEGATIVES)
    assert len(model) < 1_000_000
    session = onnxruntime.InferenceSession(model, providers=["CPUExecutionProvider"])
    import numpy as np

    pos_x = np.array([feature_vector("登录", "登录")], dtype=np.float32)
    _, pos_proba = session.run(None, {"features": pos_x})
    assert pos_proba[0][1] > 0.85  # 训练内正样本置信度 ≥ θ_high

    neg_x = np.array([feature_vector("广告", "登录")], dtype=np.float32)
    _, neg_proba = session.run(None, {"features": neg_x})
    assert neg_proba[0][1] < 0.50  # 负样本置信度 < θ_low


def test_train_classifier_onnx_rejects_oversize():
    """体积上限由 max_size_bytes 强制（防止规格退化）。"""
    try:
        train_classifier_onnx(
            SEED_POSITIVES * 20,
            SEED_NEGATIVES * 10,
            n_estimators=256,
            num_leaves=31,
            max_size_bytes=1024,
        )
    except TrainError as exc:
        assert "超限" in str(exc)
    else:
        raise AssertionError("超限模型应抛 TrainError")


def test_training_auc_golden_set():
    """黄金困难负样本回归集（兄弟 + SimHash 近邻）AUC ≥ 0.90（TDD-ONNX-03 真实侧）。"""
    model = train_classifier_onnx(SEED_POSITIVES, SEED_NEGATIVES)
    assert training_auc(SEED_POSITIVES, SEED_NEGATIVES, model) >= 0.90


def test_train_requires_positives():
    try:
        train_classifier_onnx([], SEED_NEGATIVES)
    except TrainError as exc:
        assert "正样本为空" in str(exc)
    else:
        raise AssertionError("空正样本应抛 TrainError")
