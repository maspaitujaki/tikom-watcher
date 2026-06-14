package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// chromeBin finds a usable Chrome/Chromium, or "" (no network to download in CI).
func chromeBin() string {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

const pageHTML = `<!doctype html><html><body>
<div data-testid="product-card"><button>Beli tiket sekarang</button></div>
</body></html>`

func newTestBrowser(t *testing.T) (*Browser, *httptest.Server) {
	t.Helper()
	bin := chromeBin()
	if bin == "" {
		t.Skip("no local Chrome/Chromium found; skipping live browser test")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(pageHTML))
	}))
	t.Cleanup(srv.Close)

	b, err := New(Config{Timeout: 15 * time.Second, BinPath: bin, Headless: true})
	if err != nil {
		t.Fatalf("launch browser: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, srv
}

func TestRender_ReturnsRenderedHTML(t *testing.T) {
	b, srv := newTestBrowser(t)

	html, err := b.Render(context.Background(), srv.URL, `[data-testid="product-card"]`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(html, "product-card") || !strings.Contains(html, "Beli tiket sekarang") {
		t.Fatalf("rendered html missing expected content:\n%s", html)
	}
}

func TestRender_MissingSelector_PageNotReady(t *testing.T) {
	b, srv := newTestBrowser(t)

	_, err := b.Render(context.Background(), srv.URL, `[data-testid="not-here"]`)
	if !errors.Is(err, ErrPageNotReady) {
		t.Fatalf("err = %v; want ErrPageNotReady", err)
	}
}

// Kill the underlying browser out from under the renderer; the next Render must
// transparently relaunch and succeed (crash recovery).
func TestRender_RecreatesAfterCrash(t *testing.T) {
	b, srv := newTestBrowser(t)
	sel := `[data-testid="product-card"]`

	if _, err := b.Render(context.Background(), srv.URL, sel); err != nil {
		t.Fatalf("first render: %v", err)
	}

	b.mu.Lock()
	_ = b.rod.Close() // simulate a crash: connection/process gone
	b.mu.Unlock()

	html, err := b.Render(context.Background(), srv.URL, sel)
	if err != nil {
		t.Fatalf("render after crash should recover: %v", err)
	}
	if !strings.Contains(html, "product-card") {
		t.Fatalf("recovered render missing content:\n%s", html)
	}
}
