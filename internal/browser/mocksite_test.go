package browser

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dimasfaid/tikom/internal/detect"
	"github.com/dimasfaid/tikom/internal/mocksite"
)

// Drives the real browser + detection stack against the mock site, proving the
// SOLD_OUT -> AVAILABLE flip is detected (the core of the DEMO.md flow).
func TestRender_MockSite_FlipDetected(t *testing.T) {
	bin := chromeBin()
	if bin == "" {
		t.Skip("no local Chrome/Chromium found")
	}
	ms := mocksite.New("demo")
	srv := httptest.NewServer(ms.Handler())
	t.Cleanup(srv.Close)

	b, err := New(Config{Timeout: 15 * time.Second, BinPath: bin, Headless: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	ctx := context.Background()
	url := srv.URL + "/to-do/demo"
	rules := detect.RecommendedRules()

	if r := detect.Detect(ctx, b, url, rules); r.State != detect.StateSoldOut {
		t.Fatalf("initial: got %s (%s); want SOLD_OUT", r.State, r.Reason)
	}

	ms.Set("demo", mocksite.StateAvailable)

	if r := detect.Detect(ctx, b, url, rules); r.State != detect.StateAvailable {
		t.Fatalf("after flip: got %s (%s); want AVAILABLE (span-'Habis' trap must be ignored)", r.State, r.Reason)
	}
}
