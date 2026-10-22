package main

import "testing"

// TestNormalizeClampsClipboardIntervalToFloor covers the clipboard polling
// period.
//
// The old normaliser reset anything below 200ms to the 800ms default, so a user
// who typed 100 to poll faster got a poll eight times slower instead — silently.
// The floor is what the input promises: too-small values are raised to it, and
// values at or above it are left alone (the 10s ceiling still applies).
func TestNormalizeClampsClipboardIntervalToFloor(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{100, 200},
		{199, 200},
		{200, 200},
		{800, 800}, // the shipped default must survive normalising untouched
		{10001, 10000},
	}
	for _, c := range cases {
		cfg := DefaultConfig()
		cfg.ClipboardIntervalMs = c.in
		cfg.normalize()
		if cfg.ClipboardIntervalMs != c.want {
			t.Errorf("normalize(%dms) = %dms, want %dms",
				c.in, cfg.ClipboardIntervalMs, c.want)
		}
	}

	// Clamping a hand-edited value must not move the default a fresh install
	// starts with.
	if got := DefaultConfig().ClipboardIntervalMs; got != 800 {
		t.Errorf("DefaultConfig().ClipboardIntervalMs = %d, want 800", got)
	}
}

// TestNormalizeDefaultsResumeFlushPercentWhenEnabled covers the "按百分比保存"
// trigger.
//
// The engine reads SavePercent=0 as "off" (internal/engine download.go), so an
// enabled trigger with an empty/cleared field was silently doing nothing: the
// checkbox said the transfer would be flushed by percentage while the engine
// never saw a percentage. normalize fills in the 10% default for an enabled
// trigger; an unticked switch keeps 0 as an explicit "off".
func TestNormalizeDefaultsResumeFlushPercentWhenEnabled(t *testing.T) {
	cases := []struct {
		name  string
		byPct bool
		pct   int
		want  int
	}{
		{"enabled with 0 gets the default", true, 0, 10},
		{"enabled with a value keeps it", true, 25, 25},
		{"enabled with a negative value is clamped then defaulted", true, -5, 10},
		{"disabled with 0 stays off", false, 0, 0},
		{"disabled keeps a leftover value", false, 25, 25},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ResumeFlushByPct = c.byPct
			cfg.ResumeFlushPct = c.pct
			cfg.normalize()
			if cfg.ResumeFlushPct != c.want {
				t.Errorf("normalize(byPct=%v, pct=%d) = %d, want %d",
					c.byPct, c.pct, cfg.ResumeFlushPct, c.want)
			}
		})
	}

	// A pct-only configuration must now really persist. With the value filled
	// in, the "at least one trigger" rescue must not have to force the time
	// trigger back on; before the fallback, this combination was saved as
	// "only the time trigger", which is not what the switches said.
	cfg := DefaultConfig()
	cfg.ResumeFlushBySize = false
	cfg.ResumeFlushByTime = false
	cfg.ResumeFlushByPct = true
	cfg.ResumeFlushPct = 0
	cfg.normalize()
	if cfg.ResumeFlushPct != 10 {
		t.Errorf("pct-only: ResumeFlushPct = %d, want the 10%% default", cfg.ResumeFlushPct)
	}
	if cfg.ResumeFlushByTime {
		t.Error("pct-only: the time trigger was forced back on although the pct trigger now has a value")
	}
	if cfg.ResumeFlushByPct != true {
		t.Error("pct-only: the pct trigger was turned off")
	}
}
