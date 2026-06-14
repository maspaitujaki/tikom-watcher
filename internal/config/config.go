// Package config loads the committed YAML configuration: poll timing, optional
// active hours, browser settings, the detection defaults, and the watched-event
// list. The event list is CONFIG-OWNED (not in the DB); adding an event here and
// restarting is all it takes to start watching it.
//
// Secrets (bot token, DB DSN, admin chat id) come from the environment, never
// from this file — see cmd wiring (Phase 4/5).
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dimasfaid/tikom/internal/detect"
	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a Go duration string ("60s").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the whole config.yaml.
type Config struct {
	Poll              PollConfig    `yaml:"poll"`
	ActiveHours       *ActiveHours  `yaml:"active_hours"`
	Browser           BrowserConfig `yaml:"browser"`
	DetectionDefaults detect.Rules  `yaml:"detection_defaults"`
	Events            []Event       `yaml:"events"`
}

// PollConfig controls the poll loop and the state machine.
type PollConfig struct {
	Interval               Duration `yaml:"interval"`
	Jitter                 Duration `yaml:"jitter"`
	ConfirmDelay           Duration `yaml:"confirm_delay"`
	CooldownMinutes        int      `yaml:"cooldown_minutes"`
	UnknownStreakThreshold int      `yaml:"unknown_streak_threshold"`
}

// BrowserConfig is consumed by the browser layer in Phase 4.
type BrowserConfig struct {
	Timeout   Duration `yaml:"timeout"`
	UserAgent string   `yaml:"user_agent"`
}

// ActiveHours optionally restricts polling to a daily window in a timezone.
type ActiveHours struct {
	Timezone string `yaml:"timezone"`
	Start    string `yaml:"start"` // "HH:MM"
	End      string `yaml:"end"`   // "HH:MM"

	loc              *time.Location
	startMin, endMin int
}

// Event is one watched concert page. Key is the stable identity (history is
// keyed on it, never the URL, so a slug change does not reset state).
type Event struct {
	Key       string        `yaml:"key"`
	Name      string        `yaml:"name"`
	URL       string        `yaml:"url"`
	Detection *detect.Rules `yaml:"detection"` // optional per-event override
}

// Load reads, parses, defaults, and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Poll.Interval <= 0 {
		c.Poll.Interval = Duration(60 * time.Second)
	}
	if c.Poll.Jitter < 0 {
		c.Poll.Jitter = 0
	}
	if c.Poll.ConfirmDelay <= 0 {
		c.Poll.ConfirmDelay = Duration(5 * time.Second)
	}
	if c.Poll.CooldownMinutes <= 0 {
		c.Poll.CooldownMinutes = 30
	}
	if c.Poll.UnknownStreakThreshold <= 0 {
		c.Poll.UnknownStreakThreshold = 5
	}
	if c.Browser.Timeout <= 0 {
		c.Browser.Timeout = Duration(45 * time.Second)
	}
}

func (c *Config) validate() error {
	if c.DetectionDefaults.PageReadySelector == "" {
		return errors.New("config: detection_defaults.page_ready_selector is required")
	}
	if len(c.Events) == 0 {
		return errors.New("config: events list is empty")
	}
	seen := make(map[string]bool, len(c.Events))
	for i, e := range c.Events {
		if e.Key == "" {
			return fmt.Errorf("config: events[%d] missing key", i)
		}
		if e.URL == "" {
			return fmt.Errorf("config: event %q missing url", e.Key)
		}
		if seen[e.Key] {
			return fmt.Errorf("config: duplicate event key %q", e.Key)
		}
		seen[e.Key] = true
	}
	if c.ActiveHours != nil {
		if err := c.ActiveHours.parse(); err != nil {
			return fmt.Errorf("config: active_hours: %w", err)
		}
	}
	return nil
}

// Cooldown returns the configured re-alert cooldown.
func (c *Config) Cooldown() time.Duration {
	return time.Duration(c.Poll.CooldownMinutes) * time.Minute
}

// RulesFor returns the detection rules for an event: defaults overlaid with any
// non-empty fields from the event's optional override.
func (c *Config) RulesFor(e Event) detect.Rules {
	r := c.DetectionDefaults
	o := e.Detection
	if o == nil {
		return r
	}
	if o.PageReadySelector != "" {
		r.PageReadySelector = o.PageReadySelector
	}
	if o.SoldOut.CTASelector != "" {
		r.SoldOut.CTASelector = o.SoldOut.CTASelector
	}
	if len(o.SoldOut.TextAny) > 0 {
		r.SoldOut.TextAny = o.SoldOut.TextAny
	}
	// RequireDisabled is a bool; an explicit override is only meaningful when the
	// override block is present, so we take it as-is when an override exists.
	r.SoldOut.RequireDisabled = o.SoldOut.RequireDisabled
	if len(o.ChallengeMarkers) > 0 {
		r.ChallengeMarkers = o.ChallengeMarkers
	}
	return r
}

func (a *ActiveHours) parse() error {
	loc, err := time.LoadLocation(a.Timezone)
	if err != nil {
		return fmt.Errorf("bad timezone %q: %w", a.Timezone, err)
	}
	s, err := parseHHMM(a.Start)
	if err != nil {
		return fmt.Errorf("bad start: %w", err)
	}
	e, err := parseHHMM(a.End)
	if err != nil {
		return fmt.Errorf("bad end: %w", err)
	}
	a.loc, a.startMin, a.endMin = loc, s, e
	return nil
}

// Active reports whether t (in the configured timezone) is within [start, end).
// Windows that wrap past midnight (start > end) are supported.
func (a *ActiveHours) Active(t time.Time) bool {
	if a == nil || a.loc == nil {
		return true
	}
	lt := t.In(a.loc)
	cur := lt.Hour()*60 + lt.Minute()
	if a.startMin <= a.endMin {
		return cur >= a.startMin && cur < a.endMin
	}
	return cur >= a.startMin || cur < a.endMin // wraps midnight
}

func parseHHMM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}
