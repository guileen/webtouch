package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"uws/pkg/browser"
	"uws/pkg/matcher"
	"uws/pkg/vlm"
)

type chromeResourceSample struct {
	CPUPercent float64
	RSSKB      int64
	Processes  int
}

func sampleChromeTree(rootPIDs []int) (chromeResourceSample, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,%cpu=,rss=").Output()
	if err != nil {
		return chromeResourceSample{}, err
	}
	type proc struct {
		pid, ppid int
		cpu       float64
		rss       int64
	}
	procs := make(map[int]proc)
	children := make(map[int][]int)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		cpu, err3 := strconv.ParseFloat(fields[2], 64)
		rss, err4 := strconv.ParseInt(fields[3], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		procs[pid] = proc{pid: pid, ppid: ppid, cpu: cpu, rss: rss}
		children[ppid] = append(children[ppid], pid)
	}
	seen := make(map[int]bool)
	sample := chromeResourceSample{}
	var visit func(int)
	visit = func(pid int) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		if p, ok := procs[pid]; ok {
			sample.CPUPercent += p.cpu
			sample.RSSKB += p.rss
			sample.Processes++
		}
		for _, child := range children[pid] {
			visit(child)
		}
	}
	for _, pid := range rootPIDs {
		visit(pid)
	}
	if sample.Processes == 0 {
		return sample, fmt.Errorf("no live Chrome processes found for root PIDs %v", rootPIDs)
	}
	return sample, nil
}

func measureChromeTree(rootPIDs []int, interval time.Duration) (<-chan chromeResourceSample, func()) {
	samples := make(chan chromeResourceSample, 1)
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var peak chromeResourceSample
		for {
			select {
			case <-stop:
				select {
				case samples <- peak:
				default:
				}
				close(samples)
				return
			case <-ticker.C:
				sample, err := sampleChromeTree(rootPIDs)
				if err == nil {
					if sample.RSSKB > peak.RSSKB {
						peak.RSSKB = sample.RSSKB
					}
					if sample.CPUPercent > peak.CPUPercent {
						peak.CPUPercent = sample.CPUPercent
					}
					if sample.Processes > peak.Processes {
						peak.Processes = sample.Processes
					}
				}
			}
		}
	}()
	return samples, func() { once.Do(func() { close(stop) }) }
}

func TestSprintLongPageThroughput(t *testing.T) {
	if os.Getenv("UWS_RUN_SPRINT_THROUGHPUT") != "1" {
		t.Skip("set UWS_RUN_SPRINT_THROUGHPUT=1 to run local long-page throughput measurement")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var b strings.Builder
		b.WriteString(`<!doctype html><html><head><style>body{margin:0;height:10000px;background:#f5f5f5}.mark{position:absolute;left:80px;width:240px;height:72px;background:#2463eb}</style></head><body>`)
		for _, y := range []int{500, 2600, 5200, 8100} {
			fmt.Fprintf(&b, `<div class="mark" style="top:%dpx"></div>`, y)
		}
		b.WriteString(`</body></html>`)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer fixture.Close()

	for _, concurrency := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("concurrency_%d", concurrency), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			pool := browser.NewPool(concurrency, &browser.CDPFactory{Headless: true})
			contexts := make([]*browser.BrowserContext, 0, concurrency)
			defer func() {
				for _, bc := range contexts {
					pool.Release(bc.ID)
				}
			}()
			for i := 0; i < concurrency; i++ {
				bc, err := pool.Acquire(ctx, browser.Config{})
				if err != nil {
					t.Fatalf("acquire session %d: %v", i, err)
				}
				contexts = append(contexts, bc)
				if err := bc.Session().Navigate(ctx, fixture.URL); err != nil {
					t.Fatalf("navigate local fixture: %v", err)
				}
			}
			candidates := []matcher.Candidate{
				{W: 240, H: 72, DocumentX: 80, DocumentY: 500},
				{W: 240, H: 72, DocumentX: 80, DocumentY: 2600},
				{W: 240, H: 72, DocumentX: 80, DocumentY: 5200},
				{W: 240, H: 72, DocumentX: 80, DocumentY: 8100},
			}
			capture := func(bc *browser.BrowserContext) error {
				longPage, ok := bc.Session().(browser.LongPageScreenshotter)
				if !ok {
					return fmt.Errorf("session does not support long-page capture")
				}
				blocks, panels, err := captureDocumentCandidatePanels(ctx, longPage, candidates)
				if err != nil {
					return err
				}
				if len(blocks) != 4 || len(panels) != 4 {
					return fmt.Errorf("captured blocks=%d panels=%d, want four", len(blocks), len(panels))
				}
				montage, w, h, err := vlm.BuildImagePanelMontage(panels)
				if err != nil {
					return err
				}
				if len(montage) == 0 || w == 0 || h == 0 {
					return fmt.Errorf("invalid montage %dx%d bytes=%d", w, h, len(montage))
				}
				return nil
			}
			for _, bc := range contexts {
				if err := capture(bc); err != nil {
					t.Fatalf("warm-up capture: %v", err)
				}
			}
			rootPIDs := make([]int, 0, len(contexts))
			for _, bc := range contexts {
				processInfo, ok := bc.Session().(browser.BrowserProcessInfo)
				if !ok || processInfo.BrowserPID() <= 0 {
					t.Fatal("headless test browser did not expose its process PID")
				}
				rootPIDs = append(rootPIDs, processInfo.BrowserPID())
			}
			resourceSamples, stopSampling := measureChromeTree(rootPIDs, 200*time.Millisecond)
			defer stopSampling()
			for round := 1; round <= 10; round++ {
				started := time.Now()
				errs := make(chan error, concurrency)
				var wg sync.WaitGroup
				for _, bc := range contexts {
					wg.Add(1)
					go func(bc *browser.BrowserContext) {
						defer wg.Done()
						errs <- capture(bc)
					}(bc)
				}
				wg.Wait()
				close(errs)
				failed := 0
				for err := range errs {
					if err != nil {
						failed++
						t.Errorf("round %d capture: %v", round, err)
					}
				}
				elapsed := time.Since(started)
				successful := concurrency - failed
				jobsPerMinute := float64(successful) * float64(time.Minute) / float64(elapsed)
				t.Logf("round=%d sessions=%d tasks=%d failed=%d wall_ms=%d successful_tasks_per_minute=%.2f", round, concurrency, concurrency, failed, elapsed.Milliseconds(), jobsPerMinute)
			}
			stopSampling()
			peak, ok := <-resourceSamples
			if !ok || peak.Processes == 0 {
				t.Fatal("Chrome resource sampler produced no samples")
			}
			t.Logf("chrome_tree_roots=%v sample_interval_ms=200 peak_sampled_lifetime_avg_cpu_percent_sum=%.1f peak_rss_mib=%.1f peak_processes=%d", rootPIDs, peak.CPUPercent, float64(peak.RSSKB)/1024, peak.Processes)
		})
	}
}
