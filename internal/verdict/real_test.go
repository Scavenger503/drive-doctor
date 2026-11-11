package verdict

import (
	"testing"

	"github.com/Scavenger503/drive-doctor/internal/smart"
)

func TestRealUGREENWD4006(t *testing.T) {
	rep := load(t, "real/ugreen_wd4006.txt")
	if rep.Model != "WDC WD4006FZBX-00CAJA0" || len(rep.Attributes) != 19 {
		t.Fatalf("parse: model=%q attrs=%d", rep.Model, len(rep.Attributes))
	}
	res := Evaluate(rep)
	t.Logf("level=%v findings=%s notes=%v selftestfails=%v", res.Level, codes(res), res.Notes, rep.SelfTestFails)
	if res.Level != Healthy {
		t.Fatalf("want healthy, got %v (%s)", res.Level, codes(res))
	}
}

func TestRealAsustorX12SSD(t *testing.T) {
	rep := load(t, "real/asustor_x12_ssd.txt")
	res := Evaluate(rep)
	t.Logf("ssd=%v level=%v findings=%s notes=%v", rep.IsSSD, res.Level, codes(res), res.Notes)
	if !rep.IsSSD || rep.LastErrorHour != 0 {
		t.Fatalf("parse: ssd=%v lastErr=%d", rep.IsSSD, rep.LastErrorHour)
	}
	// One error logged at hour 0 (factory) on a 2450-hour drive is not a warning.
	if res.Level != Healthy {
		t.Fatalf("want healthy, got %v (%s)", res.Level, codes(res))
	}
}

func TestOldErrorsGuards(t *testing.T) {
	base := load(t, "real/asustor_x12_ssd.txt")
	cases := []struct {
		name     string
		count    int64
		lastErr  int64
		poh      int64
		wantWarn bool
	}{
		{"factory error, long ago", 1, 0, 2450, false},
		{"recent error", 1, 2400, 2450, true},
		{"too many errors", 5, 0, 2450, true},
		{"unknown error time", 1, -1, 2450, true},
		{"drive past counter wrap", 1, 100, 70000, true},
	}
	for _, c := range cases {
		rep := *base
		rep.ATAErrorCount, rep.LastErrorHour = c.count, c.lastErr
		attrs := append([]smart.Attribute(nil), base.Attributes...)
		for i := range attrs {
			if attrs[i].ID == 9 {
				attrs[i].Raw = c.poh
			}
		}
		rep.Attributes = attrs
		res := Evaluate(&rep)
		if has(res, "ata_errors") != c.wantWarn {
			t.Errorf("%s: warn=%v, want %v (%s)", c.name, has(res, "ata_errors"), c.wantWarn, codes(res))
		}
	}
}
