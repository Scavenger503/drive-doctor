// Package verdict applies fixed, explainable rules to a parsed SMART report.
// The AI never decides the verdict; it only explains what these rules found.
package verdict

import (
	"fmt"
	"sort"

	"github.com/Scavenger503/drive-doctor/internal/smart"
)

// Level is the overall verdict, ordered by severity.
type Level int

const (
	Healthy Level = iota
	Watch
	Replace
)

func (l Level) String() string {
	switch l {
	case Watch:
		return "watch"
	case Replace:
		return "replace"
	default:
		return "healthy"
	}
}

func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// Finding is one thing the rules noticed.
type Finding struct {
	Level   Level  `json:"level"`
	Code    string `json:"code"`     // stable ID, e.g. "pending_sectors"
	Title   string `json:"title"`    // short English label
	Detail  string `json:"detail"`   // one-sentence English explanation
	Value   string `json:"value"`    // the number that triggered it
	DriveOK bool   `json:"drive_ok"` // true when the cause is likely not the drive itself (cable, cooling)
}

// Note is a neutral fact. Code + Value let the page show it in any language;
// Text is the English version (also what the AI sees).
type Note struct {
	Code  string `json:"code"`
	Value int64  `json:"value,omitempty"`
	Text  string `json:"text"`
}

// Result is the full verdict.
type Result struct {
	Level    Level     `json:"level"`
	Findings []Finding `json:"findings"`
	Notes    []Note    `json:"notes,omitempty"` // neutral facts (age, temperature)
	// Incomplete lists important values that weren't available (e.g. cut off in a
	// screenshot). A "healthy" result with missing values is only "OK so far".
	Incomplete []string `json:"incomplete,omitempty"`
	// Unconfirmed are warnings dropped because two independent screenshot reads
	// disagreed on the number behind them.
	Unconfirmed []Finding `json:"unconfirmed,omitempty"`
}

// Seagate and some others pack these raw values with vendor-specific data,
// so their raw numbers are meaningless on their own.
var packedRaw = map[int]bool{1: true, 7: true, 195: true}

// Evaluate applies the rules to a report.
func Evaluate(r *smart.Report) Result {
	res := Result{}
	add := func(f Finding) { res.Findings = append(res.Findings, f) }

	if r.OverallHealth == "FAILED" {
		add(Finding{Level: Replace, Code: "overall_failed", Title: "Drive's own health check failed",
			Detail: "The drive itself reports that it has failed its built-in health assessment.",
			Value:  "FAILED"})
	}

	for _, t := range r.SelfTestFails {
		add(Finding{Level: Replace, Code: "self_test_failed", Title: "A drive self-test failed",
			Detail: "A self-test recorded in the drive's log did not complete successfully, which usually means unreadable areas on the disk.",
			Value:  t})
		break // one is enough
	}

	if r.Kind == smart.KindNVMe && r.NVMe != nil {
		evalNVMe(r.NVMe, add, &res)
	} else {
		evalATA(r, add, &res)
	}

	if r.ATAErrorCount > 0 {
		poh := powerOnHours(r)
		// A few errors logged long ago (often at the factory, at hour 0) aren't a
		// sign of trouble today: note them instead of warning.
		// The error log's hour counter is 16-bit and wraps at 65535 hours, so on
		// drives older than that we can't tell when an error happened: always warn.
		old := r.ATAErrorCount < 5 && r.LastErrorHour >= 0 && poh > 0 && poh < 65535 &&
			poh-r.LastErrorHour >= 720
		switch {
		case old:
			res.Notes = append(res.Notes, Note{Code: "old_errors", Value: r.ATAErrorCount,
				Text: oldErrorsText(r.ATAErrorCount, r.LastErrorHour)})
		default:
			lvl := Watch
			if r.ATAErrorCount >= 50 {
				lvl = Replace
			}
			add(Finding{Level: lvl, Code: "ata_errors", Title: "Errors in the drive's error log",
				Detail: "The drive has logged read/write errors. A few old ones can be harmless; many or recent ones are a warning sign.",
				Value:  fmt.Sprint(r.ATAErrorCount)})
		}
	}

	// Overall level = worst finding.
	for _, f := range res.Findings {
		if f.Level > res.Level {
			res.Level = f.Level
		}
	}
	sort.SliceStable(res.Findings, func(i, j int) bool { return res.Findings[i].Level > res.Findings[j].Level })
	return res
}

func evalATA(r *smart.Report, add func(Finding), res *Result) {
	// Any attribute at or below its vendor threshold = the drive's own failure signal.
	// Skip the threshold comparison when smartctl says the threshold table is corrupt.
	if r.BadThresholds {
		res.Notes = append(res.Notes, Note{Code: "bad_thresholds", Text: "The drive's built-in warning limits couldn't be read, which is common and harmless on some drives. Every other check still ran normally."})
	}
	for _, a := range r.Attributes {
		belowThresh := !r.BadThresholds && a.Thresh > 0 && a.Value > 0 && a.Value <= a.Thresh
		if a.WhenFailed == "FAILING_NOW" || belowThresh {
			add(Finding{Level: Replace, Code: "attr_below_threshold", Title: "A health value crossed the failure line",
				Detail: fmt.Sprintf("%s has dropped to or below the level the manufacturer considers failing.", a.Name),
				Value:  fmt.Sprintf("%d (limit %d)", a.Value, a.Thresh)})
		} else if a.WhenFailed == "In_the_past" {
			add(Finding{Level: Watch, Code: "attr_failed_past", Title: "A health value crossed the failure line in the past",
				Detail: fmt.Sprintf("%s was at a failing level at some point, even though it is back above the limit now.", a.Name),
				Value:  a.Name})
		}
	}

	raw := func(id int) (int64, bool) {
		a, ok := r.Attr(id)
		if !ok || packedRaw[id] {
			return 0, false
		}
		return a.Raw, true
	}

	if v, ok := raw(5); ok && v > 0 {
		lvl := Watch
		if v >= 10 {
			lvl = Replace
		}
		add(Finding{Level: lvl, Code: "reallocated_sectors", Title: "Bad spots already replaced",
			Detail: "The drive found damaged areas and swapped them for spare ones. Drives with any of these are statistically much more likely to fail, and a growing number is a clear warning.",
			Value:  fmt.Sprint(v)})
	}
	if v, ok := raw(197); ok && v > 0 {
		add(Finding{Level: Replace, Code: "pending_sectors", Title: "Areas the drive can't read right now",
			Detail: "Some areas of the disk couldn't be read and are waiting to be checked. Files stored there may already be damaged.",
			Value:  fmt.Sprint(v)})
	}
	if v, ok := raw(198); ok && v > 0 {
		add(Finding{Level: Replace, Code: "uncorrectable_sectors", Title: "Areas that couldn't be repaired",
			Detail: "The drive found areas it could not read or fix during its own scans.",
			Value:  fmt.Sprint(v)})
	}
	if v, ok := raw(187); ok && v > 0 {
		lvl := Watch
		if v >= 10 {
			lvl = Replace
		}
		add(Finding{Level: lvl, Code: "reported_uncorrect", Title: "Read errors the drive couldn't fix",
			Detail: "The drive returned data it couldn't correct. This often comes before more serious failures.",
			Value:  fmt.Sprint(v)})
	}
	if v, ok := raw(196); ok && v > 0 {
		add(Finding{Level: Watch, Code: "realloc_events", Title: "Repair events recorded",
			Detail: "The drive has had to move data away from damaged areas.",
			Value:  fmt.Sprint(v)})
	}
	if v, ok := raw(10); ok && v > 0 {
		add(Finding{Level: Watch, Code: "spin_retry", Title: "Trouble spinning up",
			Detail: "The drive needed more than one try to spin up, which can point to a motor or power problem.",
			Value:  fmt.Sprint(v)})
	}
	if v, ok := raw(199); ok && v > 0 {
		add(Finding{Level: Watch, Code: "crc_errors", Title: "Connection errors (usually the cable)",
			Detail: "Data got garbled between the drive and the computer. This is usually a loose or bad cable or slot, not the drive itself. If the number keeps going up, reseat or replace the cable.",
			Value:  fmt.Sprint(v), DriveOK: true})
	}

	// SSD wear (normalized value counts down from 100).
	for _, id := range []int{231, 233, 177, 202, 173} {
		a, ok := r.Attr(id)
		if !ok || a.Value == 0 || a.Value > 100 {
			continue
		}
		if a.Value < 10 {
			add(Finding{Level: Replace, Code: "ssd_worn", Title: "SSD is nearly worn out",
				Detail: "The SSD has used up almost all of its rated write life.",
				Value:  fmt.Sprintf("%d%% life left", a.Value)})
		} else if a.Value < 20 {
			add(Finding{Level: Watch, Code: "ssd_wearing", Title: "SSD is getting worn",
				Detail: "The SSD has used most of its rated write life. Plan a replacement.",
				Value:  fmt.Sprintf("%d%% life left", a.Value)})
		}
		break
	}

	if v, ok := raw(194); ok {
		tempFinding(int(v), add, res)
	} else if v, ok := raw(190); ok {
		tempFinding(int(v), add, res)
	}
	if v, ok := raw(9); ok {
		ageFinding(v, add, res)
	}
}

func evalNVMe(n *smart.NVMeHealth, add func(Finding), res *Result) {
	if n.HasCriticalWarning && n.CriticalWarning != 0 {
		add(Finding{Level: Replace, Code: "nvme_critical_warning", Title: "SSD raised a critical warning",
			Detail: "The SSD itself is flagging a serious problem (spare space, temperature, reliability, or read-only mode).",
			Value:  fmt.Sprintf("0x%02x", n.CriticalWarning)})
	}
	if n.HasSpare && n.AvailableSpareThr > 0 && n.AvailableSpare <= n.AvailableSpareThr {
		add(Finding{Level: Replace, Code: "nvme_spare_low", Title: "SSD is out of spare space",
			Detail: "The SSD has used up the reserve it uses to replace worn-out cells.",
			Value:  fmt.Sprintf("%d%% (limit %d%%)", n.AvailableSpare, n.AvailableSpareThr)})
	}
	if n.HasUsed && n.PercentageUsed < 80 {
		res.Notes = append(res.Notes, Note{Code: "lifetime_used", Value: int64(n.PercentageUsed),
			Text: fmt.Sprintf("Rated lifetime used: %d%%", n.PercentageUsed)})
	}
	if n.HasSpare && (n.AvailableSpareThr == 0 || n.AvailableSpare > n.AvailableSpareThr) {
		res.Notes = append(res.Notes, Note{Code: "spare_left", Value: int64(n.AvailableSpare),
			Text: fmt.Sprintf("Spare space left: %d%%", n.AvailableSpare)})
	}
	if n.HasUsed {
		switch {
		case n.PercentageUsed >= 90:
			add(Finding{Level: Replace, Code: "ssd_worn", Title: "SSD is nearly worn out",
				Detail: "The SSD has used up almost all of its rated write life.",
				Value:  fmt.Sprintf("%d%% used", n.PercentageUsed)})
		case n.PercentageUsed >= 80:
			add(Finding{Level: Watch, Code: "ssd_wearing", Title: "SSD is getting worn",
				Detail: "The SSD has used most of its rated write life. Plan a replacement.",
				Value:  fmt.Sprintf("%d%% used", n.PercentageUsed)})
		}
	}
	if n.MediaErrors > 0 {
		lvl := Watch
		if n.MediaErrors >= 10 {
			lvl = Replace
		}
		add(Finding{Level: lvl, Code: "nvme_media_errors", Title: "Data errors the SSD couldn't fix",
			Detail: "The SSD found data it couldn't read back correctly.",
			Value:  fmt.Sprint(n.MediaErrors)})
	}
	if n.TemperatureC > 0 {
		if n.TemperatureC >= 70 {
			add(Finding{Level: Watch, Code: "temp_high", Title: "Running very hot",
				Detail: "The SSD is hot enough that it may slow itself down. Better airflow or a heatsink helps.",
				Value:  fmt.Sprintf("%d°C", n.TemperatureC), DriveOK: true})
		} else {
			res.Notes = append(res.Notes, tempNote(n.TemperatureC))
		}
	}
	if n.PowerOnHours > 0 {
		res.Notes = append(res.Notes, ageNote(n.PowerOnHours))
	}
}

func tempFinding(c int, add func(Finding), res *Result) {
	if c <= 0 || c > 120 {
		return
	}
	if c >= 55 {
		add(Finding{Level: Watch, Code: "temp_high", Title: "Running hot",
			Detail: "The drive is running hotter than recommended. Heat shortens drive life, so check fans and airflow.",
			Value:  fmt.Sprintf("%d°C", c), DriveOK: true})
		return
	}
	res.Notes = append(res.Notes, tempNote(c))
}

func ageFinding(h int64, add func(Finding), res *Result) {
	if h <= 0 {
		return
	}
	// ~6 years of 24/7 use: Backblaze data shows failure rates climbing with age.
	if h >= 52000 {
		add(Finding{Level: Watch, Code: "old_drive", Title: "Older drive",
			Detail: "The drive has run for more than six years. Older drives fail more often even when everything else looks fine, so make sure it's backed up.",
			Value:  hoursText(h)})
		return
	}
	res.Notes = append(res.Notes, ageNote(h))
}

func hoursText(h int64) string {
	years := float64(h) / 8766
	if years >= 1 {
		return fmt.Sprintf("%d hours (about %.1f years)", h, years)
	}
	return fmt.Sprintf("%d hours (about %d days)", h, h/24)
}

func tempNote(c int) Note {
	return Note{Code: "temperature", Value: int64(c), Text: fmt.Sprintf("Temperature: %d°C", c)}
}

func ageNote(h int64) Note {
	return Note{Code: "power_on_hours", Value: h, Text: "Powered on for " + hoursText(h)}
}

// powerOnHours returns the drive's age in hours from attribute 9 or the NVMe log.
func powerOnHours(r *smart.Report) int64 {
	if r.NVMe != nil && r.NVMe.PowerOnHours > 0 {
		return r.NVMe.PowerOnHours
	}
	if a, ok := r.Attr(9); ok {
		return a.Raw
	}
	return 0
}

func oldErrorsText(n, hour int64) string {
	if n == 1 {
		return fmt.Sprintf("1 old error in the drive's log, from %d hours of use. Nothing since then, so it's not a concern.", hour)
	}
	return fmt.Sprintf("%d old errors in the drive's log, the latest from %d hours of use. Nothing since then, so they're not a concern.", n, hour)
}

// Confirm keeps only the findings that a second, independent reading agrees on
// (same code and same value). Disagreements move to Unconfirmed and the level is
// recomputed from what's left.
func Confirm(first, second Result) Result {
	agree := map[string]bool{}
	for _, f := range second.Findings {
		agree[f.Code+"|"+f.Value] = true
	}
	out := first
	out.Findings = nil
	out.Level = Healthy
	for _, f := range first.Findings {
		if agree[f.Code+"|"+f.Value] {
			out.Findings = append(out.Findings, f)
			if f.Level > out.Level {
				out.Level = f.Level
			}
		} else {
			out.Unconfirmed = append(out.Unconfirmed, f)
		}
	}
	return out
}
