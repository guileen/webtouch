"""离线蒸馏管道单元测试（TDD-PIPE-01/02，tdd-checklist.md §6.1）。"""

import gzip

from pipeline.cc_fetcher.topology import CrawlGate, hamming_distance, simhash64
from pipeline.cc_fetcher.warc import WARCReader


def test_tdd_pipe_01_topology_dedup_and_domain_fuse():
    # SimHash 汉明距 ≤ 3 判定相似（同语义变体：长文本 + 少量动态内容）
    base = " ".join(f"词{i}" for i in range(40))
    variant = base + " 时间戳20260819"
    other = " ".join(f"无关词{i}" for i in range(40))
    assert hamming_distance(simhash64(base), simhash64(variant)) <= 3
    assert hamming_distance(simhash64(base), simhash64(other)) > 3

    gate = CrawlGate()
    domain = "example.com"

    # 完全相同的页面只保留 1 条
    assert gate.consider(domain, base) is True
    assert gate.consider(domain, variant) is False
    assert gate.deduper.kept == 1
    # 明显不同的页面保留（新簇）
    assert gate.consider(domain, other) is True
    assert gate.deduper.kept == 2

    # 同域名相似页 > 50 → 域名熔断（拉黑）
    gate = CrawlGate()
    for _ in range(51):
        gate.consider(domain, base)
    assert gate.fuse.is_blocked(domain) is True
    # 熔断后不再接受该域名任何页面
    assert gate.consider(domain, other) is False
    assert gate.deduper.kept == 1


def test_tdd_pipe_02_corrupt_warc_tolerance(tmp_path):
    good = (
        b"WARC/1.0\r\n"
        b"WARC-Type: response\r\n"
        b"WARC-Target-URI: https://a.com/\r\n"
        b"Content-Length: 5\r\n"
        b"\r\n"
        b"hello\r\n\r\n"
    )
    corrupt = (
        b"WARC/1.0\r\n"
        b"WARC-Type: response\r\n"
        b"WARC-Target-URI: https://broken.example/\r\n"
        b"Content-Length: 9999\r\n"
        b"\r\n"
        b"truncated"  # 截断：Content-Length 声明远大于实际内容
    )
    path = tmp_path / "sample.warc.gz"
    with gzip.open(path, "wb") as f:
        f.write(good + corrupt)

    reader = WARCReader(path)
    records = list(reader.records())

    # 有效记录正常产出，损坏记录被跳过且计入失败率，管道不崩溃
    assert [r.uri for r in records] == ["https://a.com/"]
    assert reader.success_count == 1
    assert reader.failure_count == 1


def _warc_record_bytes(uri: str, payload: bytes) -> bytes:
    return (
        f"WARC/1.0\r\nWARC-Type: response\r\n"
        f"WARC-Target-URI: {uri}\r\n"
        f"Content-Length: {len(payload)}\r\n\r\n".encode()
        + payload
        + b"\r\n\r\n"
    )


def test_warc_reader_streams_payload_larger_than_chunk(tmp_path):
    """payload 大于 chunk_size 时按记录级内存流式读取，内容完整。"""
    payload = ("流式正文-" * 100).encode("utf-8")  # ~500B，> chunk_size=64
    assert len(payload) > 64
    raw = _warc_record_bytes("https://a.com/big", payload)
    path = tmp_path / "big.warc"
    path.write_bytes(raw)

    reader = WARCReader(path, chunk_size=64)
    records = list(reader.records())
    assert len(records) == 1
    assert records[0].payload == payload
    assert reader.success_count == 1
    assert reader.failure_count == 0


def test_warc_reader_marker_split_across_chunks(tmp_path):
    """'WARC/' 标记恰好被 chunk 边界切开时仍能识别（跨块拼接）。"""
    payload = b"marker-split-test"
    raw = _warc_record_bytes("https://a.com/m", payload)
    # 前 8 字节恰好切开 "WARC/1.0"（W A R C / 1 . 0）
    assert raw[:8] == b"WARC/1.0"
    path = tmp_path / "split.warc"
    path.write_bytes(raw)

    reader = WARCReader(path, chunk_size=8)
    records = list(reader.records())
    assert [r.uri for r in records] == ["https://a.com/m"]
    assert records[0].payload == payload


def test_warc_reader_plain_wet(tmp_path):
    """未压缩的纯文本 WET（无 gzip 魔数）按裸字节读取。"""
    raw = (
        b"WARC/1.0\r\nWARC-Type: conversion\r\n"
        b"WARC-Target-URI: https://a.com/wet\r\n"
        b"Content-Length: 7\r\n\r\n"
        b"wet-ok!\r\n\r\n"
    )
    path = tmp_path / "plain.wet"
    path.write_bytes(raw)

    reader = WARCReader(path)
    records = list(reader.records())
    assert [r.uri for r in records] == ["https://a.com/wet"]
    assert records[0].content == "wet-ok!"
