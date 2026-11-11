package vision

import (
	"os"
	"strings"
	"testing"

	"github.com/Scavenger503/drive-doctor/internal/smart"
	"github.com/Scavenger503/drive-doctor/internal/verdict"
)

func fixture(t *testing.T, name string) (*smart.Report, []string, verdict.Result) {
	t.Helper()
	b, err := os.ReadFile("../../testdata/vision/" + name)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := ParseJSON(string(b))
	if err != nil {
		t.Fatal(err)
	}
	rep, miss := ex.ToReport()
	return rep, miss, verdict.Evaluate(rep)
}

func TestAsustorPartialTable(t *testing.T) {
	rep, miss, res := fixture(t, "asustor_partial.json")
	if rep.Kind != smart.KindATA || !rep.IsSSD {
		t.Fatalf("kind=%v ssd=%v", rep.Kind, rep.IsSSD)
	}
	// SSD: only ID 5 is required, and it's visible.
	if len(miss) != 0 || res.Level != verdict.Healthy {
		t.Fatalf("miss=%v level=%v", miss, res.Level)
	}
}

func TestPartialHDDNeedsMoreRows(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/vision/asustor_partial.json")
	s := strings.Replace(string(b), `"is_ssd":null`, `"is_ssd":false`, 1)
	ex, _ := ParseJSON(s)
	_, miss := ex.ToReport()
	if len(miss) != 2 || !strings.Contains(miss[0], "197") {
		t.Fatalf("want 197 and 198 missing, got %v", miss)
	}
}

func TestSynologySelfTestsOnly(t *testing.T) {
	rep, miss, _ := fixture(t, "synology_selftests.json") // wrapped in ```json fences
	if rep.Kind != smart.KindUnknown || len(miss) != 1 || len(rep.SelfTestFails) != 0 {
		t.Fatalf("kind=%v miss=%v fails=%v", rep.Kind, miss, rep.SelfTestFails)
	}
}

func TestUGREENNVMePartial(t *testing.T) {
	rep, miss, res := fixture(t, "ugreen_nvme_partial.json")
	if rep.Kind != smart.KindNVMe || rep.NVMe.PercentageUsed != 3 {
		t.Fatalf("parse: %+v", rep.NVMe)
	}
	if len(miss) != 2 { // critical warning + media errors not visible
		t.Fatalf("miss=%v", miss)
	}
	if res.Level != verdict.Healthy {
		t.Fatalf("level=%v", res.Level)
	}
}

func TestFailingFromScreenshot(t *testing.T) {
	_, miss, res := fixture(t, "hdd_failing_full.json")
	if len(miss) != 0 || res.Level != verdict.Replace {
		t.Fatalf("miss=%v level=%v", miss, res.Level)
	}
}

func TestFailedSelfTestFromScreenshot(t *testing.T) {
	ex, _ := ParseJSON(`{"kind":"self_tests_only","self_tests":[{"type":"Extended","result":"Read failure","failed":true}]}`)
	rep, _ := ex.ToReport()
	if verdict.Evaluate(rep).Level != verdict.Replace {
		t.Fatal("a failed self-test in a screenshot must mean replace")
	}
}

// Recreates the real misread: a number from the "error log entries" row was put
// under media errors and made a healthy SSD look like it needed replacing.
func TestMisreadRowIsRejected(t *testing.T) {
	rep, miss, res := fixture(t, "ugreen_nvme_misread.json")
	if rep.NVMe.MediaErrors != 0 {
		t.Fatalf("misread media errors kept: %d", rep.NVMe.MediaErrors)
	}
	if res.Level == verdict.Replace {
		t.Fatal("a misread row must never produce 'replace'")
	}
	found := false
	for _, m := range miss {
		if strings.Contains(m, "media") {
			found = true
		}
	}
	if !found {
		t.Fatalf("media errors should be listed as missing, got %v", miss)
	}
	if rep.NVMe.PowerOnHours != 2684 {
		t.Fatalf("correctly labeled power-on hours should be kept, got %d", rep.NVMe.PowerOnHours)
	}
}

func TestATANameMismatchDropped(t *testing.T) {
	ex, _ := ParseJSON(`{"kind":"ata","is_ssd":false,"attributes":[
	 {"id":5,"name":"Reallocated_Sector_Ct","value":100,"worst":100,"thresh":10,"raw":"0"},
	 {"id":197,"name":"Power_On_Hours","value":100,"worst":100,"thresh":0,"raw":"41000"},
	 {"id":198,"name":"Offline_Uncorrectable","value":100,"worst":100,"thresh":0,"raw":"0"}]}`)
	rep, miss := ex.ToReport()
	if _, ok := rep.Attr(197); ok {
		t.Fatal("ID 197 with a wrong name must be dropped")
	}
	if verdict.Evaluate(rep).Level == verdict.Replace || len(miss) != 1 {
		t.Fatalf("level=%v miss=%v", verdict.Evaluate(rep).Level, miss)
	}
}

// The real UGREEN page (all 3 screenshots): IDs 1-12, 14, 17. It never shows
// critical warning or media errors (ID 13), so the check stays "OK so far".
func TestUGREENFullPage(t *testing.T) {
	rep, miss, res := fixture(t, "ugreen_nvme_full_page.json")
	if res.Level != verdict.Healthy || len(miss) != 2 || rep.NVMe.UnsafeShutdowns != 320 {
		t.Fatalf("level=%v miss=%v nvme=%+v", res.Level, miss, rep.NVMe)
	}
}
