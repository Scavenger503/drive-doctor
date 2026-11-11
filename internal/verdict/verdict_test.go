package verdict

import (
	"os"
	"strings"
	"testing"

	"github.com/Scavenger503/drive-doctor/internal/smart"
)

func load(t *testing.T, name string) *smart.Report {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return smart.Parse(string(b))
}

func codes(r Result) string {
	var c []string
	for _, f := range r.Findings {
		c = append(c, f.Code)
	}
	return strings.Join(c, ",")
}

func has(r Result, code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestHealthyWDRed(t *testing.T) {
	rep := load(t, "healthy_wd_red.txt")
	if rep.Model != "WDC WD40EFRX-68N32N0" || rep.Capacity != "4.00 TB" || rep.Kind != smart.KindATA {
		t.Fatalf("bad parse: %+v", rep)
	}
	res := Evaluate(rep)
	if res.Level != Healthy {
		t.Fatalf("want healthy, got %v (%s)", res.Level, codes(res))
	}
	if len(res.Notes) != 2 {
		t.Fatalf("want temp+age notes, got %v", res.Notes)
	}
}

func TestFailingSeagate(t *testing.T) {
	rep := load(t, "failing_seagate.txt")
	res := Evaluate(rep)
	if res.Level != Replace {
		t.Fatalf("want replace, got %v", res.Level)
	}
	for _, c := range []string{"pending_sectors", "uncorrectable_sectors", "reallocated_sectors", "reported_uncorrect", "self_test_failed", "ata_errors"} {
		if !has(res, c) {
			t.Errorf("missing finding %s (got %s)", c, codes(res))
		}
	}
	// Seagate packed raw values (1, 7, 195) must not trigger anything.
	if strings.Contains(codes(res), "raw_read") {
		t.Errorf("packed raw value misread: %s", codes(res))
	}
	if rep.ATAErrorCount != 214 {
		t.Errorf("ata error count = %d", rep.ATAErrorCount)
	}
}

func TestCableCRCBriefFormat(t *testing.T) {
	rep := load(t, "cable_crc_x.txt")
	if len(rep.Attributes) != 8 {
		t.Fatalf("want 8 attributes from -x layout, got %d", len(rep.Attributes))
	}
	res := Evaluate(rep)
	if res.Level != Watch || !has(res, "crc_errors") {
		t.Fatalf("want watch with crc_errors, got %v %s", res.Level, codes(res))
	}
	if !res.Findings[0].DriveOK {
		t.Error("CRC finding should be marked as likely not the drive")
	}
}

func TestNVMeHealthy(t *testing.T) {
	rep := load(t, "nvme_healthy.txt")
	if rep.Kind != smart.KindNVMe || !rep.IsSSD || rep.NVMe.PowerOnHours != 6214 {
		t.Fatalf("bad nvme parse: %+v %+v", rep, rep.NVMe)
	}
	res := Evaluate(rep)
	if res.Level != Healthy {
		t.Fatalf("want healthy, got %v (%s)", res.Level, codes(res))
	}
}

func TestNVMeWorn(t *testing.T) {
	rep := load(t, "nvme_worn.txt")
	if rep.OverallHealth != "FAILED" {
		t.Fatalf("overall = %q", rep.OverallHealth)
	}
	res := Evaluate(rep)
	if res.Level != Replace {
		t.Fatalf("want replace, got %v", res.Level)
	}
	for _, c := range []string{"overall_failed", "nvme_critical_warning", "nvme_spare_low", "ssd_worn", "nvme_media_errors"} {
		if !has(res, c) {
			t.Errorf("missing %s (got %s)", c, codes(res))
		}
	}
}

func TestScrubRemovesSerials(t *testing.T) {
	for _, f := range []string{"healthy_wd_red.txt", "failing_seagate.txt", "nvme_healthy.txt"} {
		b, _ := os.ReadFile("../../testdata/" + f)
		out := smart.Scrub(string(b))
		for _, s := range []string{"WD-WCC7K1ABCDEF", "ZFL1XYZ9", "S4EWNX0R123456A", "0014ee 2b8e1a2c3", "5b11b2c3d4"} {
			if strings.Contains(out, s) {
				t.Errorf("%s: identifier %q survived scrub", f, s)
			}
		}
	}
}

func TestScrubLowercaseSerial(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/real/synology_no_dsat.txt")
	if strings.Contains(smart.Scrub(string(b)), "WD-TESTSERIAL9") {
		t.Fatal("lowercase 'Serial number' was not scrubbed")
	}
}
