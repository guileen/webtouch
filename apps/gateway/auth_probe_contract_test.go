package gateway

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the actual probe and serialize its return value as CDP does. A fake
// engine that returns an ideal object directly cannot catch double encoding.
func TestAuthProbeCDPValueContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute the browser-side probe")
	}
	for _, tc := range []struct{ name, text, want string }{
		{"composer", "Welcome", "logged_in"},
		{"login", "Sign in", "logged_out"},
		{"challenge", "Verify you are human", "challenge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.text)
			script := `const input={offsetWidth:100,offsetHeight:20,disabled:false,getAttribute:()=>null};
global.document={body:{innerText:` + string(body) + `},querySelectorAll:(s)=>s.startsWith('textarea')?[input]:[]};
process.stdout.write(JSON.stringify(` + authProbeScript + `));`
			raw, err := exec.Command(node, "-e", script).CombinedOutput()
			if err != nil {
				t.Fatalf("execute probe: %v: %s", err, raw)
			}
			var result struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(raw, &result); err != nil || result.Status != tc.want {
				t.Fatalf("CDP value must be an object with status %s, got %s (%v)", tc.want, raw, err)
			}
			engine := newFakeEngine()
			engine.evaluateFunc = func(req ActionRequest) (ActionResult, error) {
				if !strings.Contains(req.Value, "__uws_auth_probe") {
					t.Fatal("unexpected script")
				}
				return ActionResult{Success: true, ExtractedData: string(raw)}, nil
			}
			gw := newAuthStateTestGateway(t, engine, filepath.Join(t.TempDir(), "auth.json"))
			ch := &gw.cfg.Channels[0]
			if _, err := gw.ensureSession(context.Background(), ch, "probe-contract", ""); err != nil {
				t.Fatal(err)
			}
			err = gw.ensureAuthenticated(context.Background(), ch, "probe-contract", "")
			if (err == nil) != (tc.want == "logged_in") {
				t.Fatalf("status %s: error=%v", tc.want, err)
			}
			if got := string(gw.authSnapshot(ch.ID).Status); got != tc.want {
				t.Fatalf("status=%s want=%s", got, tc.want)
			}
		})
	}
}
