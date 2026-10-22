package main

import "testing"

// TestBuildTrayMenuModelEnableRules pins when 暂停全部 / 继续全部 are clickable.
//
// The rules mirror the Manager verbs they call: StopAll pauses transferring and
// queued tasks, StartAll resumes paused, failed, canceled and queued ones. A
// menu item that does nothing is worse than a greyed-out one, so a state with
// nothing to act on must be disabled — and the tray must agree with the sidebar
// counters.
func TestBuildTrayMenuModelEnableRules(t *testing.T) {
	cases := []struct {
		name       string
		stats      map[string]int64
		wantPause  bool
		wantResume bool
	}{
		{"nothing at all", nil, false, false},
		{"only completed", map[string]int64{"completed": 3, "total": 3}, false, false},
		{"downloading", map[string]int64{"active": 2}, true, false},
		{"queued counts for both", map[string]int64{"queued": 1}, true, true},
		{"paused", map[string]int64{"paused": 4}, false, true},
		{"failed", map[string]int64{"failed": 1}, false, true},
		{"canceled", map[string]int64{"canceled": 2}, false, true},
		{"mixed", map[string]int64{"active": 1, "paused": 1, "completed": 5}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := buildTrayMenuModel(c.stats, 0)
			if model.CanPause != c.wantPause {
				t.Errorf("CanPause = %v, want %v", model.CanPause, c.wantPause)
			}
			if model.CanResume != c.wantResume {
				t.Errorf("CanResume = %v, want %v", model.CanResume, c.wantResume)
			}
		})
	}
}

// TestBuildTrayMenuModelCarriesTheSpeedLimit covers the value the 限速 submenu
// ticks: whatever the settings hold, including values the presets do not cover.
func TestBuildTrayMenuModelCarriesTheSpeedLimit(t *testing.T) {
	for _, kb := range []int{0, 1024, 5 * 1024, 10 * 1024, 2048} {
		if got := buildTrayMenuModel(nil, kb).SpeedKB; got != kb {
			t.Errorf("SpeedKB = %d, want %d", got, kb)
		}
	}
}

// TestTraySpeedPresets pins the four menu entries and their units: KiB/s, with 0
// meaning unlimited (the same encoding `speedLimitKb` uses).
func TestTraySpeedPresets(t *testing.T) {
	want := []traySpeedPreset{
		{Label: "不限速", KB: 0},
		{Label: "1 MB/s", KB: 1024},
		{Label: "5 MB/s", KB: 5 * 1024},
		{Label: "10 MB/s", KB: 10 * 1024},
	}

	got := traySpeedPresets()
	if len(got) != len(want) {
		t.Fatalf("presets = %+v, want %d entries", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("preset %d = %+v, want %+v", i, got[i], want[i])
		}
		if i > 0 && got[i].KB <= got[i-1].KB {
			t.Errorf("preset %d is not faster than %d: preset order is wrong", i, i-1)
		}
	}
}

// TestTrayCheckedSpeedIndex covers which preset shows the tick: exactly one for a
// preset value, none for a custom cap typed into settings.
func TestTrayCheckedSpeedIndex(t *testing.T) {
	cases := []struct {
		kb   int
		want int
	}{
		{0, 0},
		{1024, 1},
		{5 * 1024, 2},
		{10 * 1024, 3},
		{2048, -1}, // a custom limit is between presets
		{-5, -1},   // cannot be configured, but must not tick the wrong item
	}
	for _, c := range cases {
		if got := trayCheckedSpeedIndex(c.kb); got != c.want {
			t.Errorf("trayCheckedSpeedIndex(%d) = %d, want %d", c.kb, got, c.want)
		}
	}
}
