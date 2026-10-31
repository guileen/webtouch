package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"uws/pkg/browser/internal/scasttest"
	"uws/pkg/browser/window"
)

func TestViewerBridgeRefreshesOnlyTheLatestScreencastFrame(t *testing.T) {
	stream, _ := scasttest.NewStream("flattened-1", "target-1")
	if err := stream.Start(context.Background(), ScreencastOptions{}); err != nil {
		t.Fatal(err)
	}
	bridge, err := NewViewerBridge(ViewerSessionInfo{ID: "agent-a/run-1", AgentName: "Agent A", PageTitle: "Example", URL: "https://example.test"})
	if err != nil {
		t.Fatalf("NewViewerBridge: %v", err)
	}

	stream.HandleFrame("flattened-1", scasttest.FrameParams(t, 1, []byte("old"), ScreencastMetadata{}))
	stream.HandleFrame("flattened-1", scasttest.FrameParams(t, 2, []byte("new"), ScreencastMetadata{}))
	if !bridge.RefreshScreencast(stream) {
		t.Fatal("RefreshScreencast did not publish the latest frame")
	}
	if bridge.RefreshScreencast(stream) {
		t.Fatal("the same retained frame was published twice")
	}

	snapshot, ok := bridge.LatestViewerUpdate()
	if !ok || snapshot.Frame == nil {
		t.Fatalf("snapshot = %#v, ok=%v; want retained frame", snapshot, ok)
	}
	if got := string(snapshot.Frame.JPEG); got != "new" {
		t.Fatalf("JPEG = %q, want latest frame", got)
	}
	if got := snapshot.Frame.ScreencastSessionID; got != 2 {
		t.Fatalf("ScreencastSessionID = %d, want 2", got)
	}

	// The bridge owns a copy: a caller cannot mutate the retained page image.
	snapshot.Frame.JPEG[0] = 'X'
	again, _ := bridge.LatestViewerUpdate()
	if got := string(again.Frame.JPEG); got != "new" {
		t.Fatalf("consumer mutation leaked into bridge: %q", got)
	}
}

func TestViewerBridgeJSONLineCarriesSessionFramePointerAndRedactedAction(t *testing.T) {
	bridge, err := NewViewerBridge(ViewerSessionInfo{
		ID: "agent-a/run-1", AgentName: "Agent A", PageTitle: "Inbox", URL: "https://example.test/inbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge.PublishFrame(ScreencastFrame{
		JPEG: []byte{0xff, 0xd8, 0xff}, Metadata: ScreencastMetadata{DeviceWidth: 1280, DeviceHeight: 720},
		CDPSessionID: "flattened-1", ScreencastSessionID: 9, TargetID: "target-1",
	})
	bridge.PublishPointer(ViewerPointer{X: 300, Y: 120, Kind: ViewerPointerPressed, Button: "left"})
	bridge.PublishAction(ViewerAction{Kind: "input", Summary: "text entered"})

	var out bytes.Buffer
	if err := bridge.WriteJSONLine(&out); err != nil {
		t.Fatalf("WriteJSONLine: %v", err)
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatal("wire message must end in one JSON line newline")
	}
	var wire ViewerUpdate
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &wire); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	if wire.Version != window.BridgeProtocolVersion || wire.Session.ID != "agent-a/run-1" {
		t.Fatalf("wire identity = %#v", wire)
	}
	if wire.Frame == nil || string(wire.Frame.JPEG) != string([]byte{0xff, 0xd8, 0xff}) || wire.Frame.TargetID != "target-1" {
		t.Fatalf("wire frame = %#v", wire.Frame)
	}
	if wire.Pointer == nil || wire.Pointer.Kind != ViewerPointerPressed || wire.Pointer.X != 300 || wire.Pointer.Y != 120 {
		t.Fatalf("wire pointer = %#v", wire.Pointer)
	}
	if wire.Action == nil || wire.Action.Summary != "text entered" {
		t.Fatalf("wire action = %#v", wire.Action)
	}
}

func TestViewerBridgeSessionIDIsRequired(t *testing.T) {
	if _, err := NewViewerBridge(ViewerSessionInfo{}); err == nil {
		t.Fatal("empty session ID was accepted")
	}
}
