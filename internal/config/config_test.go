package config

import (
	"testing"
	"time"

	"github.com/dimasfaid/tikom/internal/detect"
)

func TestLoad_Example(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if c.Poll.Interval.D() != 60*time.Second {
		t.Errorf("interval = %v; want 60s", c.Poll.Interval.D())
	}
	if c.Cooldown() != 30*time.Minute {
		t.Errorf("cooldown = %v; want 30m", c.Cooldown())
	}
	if len(c.Events) == 0 {
		t.Fatalf("events = %d; want at least one", len(c.Events))
	}
	if c.Events[0].Key == "" || c.Events[0].URL == "" {
		t.Fatalf("first event missing key/url: %+v", c.Events[0])
	}
	if c.DetectionDefaults.PageReadySelector != `[data-testid="product-card"]` {
		t.Errorf("page_ready_selector = %q", c.DetectionDefaults.PageReadySelector)
	}
}

func TestRulesFor_OverrideOverlaysDefaults(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ev := c.Events[0]
	ev.Detection = &c.DetectionDefaults // start from a copy
	override := *ev.Detection
	override.SoldOut.TextAny = []string{"sold out"}
	ev.Detection = &override

	r := c.RulesFor(ev)
	if got := r.SoldOut.TextAny; len(got) != 1 || got[0] != "sold out" {
		t.Errorf("text_any = %v; want override [sold out]", got)
	}
	// Non-overridden field falls back to default.
	if r.PageReadySelector != c.DetectionDefaults.PageReadySelector {
		t.Errorf("page_ready_selector should fall back to default, got %q", r.PageReadySelector)
	}
}

func TestValidate_RejectsDuplicateKeysAndMissingFields(t *testing.T) {
	cases := []struct {
		name string
		c    Config
	}{
		{"no events", Config{DetectionDefaults: rulesWithSelector()}},
		{"missing url", Config{
			DetectionDefaults: rulesWithSelector(),
			Events:            []Event{{Key: "a"}},
		}},
		{"dup key", Config{
			DetectionDefaults: rulesWithSelector(),
			Events: []Event{
				{Key: "a", URL: "u1"}, {Key: "a", URL: "u2"},
			},
		}},
		{"no page_ready_selector", Config{
			Events: []Event{{Key: "a", URL: "u"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.c
			if err := c.validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestActiveHours(t *testing.T) {
	a := &ActiveHours{Timezone: "Asia/Jakarta", Start: "08:00", End: "23:00"}
	if err := a.parse(); err != nil {
		t.Fatal(err)
	}
	jkt := a.loc
	if a.Active(time.Date(2026, 6, 14, 7, 59, 0, 0, jkt)) {
		t.Error("07:59 should be inactive")
	}
	if !a.Active(time.Date(2026, 6, 14, 8, 0, 0, 0, jkt)) {
		t.Error("08:00 should be active")
	}
	if a.Active(time.Date(2026, 6, 14, 23, 0, 0, 0, jkt)) {
		t.Error("23:00 should be inactive (end-exclusive)")
	}

	// nil ActiveHours => always active.
	var none *ActiveHours
	if !none.Active(time.Now()) {
		t.Error("nil active_hours must be always-on")
	}
}

func rulesWithSelector() (r detect.Rules) {
	r.PageReadySelector = "x"
	return
}
