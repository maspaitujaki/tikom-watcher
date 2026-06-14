// Package browser is the reliability layer around headless Chrome. It owns ONE
// reused rod browser instance, renders each check on a fresh page with a
// per-check timeout, recreates the browser if it crashes, and retries transient
// failures with backoff. It implements detect.Renderer.
package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)

const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// ErrPageNotReady means the page-ready selector never appeared within the
// per-check timeout. It is determinate (not retried) and the caller maps it to
// UNKNOWN.
var ErrPageNotReady = errors.New("page-ready selector did not appear")

// Config configures the browser.
type Config struct {
	Timeout    time.Duration // per-check budget (navigate + wait + html)
	UserAgent  string        // defaults to a desktop Chrome UA
	BinPath    string        // explicit Chrome/headless-shell binary; empty = rod-managed
	Headless   bool
	MaxRetries int // transient retries per render; defaults to 2
	Logger     *slog.Logger
}

// Browser owns a single reused rod browser instance.
type Browser struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	rod     *rod.Browser
	launch  *launcher.Launcher
}

// New launches the initial browser instance.
func New(cfg Config) (*Browser, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 45 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	b := &Browser{cfg: cfg, log: cfg.Logger}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.launchLocked(); err != nil {
		return nil, err
	}
	return b, nil
}

// Render implements detect.Renderer. It renders url to fully-loaded HTML,
// waiting for pageReadySelector, retrying transient errors with backoff and
// recreating the browser if it has crashed.
func (b *Browser) Render(ctx context.Context, url, pageReadySelector string) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= b.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if !b.healthy() {
				b.log.Warn("browser unhealthy; recreating", "url", url)
				b.reset()
			}
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return "", err
			}
			b.log.Warn("render retry", "url", url, "attempt", attempt, "err", lastErr)
		}

		html, err := b.renderOnce(ctx, url, pageReadySelector)
		if err == nil {
			return html, nil
		}
		if errors.Is(err, ErrPageNotReady) {
			return "", err // determinate -> UNKNOWN, do not retry
		}
		lastErr = err
	}
	return "", fmt.Errorf("render %s: %w", url, lastErr)
}

func (b *Browser) renderOnce(ctx context.Context, url, sel string) (string, error) {
	rb, err := b.instance()
	if err != nil {
		return "", fmt.Errorf("acquire browser: %w", err)
	}

	cctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	page, err := stealth.Page(rb)
	if err != nil {
		return "", fmt.Errorf("new page: %w", err) // often a sign the browser died
	}
	defer func() { _ = page.Close() }()
	page = page.Context(cctx)

	if b.cfg.UserAgent != "" {
		if err := page.SetUserAgent(&proto.NetworkSetUserAgentOverride{
			UserAgent:      b.cfg.UserAgent,
			AcceptLanguage: "id-ID",
		}); err != nil {
			return "", fmt.Errorf("set user-agent: %w", err)
		}
	}
	if err := page.Navigate(url); err != nil {
		return "", fmt.Errorf("navigate: %w", err)
	}
	_ = page.WaitLoad() // best-effort; the selector wait below is authoritative

	if _, err := page.Element(sel); err != nil {
		// Selector never appeared within the timeout (or the wait was cut short).
		return "", ErrPageNotReady
	}
	_ = page.WaitDOMStable(time.Second, 0)

	html, err := page.HTML()
	if err != nil {
		return "", fmt.Errorf("read html: %w", err)
	}
	return html, nil
}

// instance returns the live browser, launching one if needed.
func (b *Browser) instance() (*rod.Browser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rod == nil {
		if err := b.launchLocked(); err != nil {
			return nil, err
		}
	}
	return b.rod, nil
}

func (b *Browser) launchLocked() error {
	l := launcher.New().
		Headless(b.cfg.Headless).
		Set("disable-blink-features", "AutomationControlled").
		Set("lang", "id-ID").
		Set("no-sandbox").             // required when running as root in a container
		Set("disable-dev-shm-usage").  // avoid /dev/shm exhaustion in containers
		Set("disable-gpu")
	if b.cfg.BinPath != "" {
		l = l.Bin(b.cfg.BinPath)
	}
	ctrlURL, err := l.Launch()
	if err != nil {
		return fmt.Errorf("launch chrome: %w", err)
	}
	rb := rod.New().ControlURL(ctrlURL)
	if err := rb.Connect(); err != nil {
		l.Cleanup()
		return fmt.Errorf("connect chrome: %w", err)
	}
	b.rod = rb
	b.launch = l
	b.log.Info("browser launched", "headless", b.cfg.Headless)
	return nil
}

// healthy reports whether the current browser still responds to CDP.
func (b *Browser) healthy() bool {
	b.mu.Lock()
	rb := b.rod
	b.mu.Unlock()
	if rb == nil {
		return false
	}
	_, err := rb.Pages()
	return err == nil
}

// reset tears down the current (presumed dead) browser so the next render
// relaunches a fresh one.
func (b *Browser) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.teardownLocked()
}

func (b *Browser) teardownLocked() {
	if b.rod != nil {
		_ = b.rod.Close()
		b.rod = nil
	}
	if b.launch != nil {
		b.launch.Cleanup()
		b.launch = nil
	}
}

// Close shuts down the browser. Safe to call once during graceful shutdown.
func (b *Browser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.teardownLocked()
	return nil
}

func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond * time.Duration(1<<(attempt-1)) // 500ms, 1s, 2s, ...
	if max := 5 * time.Second; d > max {
		d = max
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
