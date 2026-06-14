// Package detect decides whether a tiket.com event page reads as AVAILABLE,
// SOLD_OUT, or UNKNOWN.
//
// The heart of the package is Classify: a pure, browser-free function over a
// rendered HTML document. Keeping it browser-free makes it fast and trivially
// unit-testable against saved fixtures. The live, go-rod-backed rendering that
// feeds it HTML is provided via the Renderer interface and implemented in the
// browser layer (Phase 4); Detect glues the two together.
//
// Detection is EAGER and config-driven (see Rules). Precedence:
//  1. Purchase section never rendered / challenge / render failure -> UNKNOWN
//     (a failed load is NEVER reported as AVAILABLE or SOLD_OUT).
//  2. Page rendered AND the sold-out rule matches                  -> SOLD_OUT.
//  3. Page rendered AND the sold-out rule does not match           -> AVAILABLE.
package detect

import (
	"context"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// State is the detection outcome for an event page.
type State string

const (
	StateUnknown   State = "UNKNOWN"
	StateSoldOut   State = "SOLD_OUT"
	StateAvailable State = "AVAILABLE"
)

// Rules is the config-driven detection spec (defaults + per-event override come
// from config.yaml in Phase 5). Nothing about the decision is hardcoded here.
type Rules struct {
	// PageReadySelector must target the ticket/purchase SECTION specifically,
	// so a pre-sale/error/challenge page with no purchase section falls to
	// UNKNOWN rather than a false AVAILABLE. If it is absent from the rendered
	// DOM, the state is UNKNOWN.
	PageReadySelector string `yaml:"page_ready_selector" json:"page_ready_selector"`

	// SoldOut is the rule that positively identifies a sold-out page.
	SoldOut SoldOutRule `yaml:"sold_out" json:"sold_out"`

	// ChallengeMarkers only enrich the UNKNOWN reason string when the purchase
	// section is absent (for clearer "site may be blocked" logging). They never
	// change the State and are never consulted on a rendered page, so the
	// false-positive markers seen on normal pages (Phase 0) cannot mislead us.
	ChallengeMarkers []string `yaml:"challenge_markers" json:"challenge_markers"`
}

// SoldOutRule scopes the sold-out check to the primary purchase control(s).
// Scoping is essential: the page-wide text "Habis" appears even on on-sale
// pages (per-category badges), so a whole-page text search would false-positive.
type SoldOutRule struct {
	// CTASelector selects the candidate purchase control(s). Defaults to
	// "button" — which, on tiket.com, excludes the <span> "Terjual habis"
	// category badges and is independent of fragile CSS-module class hashes.
	CTASelector string `yaml:"cta_selector" json:"cta_selector"`

	// TextAny: a CTA matches if its (case-insensitive) text contains any of
	// these substrings, e.g. ["terjual habis","habis","sold out"].
	TextAny []string `yaml:"text_any" json:"text_any"`

	// RequireDisabled: a matching CTA must also be disabled (disabled attr or
	// aria-disabled="true").
	RequireDisabled bool `yaml:"require_disabled" json:"require_disabled"`
}

// Result is a state plus a human-readable reason (handy for slog + the Phase-3
// unknown_streak warning).
type Result struct {
	State  State
	Reason string
}

// Renderer renders url to fully-loaded HTML, waiting for pageReadySelector to
// appear. It must map a load timeout / missing selector / bot challenge to a
// non-nil error so the caller resolves them to UNKNOWN. The go-rod
// implementation lives in the browser layer (Phase 4).
type Renderer interface {
	Render(ctx context.Context, url, pageReadySelector string) (html string, err error)
}

// Detect renders url and classifies it. Any render failure is UNKNOWN.
func Detect(ctx context.Context, r Renderer, url string, rules Rules) Result {
	html, err := r.Render(ctx, url, rules.PageReadySelector)
	if err != nil {
		return Result{StateUnknown, "render failed: " + err.Error()}
	}
	return Classify(html, rules)
}

// Classify applies the eager precedence to an already-rendered HTML document.
func Classify(html string, r Rules) Result {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return Result{StateUnknown, "html parse error: " + err.Error()}
	}
	if strings.TrimSpace(r.PageReadySelector) == "" {
		return Result{StateUnknown, "no page_ready_selector configured"}
	}

	// (1) Purchase section never rendered -> UNKNOWN.
	if doc.Find(r.PageReadySelector).Length() == 0 {
		if m := firstMarker(html, r.ChallengeMarkers); m != "" {
			return Result{StateUnknown, "challenge/interstitial page (marker: " + m + ")"}
		}
		return Result{StateUnknown, "purchase section not rendered: " + r.PageReadySelector}
	}

	// (2) Rendered AND sold-out rule matches -> SOLD_OUT.
	if cta, ok := matchSoldOut(doc, r.SoldOut); ok {
		return Result{StateSoldOut, "sold-out CTA matched: " + cta}
	}

	// (3) Rendered AND no sold-out match -> AVAILABLE (eager default).
	return Result{StateAvailable, "purchase section rendered; no sold-out CTA matched"}
}

// matchSoldOut returns the matched CTA text and true if any CTA satisfies the
// rule. With nothing to match on (no text and disabled not required) it never
// declares sold-out, so the AVAILABLE default holds.
func matchSoldOut(doc *goquery.Document, r SoldOutRule) (string, bool) {
	sel := strings.TrimSpace(r.CTASelector)
	if sel == "" {
		sel = "button"
	}
	if len(r.TextAny) == 0 && !r.RequireDisabled {
		return "", false
	}

	matched := false
	matchedText := ""
	doc.Find(sel).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if r.RequireDisabled && !isDisabled(s) {
			return true // keep looking
		}
		text := strings.TrimSpace(s.Text())
		if len(r.TextAny) > 0 && !containsAnyFold(text, r.TextAny) {
			return true // keep looking
		}
		matched = true
		matchedText = text
		return false // stop
	})
	if matched && matchedText == "" {
		matchedText = "(disabled " + sel + ")"
	}
	return matchedText, matched
}

func isDisabled(s *goquery.Selection) bool {
	if _, ok := s.Attr("disabled"); ok {
		return true
	}
	if v, ok := s.Attr("aria-disabled"); ok && strings.EqualFold(strings.TrimSpace(v), "true") {
		return true
	}
	return false
}

func containsAnyFold(s string, subs []string) bool {
	ls := strings.ToLower(s)
	for _, sub := range subs {
		sub = strings.ToLower(strings.TrimSpace(sub))
		if sub != "" && strings.Contains(ls, sub) {
			return true
		}
	}
	return false
}

func firstMarker(html string, markers []string) string {
	lh := strings.ToLower(html)
	for _, m := range markers {
		m = strings.ToLower(strings.TrimSpace(m))
		if m != "" && strings.Contains(lh, m) {
			return m
		}
	}
	return ""
}

// RecommendedRules is the detection config derived from the Phase-0 spike for
// tiket.com /to-do/ pages. In production these values come from config.yaml
// (Phase 5); this constructor documents the proven defaults and backs the tests.
func RecommendedRules() Rules {
	return Rules{
		PageReadySelector: `[data-testid="product-card"]`,
		SoldOut: SoldOutRule{
			CTASelector:     "button",
			TextAny:         []string{"terjual habis", "habis", "sold out"},
			RequireDisabled: true,
		},
		ChallengeMarkers: []string{
			"just a moment", "checking your browser", "challenge-platform",
			"cf-chl", "captcha-delivery", "attention required",
		},
	}
}
