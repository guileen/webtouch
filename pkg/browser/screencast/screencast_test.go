package screencast_test

import (
	"context"
	"errors"
	"testing"

	"uws/pkg/browser/internal/scasttest"
	"uws/pkg/browser/screencast"
)

func TestStartDefaultsAndIdempotence(t *testing.T) {
	s, transport := scasttest.NewStream("flattened-1", "target-1")
	if err := s.Start(context.Background(), screencast.Options{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Start(context.Background(), screencast.Options{}); err != nil {
		t.Fatalf("second start: %v", err)
	}
	commands, _ := transport.Snapshot()
	if len(commands) != 1 || commands[0].Method != "Page.startScreencast" {
		t.Fatalf("commands = %#v, want one Page.startScreencast", commands)
	}
	params, ok := commands[0].Params.(map[string]any)
	if !ok {
		t.Fatalf("start params type = %T", commands[0].Params)
	}
	want := map[string]any{
		"format": "jpeg", "quality": 60, "maxWidth": 640, "maxHeight": 480, "everyNthFrame": 1,
	}
	for key, value := range want {
		if got := params[key]; got != value {
			t.Errorf("start param %s = %#v, want %#v", key, got, value)
		}
	}
}

func TestStartUsesCallerOptions(t *testing.T) {
	s, transport := scasttest.NewStream("flattened-1", "target-1")
	options := screencast.Options{Quality: 75, MaxWidth: 320, MaxHeight: 180, EveryNthFrame: 2}
	if err := s.Start(context.Background(), options); err != nil {
		t.Fatalf("start: %v", err)
	}
	commands, _ := transport.Snapshot()
	params := commands[0].Params.(map[string]any)
	for key, want := range map[string]any{
		"format": "jpeg", "quality": 75, "maxWidth": 320, "maxHeight": 180, "everyNthFrame": 2,
	} {
		if got := params[key]; got != want {
			t.Errorf("start param %s = %#v, want %#v", key, got, want)
		}
	}
}

func TestStartRejectsInvalidOptions(t *testing.T) {
	s, transport := scasttest.NewStream("flattened-1", "target-1")
	for _, bad := range []screencast.Options{
		{Format: "png"},
		{Quality: 101},
		{MaxWidth: -1},
	} {
		if err := s.Start(context.Background(), bad); err == nil {
			t.Errorf("Start(%+v) 应当被拒", bad)
		}
	}
	if commands, _ := transport.Snapshot(); len(commands) != 0 {
		t.Fatalf("非法参数不该发出协议命令: %#v", commands)
	}
}

func TestEventDecodesAndAcknowledges(t *testing.T) {
	s, transport := scasttest.NewStream("flattened-1", "target-1")
	if err := s.Start(context.Background(), screencast.Options{}); err != nil {
		t.Fatal(err)
	}
	metadata := screencast.Metadata{
		OffsetTop: 12.5, PageScaleFactor: 1.25, DeviceWidth: 1280, DeviceHeight: 720,
		ScrollOffsetX: 3.5, ScrollOffsetY: 9.5, Timestamp: 42.25,
	}
	s.HandleFrame("flattened-1", scasttest.FrameParams(t, 17, []byte{0xff, 0xd8, 0xff}, metadata))
	frame, ok := s.LatestFrame()
	if !ok {
		t.Fatal("missing latest frame")
	}
	if got, want := string(frame.JPEG), string([]byte{0xff, 0xd8, 0xff}); got != want {
		t.Errorf("JPEG = %x, want %x", frame.JPEG, []byte{0xff, 0xd8, 0xff})
	}
	if frame.Metadata != metadata || frame.CDPSessionID != "flattened-1" || frame.ScreencastSessionID != 17 || frame.TargetID != "target-1" {
		t.Errorf("frame = %+v, want metadata/session/target preserved", frame)
	}
	_, acks := transport.Snapshot()
	if len(acks) != 1 || acks[0].Method != "Page.screencastFrameAck" {
		t.Fatalf("acks = %#v", acks)
	}
	params := acks[0].Params.(map[string]any)
	if got := params["sessionId"]; got != int64(17) {
		t.Errorf("ack sessionId = %#v, want 17", got)
	}
}

func TestLatestFrameOverwritesAndIsDefensive(t *testing.T) {
	s, _ := scasttest.NewStream("flattened-1", "target-1")
	if err := s.Start(context.Background(), screencast.Options{}); err != nil {
		t.Fatal(err)
	}
	s.HandleFrame("flattened-1", scasttest.FrameParams(t, 1, []byte("old"), screencast.Metadata{}))
	s.HandleFrame("flattened-1", scasttest.FrameParams(t, 2, []byte("new"), screencast.Metadata{}))
	frame, ok := s.LatestFrame()
	if !ok || string(frame.JPEG) != "new" || frame.ScreencastSessionID != 2 {
		t.Fatalf("latest frame = %+v, ok=%v; want only new frame", frame, ok)
	}
	frame.JPEG[0] = 'X'
	again, ok := s.LatestFrame()
	if !ok || string(again.JPEG) != "new" {
		t.Fatalf("consumer mutation leaked into frame slot: %+v", again)
	}
}

func TestStopAndReleaseAreIdempotent(t *testing.T) {
	s, transport := scasttest.NewStream("flattened-1", "target-1")
	if err := s.Start(context.Background(), screencast.Options{}); err != nil {
		t.Fatal(err)
	}
	s.HandleFrame("flattened-1", scasttest.FrameParams(t, 1, []byte("frame"), screencast.Metadata{}))
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if _, ok := s.LatestFrame(); ok {
		t.Fatal("stop must release latest frame")
	}
	commands, _ := transport.Snapshot()
	if len(commands) != 2 || commands[1].Method != "Page.stopScreencast" {
		t.Fatalf("commands = %#v, want one stop after start", commands)
	}
	s.Release()
	s.Release()
	if err := s.Start(context.Background(), screencast.Options{}); !errors.Is(err, screencast.ErrClosed) {
		t.Fatalf("start after release = %v, want ErrClosed", err)
	}
}

func TestInvalidOrForeignFrameDoesNotPublish(t *testing.T) {
	s, transport := scasttest.NewStream("flattened-1", "target-1")
	if err := s.Start(context.Background(), screencast.Options{}); err != nil {
		t.Fatal(err)
	}
	// A frame from a different flattened session must not ACK or leak across targets.
	s.HandleFrame("flattened-other", scasttest.FrameParams(t, 7, []byte("foreign"), screencast.Metadata{}))
	// A malformed image still receives the protocol ACK, then is discarded.
	s.HandleFrame("flattened-1", []byte(`{"data":"%%%","sessionId":8}`))
	if _, ok := s.LatestFrame(); ok {
		t.Fatal("invalid or foreign frame must not publish")
	}
	_, acks := transport.Snapshot()
	if len(acks) != 1 {
		t.Fatalf("acks = %#v, want only malformed local frame acknowledged", acks)
	}
}
