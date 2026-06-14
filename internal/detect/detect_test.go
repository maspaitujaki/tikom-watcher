package detect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// Classify against the real Phase-0 fixtures, covering all three states plus
// the synthetic challenge page.
func TestClassify_Fixtures(t *testing.T) {
	rules := RecommendedRules()
	cases := []struct {
		name    string
		fixture string
		want    State
	}{
		{"sold out: BTS, disabled 'Terjual habis' CTA", "sold_out_bts.html", StateSoldOut},
		{"on sale: Weeknd, enabled 'Beli' CTA + span-habis trap", "on_sale_weeknd.html", StateAvailable},
		{"on sale: LANY", "on_sale_lany.html", StateAvailable},
		{"challenge: no purchase section", "blocked_challenge.html", StateUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(loadFixture(t, c.fixture), rules)
			if got.State != c.want {
				t.Fatalf("state = %s; want %s (reason: %s)", got.State, c.want, got.Reason)
			}
			t.Logf("%s -> %s (%s)", c.fixture, got.State, got.Reason)
		})
	}
}

// The decisive regression guard: on-sale pages embed <span>Terjual habis</span>
// per-category badges. A page-wide text match would wrongly flag SOLD_OUT;
// scoping to the <button> CTA must keep them AVAILABLE.
func TestClassify_HabisTrap_OnSaleStaysAvailable(t *testing.T) {
	rules := RecommendedRules()
	for _, f := range []string{"on_sale_weeknd.html", "on_sale_lany.html"} {
		got := Classify(loadFixture(t, f), rules)
		if got.State != StateAvailable {
			t.Errorf("%s: got %s; want AVAILABLE (reason: %s)", f, got.State, got.Reason)
		}
	}
}

func TestClassify_ChallengeReasonLabelled(t *testing.T) {
	got := Classify(loadFixture(t, "blocked_challenge.html"), RecommendedRules())
	if got.State != StateUnknown {
		t.Fatalf("state = %s; want UNKNOWN", got.State)
	}
	if !strings.Contains(got.Reason, "challenge") {
		t.Errorf("reason = %q; want it to mention 'challenge'", got.Reason)
	}
}

// A page that renders an enabled "Beli" button but NO ticket section must be
// UNKNOWN, not AVAILABLE — proving page-ready targets the section specifically.
func TestClassify_NoPurchaseSection_Unknown(t *testing.T) {
	html := `<html><body><div class="event"><h1>Coming soon</h1>
		<p>Tiket akan segera tersedia.</p>
		<button>Beli tiket sekarang</button></div></body></html>`
	got := Classify(html, RecommendedRules())
	if got.State != StateUnknown {
		t.Fatalf("state = %s; want UNKNOWN (reason: %s)", got.State, got.Reason)
	}
}

func TestClassify_NoSelectorConfigured_Unknown(t *testing.T) {
	html := `<html><body><div data-testid="product-card"></div></body></html>`
	got := Classify(html, Rules{}) // empty rules -> no page_ready_selector
	if got.State != StateUnknown {
		t.Fatalf("state = %s; want UNKNOWN", got.State)
	}
}

// A disabled CTA with no text still counts as sold-out (require_disabled alone).
func TestClassify_DisabledCTANoText_SoldOut(t *testing.T) {
	html := `<html><body><div data-testid="product-card"></div>
		<button disabled></button></body></html>`
	rules := Rules{
		PageReadySelector: `[data-testid="product-card"]`,
		SoldOut:           SoldOutRule{CTASelector: "button", RequireDisabled: true},
	}
	if got := Classify(html, rules); got.State != StateSoldOut {
		t.Fatalf("state = %s; want SOLD_OUT (reason: %s)", got.State, got.Reason)
	}
}

// aria-disabled is honoured as disabled.
func TestClassify_AriaDisabled_SoldOut(t *testing.T) {
	html := `<html><body><div data-testid="product-card"></div>
		<button aria-disabled="true">Terjual habis</button></body></html>`
	if got := Classify(html, RecommendedRules()); got.State != StateSoldOut {
		t.Fatalf("state = %s; want SOLD_OUT (reason: %s)", got.State, got.Reason)
	}
}

// --- Detect (Renderer-backed) ---

type fakeRenderer struct {
	html string
	err  error
}

func (f fakeRenderer) Render(_ context.Context, _, _ string) (string, error) {
	return f.html, f.err
}

func TestDetect_RenderError_Unknown(t *testing.T) {
	got := Detect(context.Background(), fakeRenderer{err: errors.New("nav timeout")},
		"https://x", RecommendedRules())
	if got.State != StateUnknown {
		t.Fatalf("render error must be UNKNOWN; got %s (%s)", got.State, got.Reason)
	}
}

func TestDetect_FixturesThroughRenderer(t *testing.T) {
	cases := []struct {
		fixture string
		want    State
	}{
		{"sold_out_bts.html", StateSoldOut},
		{"on_sale_weeknd.html", StateAvailable},
		{"blocked_challenge.html", StateUnknown},
	}
	for _, c := range cases {
		got := Detect(context.Background(),
			fakeRenderer{html: loadFixture(t, c.fixture)}, "https://x", RecommendedRules())
		if got.State != c.want {
			t.Errorf("%s: got %s; want %s (%s)", c.fixture, got.State, c.want, got.Reason)
		}
	}
}
