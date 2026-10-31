package qa

import (
	"testing"
)

func TestParseViewports(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{"empty returns default 3", "", 3, false},
		{"named viewports", "mobile,tablet,desktop", 3, false},
		{"single width", "390", 1, false},
		{"multiple widths", "390,768,1440", 3, false},
		{"explicit WxH", "375x667,1920x1080", 2, false},
		{"mixed alias and WxH", "mobile, 800x600", 2, false},
		{"invalid format", "abc", 0, true},
		{"invalid dimension", "390x-10", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseViewports(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseViewports(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && len(got) != tt.wantLen {
				t.Errorf("ParseViewports(%q) len = %d, want %d", tt.input, len(got), tt.wantLen)
			}
		})
	}

	// 细粒度校验 390
	vps, err := ParseViewports("390")
	if err != nil {
		t.Fatal(err)
	}
	if vps[0].Width != 390 || vps[0].Height != 844 || !vps[0].Mobile {
		t.Errorf("Unexpected viewport parsed from 390: %+v", vps[0])
	}

	// 细粒度校验 375x667
	vps2, err := ParseViewports("375x667")
	if err != nil {
		t.Fatal(err)
	}
	if vps2[0].Width != 375 || vps2[0].Height != 667 || !vps2[0].Mobile {
		t.Errorf("Unexpected viewport parsed from 375x667: %+v", vps2[0])
	}
}

func TestDefaultViewports(t *testing.T) {
	vps := DefaultViewports()
	if len(vps) != 3 {
		t.Fatalf("expected 3 default viewports, got %d", len(vps))
	}
	expected := []string{"mobile", "tablet", "desktop"}
	for i, vp := range vps {
		if vp.Name != expected[i] {
			t.Errorf("viewport[%d].Name = %s, want %s", i, vp.Name, expected[i])
		}
	}
}
