package browser

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dimasfaid/tikom/internal/detect"
)

// TestLive_DetectRealTiketPages drives the real browser + detection stack
// against live tiket.com pages. Gated behind TIKOM_LIVE=1 (needs internet +
// Chrome) so it is skipped in normal runs.
//
//	TIKOM_LIVE=1 go test ./internal/browser/ -run Live -v
func TestLive_DetectRealTiketPages(t *testing.T) {
	if os.Getenv("TIKOM_LIVE") == "" {
		t.Skip("set TIKOM_LIVE=1 to run live tiket.com detection")
	}
	bin := chromeBin()
	if bin == "" {
		t.Skip("no local Chrome/Chromium found")
	}

	b, err := New(Config{Timeout: 45 * time.Second, BinPath: bin, Headless: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() { _ = b.Close() }()

	rules := detect.RecommendedRules()
	cases := []struct {
		url  string
		want detect.State
	}{
		{"https://www.tiket.com/id-id/to-do/bts-jakarta-day1", detect.StateSoldOut},
		{"https://www.tiket.com/id-id/to-do/theweekndinjakarta-generalonsaleday2", detect.StateAvailable},
		{"https://www.tiket.com/id-id/to-do/lany-soft-world-tour-in-jakarta-2026-29-oct-gos", detect.StateAvailable},
	}
	for _, c := range cases {
		res := detect.Detect(context.Background(), b, c.url, rules)
		t.Logf("%s -> %s (%s)", c.url, res.State, res.Reason)
		if res.State != c.want {
			t.Errorf("%s: got %s; want %s", c.url, res.State, c.want)
		}
	}
}
