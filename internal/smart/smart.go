// Package smart parses `smartctl -a` / `smartctl -x` text output for ATA/SATA
// and NVMe drives into a structured report.
package smart

import (
	"regexp"
	"strconv"
	"strings"
)

// Kind is the drive interface type detected in the report.
type Kind string

const (
	KindATA     Kind = "ata"
	KindNVMe    Kind = "nvme"
	KindUnknown Kind = "unknown"
)

// Attribute is one row of the ATA SMART attribute table.
type Attribute struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Value      int    `json:"value"`  // normalized current value
	Worst      int    `json:"worst"`  // normalized worst value
	Thresh     int    `json:"thresh"` // vendor failure threshold
	WhenFailed string `json:"when_failed,omitempty"`
	RawText    string `json:"raw_text"`
	Raw        int64  `json:"raw"` // leading integer of the raw value
}

// NVMeHealth holds the NVMe SMART/Health log fields we care about.
type NVMeHealth struct {
	CriticalWarning    int   `json:"critical_warning"`
	TemperatureC       int   `json:"temperature_c"`
	AvailableSpare     int   `json:"available_spare"`
	AvailableSpareThr  int   `json:"available_spare_threshold"`
	PercentageUsed     int   `json:"percentage_used"`
	PowerOnHours       int64 `json:"power_on_hours"`
	UnsafeShutdowns    int64 `json:"unsafe_shutdowns"`
	MediaErrors        int64 `json:"media_errors"`
	ErrorLogEntries    int64 `json:"error_log_entries"`
	HasCriticalWarning bool  `json:"-"`
	HasSpare           bool  `json:"-"`
	HasUsed            bool  `json:"-"`
}

// Report is the parsed drive health report.
type Report struct {
	Kind          Kind        `json:"kind"`
	Model         string      `json:"model"`
	Family        string      `json:"family,omitempty"`
	Firmware      string      `json:"firmware,omitempty"`
	Capacity      string      `json:"capacity,omitempty"`
	RotationRate  string      `json:"rotation_rate,omitempty"`
	IsSSD         bool        `json:"is_ssd"`
	OverallHealth string      `json:"overall_health,omitempty"` // PASSED / FAILED / ""
	Attributes    []Attribute `json:"attributes,omitempty"`
	NVMe          *NVMeHealth `json:"nvme,omitempty"`
	ATAErrorCount int64       `json:"ata_error_count"`
	// BadThresholds is set when smartctl warns the threshold table checksum is
	// invalid, so vendor thresholds can't be trusted.
	BadThresholds bool `json:"bad_thresholds,omitempty"`
	// LastErrorHour is the drive age (power-on hours) at the most recent
	// logged ATA error, or -1 if unknown.
	LastErrorHour int64    `json:"last_error_hour"`
	SelfTestFails []string `json:"self_test_failures,omitempty"`
	serial        string
}

// Attr returns the attribute with the given ID, if present.
func (r *Report) Attr(id int) (Attribute, bool) {
	for _, a := range r.Attributes {
		if a.ID == id {
			return a, true
		}
	}
	return Attribute{}, false
}

var (
	leadingInt = regexp.MustCompile(`^-?[\d,]+`)
	keyValue   = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9 /()\-.]*?):\s+(.*)$`)
	// "# 1  Extended offline    Completed: read failure       90%     41234         12345678"
	// "Error 1 occurred at disk power-on lifetime: 0 hours (0 days + 0 hours)"
	errorLifetime = regexp.MustCompile(`^Error \d+ (?:\[\d+\] )?occurred at disk power-on lifetime:\s*(\d+) hours`)
	selfTestRow   = regexp.MustCompile(`^#\s*\d+\s+(.+)$`)
)

// NoSMARTData reports whether smartctl said it couldn't get health data, which
// usually means the drive type wasn't detected (common on Synology /dev/sataN).
func NoSMARTData(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "lacks smart capability") ||
		(strings.Contains(t, "smart support is:") && strings.Contains(t, "unavailable"))
}

// LooksLikeReport reports whether text appears to be smartctl output.
func LooksLikeReport(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "smart") &&
		(strings.Contains(t, "attribute_name") ||
			strings.Contains(t, "smart/health information") ||
			strings.Contains(t, "overall-health") ||
			strings.Contains(t, "device model") ||
			strings.Contains(t, "model number"))
}

// Parse parses smartctl text output. It never fails; unknown fields are left empty.
func Parse(text string) *Report {
	r := &Report{Kind: KindUnknown, LastErrorHour: -1}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	inAttrTable := false
	inSelfTest := false
	nvme := &NVMeHealth{}
	sawNVMe := false

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trim := strings.TrimSpace(line)

		if trim == "" {
			inAttrTable = false
			inSelfTest = false
			continue
		}

		// Section markers.
		if strings.HasPrefix(trim, "ID#") && strings.Contains(trim, "ATTRIBUTE_NAME") {
			inAttrTable = true
			r.Kind = KindATA
			continue
		}
		if strings.HasPrefix(trim, "Num") && strings.Contains(trim, "Test_Description") {
			inSelfTest = true
			continue
		}
		if strings.Contains(trim, "SMART/Health Information") || strings.Contains(trim, "NVMe Log 0x02") {
			sawNVMe = true
			r.Kind = KindNVMe
			continue
		}

		if inAttrTable {
			if a, ok := parseAttrRow(trim); ok {
				r.Attributes = append(r.Attributes, a)
				continue
			}
			// Lines like "||||||_ K auto-keep" in -x output: skip.
			if strings.HasPrefix(trim, "|") || strings.HasPrefix(trim, "_") {
				continue
			}
			inAttrTable = false
		}

		if inSelfTest {
			if m := selfTestRow.FindStringSubmatch(trim); m != nil {
				desc := strings.ToLower(m[1])
				if strings.Contains(desc, "failure") || strings.Contains(desc, "failed") ||
					strings.Contains(desc, "fatal") {
					r.SelfTestFails = append(r.SelfTestFails, selfTestSummary(m[1]))
				}
				continue
			}
		}

		if strings.HasPrefix(trim, "SMART overall-health self-assessment test result:") {
			v := strings.TrimSpace(strings.TrimPrefix(trim, "SMART overall-health self-assessment test result:"))
			r.OverallHealth = strings.Trim(strings.ToUpper(strings.Fields(v + " ")[0]), "!.")
			continue
		}
		if strings.HasPrefix(trim, "SMART Health Status:") { // SAS drives
			v := strings.TrimSpace(strings.TrimPrefix(trim, "SMART Health Status:"))
			if strings.EqualFold(v, "OK") {
				r.OverallHealth = "PASSED"
			} else {
				r.OverallHealth = "FAILED"
			}
			continue
		}
		if strings.Contains(trim, "Thresholds Structure error") {
			r.BadThresholds = true
			continue
		}
		if m := errorLifetime.FindStringSubmatch(trim); m != nil {
			if h := firstInt(m[1]); h > r.LastErrorHour {
				r.LastErrorHour = h
			}
			continue
		}
		if strings.HasPrefix(trim, "ATA Error Count:") {
			r.ATAErrorCount = firstInt(strings.TrimPrefix(trim, "ATA Error Count:"))
			continue
		}

		m := keyValue.FindStringSubmatch(trim)
		if m == nil {
			continue
		}
		key, val := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		switch key {
		case "Device Model", "Model Number", "Product":
			if r.Model == "" {
				r.Model = val
			}
			if key == "Model Number" {
				r.Kind = KindNVMe
			}
		case "Model Family":
			r.Family = val
		case "Serial Number":
			r.serial = val
		case "Firmware Version":
			r.Firmware = val
		case "User Capacity", "Total NVM Capacity", "Namespace 1 Size/Capacity":
			if r.Capacity == "" {
				r.Capacity = bracketed(val)
			}
		case "Rotation Rate":
			r.RotationRate = val
			if strings.Contains(strings.ToLower(val), "solid state") {
				r.IsSSD = true
			}
		}

		if sawNVMe {
			switch key {
			case "Critical Warning":
				nvme.CriticalWarning = int(parseHexOrInt(val))
				nvme.HasCriticalWarning = true
			case "Temperature":
				nvme.TemperatureC = int(firstInt(val))
			case "Available Spare":
				nvme.AvailableSpare = int(firstInt(val))
				nvme.HasSpare = true
			case "Available Spare Threshold":
				nvme.AvailableSpareThr = int(firstInt(val))
			case "Percentage Used":
				nvme.PercentageUsed = int(firstInt(val))
				nvme.HasUsed = true
			case "Power On Hours":
				nvme.PowerOnHours = firstInt(val)
			case "Unsafe Shutdowns":
				nvme.UnsafeShutdowns = firstInt(val)
			case "Media and Data Integrity Errors":
				nvme.MediaErrors = firstInt(val)
			case "Error Information Log Entries":
				nvme.ErrorLogEntries = firstInt(val)
			}
		}
	}

	if sawNVMe {
		r.NVMe = nvme
		r.IsSSD = true
	}
	if r.Kind == KindUnknown && len(r.Attributes) > 0 {
		r.Kind = KindATA
	}
	// SSDs that don't report a rotation rate usually expose wear attributes.
	if !r.IsSSD {
		for _, id := range []int{173, 177, 202, 231, 233} {
			if _, ok := r.Attr(id); ok {
				r.IsSSD = true
				break
			}
		}
	}
	return r
}

// parseAttrRow handles both the classic `smartctl -a` layout:
//
//	ID# ATTRIBUTE_NAME FLAG VALUE WORST THRESH TYPE UPDATED WHEN_FAILED RAW_VALUE
//
// and the brief `smartctl -x` layout:
//
//	ID# ATTRIBUTE_NAME FLAGS VALUE WORST THRESH FAIL RAW_VALUE
func parseAttrRow(line string) (Attribute, bool) {
	f := strings.Fields(line)
	if len(f) < 8 {
		return Attribute{}, false
	}
	id, err := strconv.Atoi(f[0])
	if err != nil {
		return Attribute{}, false
	}
	a := Attribute{ID: id, Name: f[1]}
	a.Value = atoiLoose(f[3])
	a.Worst = atoiLoose(f[4])
	a.Thresh = atoiLoose(f[5])

	var rawFields []string
	if (f[6] == "Pre-fail" || f[6] == "Old_age") && len(f) >= 10 {
		a.WhenFailed = f[8]
		rawFields = f[9:]
	} else {
		a.WhenFailed = f[6]
		rawFields = f[7:]
	}
	if a.WhenFailed == "-" {
		a.WhenFailed = ""
	}
	a.RawText = strings.Join(rawFields, " ")
	a.Raw = firstInt(a.RawText)
	return a, true
}

var selfTestParts = regexp.MustCompile(`^(\w+)\s+\w+\s+(.*?)\s+\d+%`)

// selfTestSummary turns "Extended offline  Completed: read failure  90% 42180 123"
// into "Extended test: read failure".
func selfTestSummary(row string) string {
	m := selfTestParts.FindStringSubmatch(row)
	if m == nil {
		return strings.Join(strings.Fields(row), " ")
	}
	status := strings.TrimPrefix(strings.TrimSpace(m[2]), "Completed: ")
	return m[1] + " test: " + strings.Join(strings.Fields(status), " ")
}

func atoiLoose(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func firstInt(s string) int64 {
	m := leadingInt.FindString(strings.TrimSpace(s))
	if m == "" {
		return 0
	}
	n, _ := strconv.ParseInt(strings.ReplaceAll(m, ",", ""), 10, 64)
	return n
}

func parseHexOrInt(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "0x") {
		n, _ := strconv.ParseInt(strings.Fields(s)[0][2:], 16, 64)
		return n
	}
	return firstInt(s)
}

// bracketed returns "4.00 TB" from "4,000,787,030,016 bytes [4.00 TB]".
func bracketed(s string) string {
	if i := strings.Index(s, "["); i >= 0 {
		if j := strings.Index(s[i:], "]"); j > 0 {
			return s[i+1 : i+j]
		}
	}
	return s
}

// Scrub removes the serial number and other device identifiers from the report
// text so they never leave the server.
func Scrub(text string) string {
	idLine := regexp.MustCompile(`(?mi)^(\s*(?:Serial Number|LU WWN Device Id|IEEE OUI Identifier|Logical Unit id|Namespace 1 IEEE EUI-64|World Wide Name)\s*:\s*).*$`)
	serials := []string{}
	for _, m := range regexp.MustCompile(`(?mi)^\s*Serial Number\s*:\s*(\S+)`).FindAllStringSubmatch(text, -1) {
		if len(m[1]) >= 4 {
			serials = append(serials, m[1])
		}
	}
	text = idLine.ReplaceAllString(text, "${1}[removed]")
	for _, s := range serials {
		text = strings.ReplaceAll(text, s, "[removed]")
	}
	return text
}

// LeadingInt returns the first integer in s ("31 (Min/Max 20/45)" -> 31).
func LeadingInt(s string) int64 { return firstInt(s) }
