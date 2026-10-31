package browser

import (
	"context"
	"errors"
	"testing"

	"uws/pkg/browser/internal/scasttest"
)

// TestCDPSessionTargetAndSessionCloseReleaseScreencast 守住画面流随 target/session
// 消失而释放：陈旧流必须拒绝再次 Start（不能向已销毁的 target 写）。
//
// 流本身的单元测试在 pkg/browser/screencast（2026-09-27 复杂度治理·阶段 1 单元 5 出包）；
// 本文件只留需要 *cdpSession 的集成用例。
func TestCDPSessionTargetAndSessionCloseReleaseScreencast(t *testing.T) {
	stream, _ := scasttest.NewStream("flattened-1", "target-1")
	if err := stream.Start(context.Background(), ScreencastOptions{}); err != nil {
		t.Fatal(err)
	}
	stream.HandleFrame("flattened-1", scasttest.FrameParams(t, 1, []byte("frame"), ScreencastMetadata{}))
	session := &cdpSession{targetID: "target-1", screencast: stream, closedCh: make(chan struct{})}
	session.markTargetDestroyed("another-target")
	if _, ok := stream.LatestFrame(); !ok {
		t.Fatal("foreign target destruction must not release this stream")
	}
	session.markTargetDestroyed("target-1")
	if _, ok := stream.LatestFrame(); ok {
		t.Fatal("target destruction must release latest frame")
	}
	if err := stream.Start(context.Background(), ScreencastOptions{}); !errors.Is(err, ErrScreencastClosed) {
		t.Fatalf("start after target destruction = %v, want ErrScreencastClosed", err)
	}

	stream2, _ := scasttest.NewStream("flattened-1", "target-2")
	if err := stream2.Start(context.Background(), ScreencastOptions{}); err != nil {
		t.Fatal(err)
	}
	session2 := &cdpSession{targetID: "target-2", screencast: stream2, closedCh: make(chan struct{})}
	if err := session2.Close(); err != nil {
		t.Fatalf("session close: %v", err)
	}
	if err := session2.Close(); err != nil {
		t.Fatalf("second session close: %v", err)
	}
	if err := stream2.Start(context.Background(), ScreencastOptions{}); !errors.Is(err, ErrScreencastClosed) {
		t.Fatalf("start after session close = %v, want ErrScreencastClosed", err)
	}
}
