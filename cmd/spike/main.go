// Command spike is a THROWAWAY Phase-0 diagnostic.
//
// It loads the three reference tiket.com pages with a headless (then headful)
// Chrome via go-rod, reports whether we hit a Cloudflare/DataDome bot wall,
// probes the DOM for selector/state evidence, and saves rendered HTML fixtures.
//
// Delete cmd/spike and spike/out/ after Phase 0 is reviewed. The fixtures in
// testdata/fixtures/ are kept for Phase 1 unit tests.
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)

const (
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	chromeBin   = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	outDir      = "spike/out"
	fixturesDir = "testdata/fixtures"
	pageBudget  = 90 * time.Second
)

type target struct {
	key, fixture, url string
}

var targets = []target{
	{"bts-jakarta-day1", "sold_out_bts.html", "https://www.tiket.com/id-id/to-do/bts-jakarta-day1"},
	{"weeknd-gos-day2", "on_sale_weeknd.html", "https://www.tiket.com/id-id/to-do/theweekndinjakarta-generalonsaleday2"},
	{"lany-jakarta-2026", "on_sale_lany.html", "https://www.tiket.com/id-id/to-do/lany-soft-world-tour-in-jakarta-2026-29-oct-gos"},
}

var challengeMarkers = []string{
	"just a moment", "cf-chl", "challenge-platform", "attention required",
	"checking your browser", "captcha-delivery", "datadome", "geo.captcha",
}

const probeJS = `() => {
  const leaf = e => e.childElementCount === 0;
  const norm = e => ((e.innerText || e.textContent || '') + '').trim();
  const isDisabled = e => e.disabled === true || e.getAttribute('aria-disabled') === 'true' || e.hasAttribute('disabled');
  const testids = [...new Set([...document.querySelectorAll('[data-testid]')].map(e => e.getAttribute('data-testid')))];
  const buttons = [...document.querySelectorAll('button, [role=button]')]
    .map(e => ({ tag: e.tagName.toLowerCase(), text: norm(e).slice(0,60), testid: e.getAttribute('data-testid'), cls: (e.className||'').toString().slice(0,90), disabled: isDisabled(e) }))
    .filter(b => b.text.length > 0).slice(0,60);
  const signalEls = [...document.querySelectorAll('*')]
    .filter(e => leaf(e) && /^(Habis|Sold\s*Out|Beli|Pilih)/i.test(norm(e)))
    .map(e => ({ tag: e.tagName.toLowerCase(), text: norm(e).slice(0,40), testid: e.getAttribute('data-testid'), cls: (e.className||'').toString().slice(0,90), disabled: isDisabled(e) }))
    .slice(0,40);
  const body = document.body ? document.body.innerText : '';
  const count = re => (body.match(new RegExp(re,'gi'))||[]).length;
  return {
    title: document.title, url: location.href, bodyLen: body.length,
    testids, buttons, signalEls,
    signals: { Habis: count('Habis'), SoldOut: count('Sold\\s*Out'), Beli: count('Beli'), Pilih: count('Pilih'), Tiket: count('Tiket'), Rp: count('Rp') },
  };
}`

type elem struct {
	Tag      string  `json:"tag"`
	Text     string  `json:"text"`
	Testid   *string `json:"testid"`
	Cls      string  `json:"cls"`
	Disabled bool    `json:"disabled"`
}

type probe struct {
	Title     string         `json:"title"`
	URL       string         `json:"url"`
	BodyLen   int            `json:"bodyLen"`
	Testids   []string       `json:"testids"`
	Signals   map[string]int `json:"signals"`
	Buttons   []elem         `json:"buttons"`
	SignalEls []elem         `json:"signalEls"`
}

type result struct {
	mode     string
	title    string
	finalURL string
	html     string
	png      []byte
	markers  []string
	p        probe
	readable bool
	blocked  bool
	err      error
}

func newLauncher(headless bool) *launcher.Launcher {
	l := launcher.New().
		Headless(headless).
		Set("disable-blink-features", "AutomationControlled").
		Set("lang", "id-ID")
	if _, err := os.Stat(chromeBin); err == nil {
		l = l.Bin(chromeBin)
	}
	return l
}

func check(browser *rod.Browser, t target, mode string, dwell time.Duration) result {
	r := result{mode: mode}

	page, err := stealth.Page(browser)
	if err != nil {
		r.err = err
		return r
	}
	defer page.Close()
	page = page.Timeout(pageBudget)

	if err := page.SetUserAgent(&proto.NetworkSetUserAgentOverride{
		UserAgent:      userAgent,
		AcceptLanguage: "id-ID",
	}); err != nil {
		r.err = err
		return r
	}

	if err := page.Navigate(t.url); err != nil {
		r.err = err
		return r
	}
	_ = page.WaitLoad()
	_ = page.WaitDOMStable(2*time.Second, 0)
	if dwell > 0 {
		time.Sleep(dwell)
		_ = page.WaitDOMStable(1*time.Second, 0)
	}

	if html, err := page.HTML(); err == nil {
		r.html = html
		r.markers = findMarkers(html)
	} else {
		r.err = err
	}

	if obj, err := page.Eval(probeJS); err == nil {
		raw := obj.Value.JSON("", "")
		if jerr := json.Unmarshal([]byte(raw), &r.p); jerr != nil {
			log.Printf("[%s] probe unmarshal: %v", mode, jerr)
		}
	} else {
		log.Printf("[%s] probe eval: %v", mode, err)
	}
	r.title = r.p.Title
	r.finalURL = r.p.URL

	if png, err := page.Screenshot(true, nil); err == nil {
		r.png = png
	}

	sig := r.p.Signals
	contentful := r.p.BodyLen > 800 &&
		(sig["Tiket"] > 0 || sig["Beli"] > 0 || sig["Habis"] > 0 || sig["Rp"] > 0)
	titleBlocked := containsFold(r.title, "just a moment") ||
		containsFold(r.title, "attention required") ||
		containsFold(r.title, "access denied")
	r.readable = contentful && !titleBlocked
	r.blocked = !r.readable
	return r
}

func findMarkers(html string) []string {
	low := strings.ToLower(html)
	var found []string
	for _, m := range challengeMarkers {
		if strings.Contains(low, m) {
			found = append(found, m)
		}
	}
	return found
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), sub)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func dump(key string, r result) {
	base := filepath.Join(outDir, key+"__"+r.mode)
	if r.html != "" {
		writeFile(base+".html", []byte(r.html))
	}
	if len(r.png) > 0 {
		writeFile(base+".png", r.png)
	}
}

func writeFile(path string, b []byte) {
	if err := os.WriteFile(path, b, 0o644); err != nil {
		log.Printf("write %s: %v", path, err)
	}
}

func report(r result) {
	if r.err != nil {
		log.Printf("  [%s] ERROR: %v", r.mode, r.err)
		return
	}
	log.Printf("  [%s] title=%q bodyLen=%d readable=%v markers=%v",
		r.mode, r.title, r.p.BodyLen, r.readable, r.markers)
	log.Printf("  [%s] signals=%v", r.mode, r.p.Signals)
	log.Printf("  [%s] testids(%d)=%v", r.mode, len(r.p.Testids), r.p.Testids)
	for _, b := range r.p.Buttons {
		log.Printf("    btn  <%s testid=%q disabled=%v> %q  cls=%q",
			b.Tag, deref(b.Testid), b.Disabled, b.Text, b.Cls)
	}
	for _, e := range r.p.SignalEls {
		log.Printf("    sig  <%s testid=%q disabled=%v> %q  cls=%q",
			e.Tag, deref(e.Testid), e.Disabled, e.Text, e.Cls)
	}
}

func main() {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		log.Fatal(err)
	}

	headless := rod.New().ControlURL(newLauncher(true).MustLaunch()).MustConnect()
	defer headless.MustClose()

	var headful *rod.Browser
	getHeadful := func() *rod.Browser {
		if headful == nil {
			headful = rod.New().ControlURL(newLauncher(false).MustLaunch()).MustConnect()
		}
		return headful
	}
	defer func() {
		if headful != nil {
			_ = headful.Close()
		}
	}()

	blockedSaved := false
	for _, t := range targets {
		log.Printf("================ %s ================", t.key)
		log.Printf("url: %s", t.url)

		var attempts []result
		var chosen *result

		run := func(b *rod.Browser, mode string, dwell time.Duration) {
			r := check(b, t, mode, dwell)
			dump(t.key, r)
			report(r)
			attempts = append(attempts, r)
			if chosen == nil && r.readable {
				rr := r
				chosen = &rr
			}
		}

		run(headless, "headless_stealth", 0)
		if chosen == nil {
			run(headless, "headless_dwell", 8*time.Second)
		}
		if chosen == nil {
			run(getHeadful(), "headful", 8*time.Second)
		}

		if chosen != nil {
			writeFile(filepath.Join(fixturesDir, t.fixture), []byte(chosen.html))
			log.Printf(">>> SAVED fixture %s (mode=%s)", t.fixture, chosen.mode)
		} else if len(attempts) > 0 {
			last := attempts[len(attempts)-1]
			if !blockedSaved && last.html != "" {
				writeFile(filepath.Join(fixturesDir, "blocked_challenge.html"), []byte(last.html))
				blockedSaved = true
				log.Printf(">>> ALL MODES BLOCKED — saved blocked_challenge.html from %s", last.mode)
			} else {
				log.Printf(">>> ALL MODES BLOCKED for %s (no fixture saved)", t.key)
			}
		}
	}
	log.Printf("================ done ================")
}
