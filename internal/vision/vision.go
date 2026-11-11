// Package vision reads drive health values from screenshots (NAS web pages,
// CrystalDiskInfo, etc.) using a vision model, then hands them to the same
// rules as pasted reports. The model only transcribes numbers; it never judges.
package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Scavenger503/drive-doctor/internal/ai"
	"github.com/Scavenger503/drive-doctor/internal/smart"
)

// Image is one uploaded screenshot.
type Image struct {
	MIME string
	Data []byte
}

// Config picks the vision models. Base URL and key come from the AI config.
type Config struct {
	AI          ai.Config
	Model       string
	BackupModel string
	Timeout     time.Duration
}

// ConfigFromEnv reads DD_VISION_MODEL / DD_VISION_BACKUP_MODEL.
func ConfigFromEnv(base ai.Config) Config {
	c := Config{AI: base, Model: os.Getenv("DD_VISION_MODEL"), BackupModel: os.Getenv("DD_VISION_BACKUP_MODEL"), Timeout: 45 * time.Second}
	if c.Model == "" {
		c.Model = "DeepSeek-V4.1-Flash"
	}
	if c.BackupModel == "" {
		c.BackupModel = "Gemma-4-31B-IT"
	}
	if strings.EqualFold(c.BackupModel, "none") || c.BackupModel == c.Model {
		c.BackupModel = ""
	}
	return c
}

// NVMeFields are the NVMe health values read from a screenshot (nil = not visible).
type NVMeFields struct {
	CriticalWarning *int   `json:"critical_warning"`
	TemperatureC    *int   `json:"temperature_c"`
	AvailableSpare  *int   `json:"available_spare"`
	SpareThreshold  *int   `json:"available_spare_threshold"`
	PercentageUsed  *int   `json:"percentage_used"`
	PowerOnHours    *int64 `json:"power_on_hours"`
	MediaErrors     *int64 `json:"media_errors"`
	UnsafeShutdowns *int64 `json:"unsafe_shutdowns"`
	// Labels holds the exact on-screen row name each value came from, so the
	// server can reject values copied from the wrong row.
	Labels map[string]string `json:"labels"`
}

// Extracted is what the model transcribed from the screenshots.
type Extracted struct {
	Kind       string `json:"kind"` // "ata", "nvme", "self_tests_only", "unknown"
	Model      string `json:"model"`
	Capacity   string `json:"capacity"`
	IsSSD      *bool  `json:"is_ssd"`
	Overall    string `json:"overall"` // "PASSED", "FAILED", or ""
	Attributes []struct {
		ID     int    `json:"id"`
		Name   string `json:"name"`
		Value  *int   `json:"value"`
		Worst  *int   `json:"worst"`
		Thresh *int   `json:"thresh"`
		Raw    string `json:"raw"`
		State  string `json:"state"`
	} `json:"attributes"`
	NVMe      NVMeFields `json:"nvme"`
	SelfTests []struct {
		Type   string `json:"type"`
		Result string `json:"result"`
		Failed bool   `json:"failed"`
	} `json:"self_tests"`
}

const prompt = `You transcribe hard drive / SSD health (S.M.A.R.T.) data from screenshots. You only copy numbers you can clearly see. You never judge drive health and never guess.

Return ONLY one JSON object, no markdown, in exactly this shape:
{
  "kind": "ata" | "nvme" | "self_tests_only" | "unknown",
  "model": "drive model if visible, else empty",
  "capacity": "capacity if visible, else empty",
  "is_ssd": true | false | null,
  "overall": "PASSED" | "FAILED" | "",
  "attributes": [ {"id": 5, "name": "Reallocated_Sector_Ct", "value": 100, "worst": 100, "thresh": 50, "raw": "0", "state": "Normal"} ],
  "nvme": {"critical_warning": null, "temperature_c": null, "available_spare": null, "available_spare_threshold": null, "percentage_used": null, "power_on_hours": null, "media_errors": null, "unsafe_shutdowns": null,
           "labels": {"temperature_c": "exact row name shown on screen", "media_errors": "exact row name shown on screen"}},
  "self_tests": [ {"type": "Extended", "result": "Healthy", "failed": false} ]
}

Rules:
- "ata": a table of numbered SMART attributes (ID, name, value/current, worst, threshold, raw). Copy every visible row exactly. Use null for any column that is not shown.
- "nvme": NVMe health fields (critical warning, temperature, available spare, spare threshold, percentage used / lifetime used, power on hours, media and data integrity errors, unsafe shutdowns). Use null for anything not visible. Do not put NVMe fields in "attributes".
- For every NVMe value you fill in, put the exact row name you read it from in "labels" under the same key. Read each value from the SAME row as its name. If you are not sure which row a number belongs to, use null. Never fill a field from a row with a different meaning (for example, do not use "Error information log entries", "Power cycles", or "Unsafe shutdowns" as media errors).
- "self_tests_only": the screen only shows a list of self-test runs and results.
- For self tests, set "failed": true only if the result says it failed, error, or read failure.
- If the screen shows the drive's overall health status, put PASSED for good/normal/healthy and FAILED for failing/bad.
- Never output serial numbers or any other identifiers.
- If there are several screenshots of the same drive, combine them into one object.`

// Extract sends the screenshots to the vision model (with one backup try).
func Extract(ctx context.Context, cfg Config, images []Image) (*Extracted, string, error) {
	if !cfg.AI.Enabled() {
		return nil, "", errors.New("no API key configured")
	}
	models := []string{cfg.Model}
	if cfg.BackupModel != "" {
		models = append(models, cfg.BackupModel)
	}
	var errs []error
	for _, m := range models {
		if ctx.Err() != nil {
			break
		}
		ex, err := extractOnce(ctx, cfg, m, images)
		if err == nil {
			return ex, m, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", m, err))
		log.Printf("%s: %s failed, trying next model if any: %v", "vision", m, err)
	}
	return nil, "", errors.Join(errs...)
}

// ExtractWithModel reads the screenshots with one specific model (no backup).
func ExtractWithModel(ctx context.Context, cfg Config, model string, images []Image) (*Extracted, error) {
	return extractOnce(ctx, cfg, model, images)
}

func extractOnce(ctx context.Context, cfg Config, model string, images []Image) (*Extracted, error) {
	content := []map[string]any{{"type": "text", "text": "Transcribe the drive health data from these screenshots."}}
	for _, img := range images {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data)},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": prompt},
			{"role": "user", "content": content},
		},
		"max_tokens":  2000,
		"temperature": 0,
	})
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.AI.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AI.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vision API returned %d: %.200s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return nil, fmt.Errorf("unexpected vision response: %.200s", raw)
	}
	return ParseJSON(out.Choices[0].Message.Content)
}

// ParseJSON pulls the JSON object out of a model reply (tolerating code fences,
// hidden reasoning, or text around it).
func ParseJSON(s string) (*Extracted, error) {
	if i := strings.Index(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, errors.New("no JSON in vision reply")
	}
	var ex Extracted
	if err := json.Unmarshal([]byte(s[start:end+1]), &ex); err != nil {
		return nil, fmt.Errorf("bad JSON in vision reply: %w", err)
	}
	return &ex, nil
}

// labelOK checks that the on-screen row name matches the field it was used for.
func labelOK(field, label string) bool {
	l := strings.ToLower(label)
	if l == "" {
		return false
	}
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(l, w) {
				return true
			}
		}
		return false
	}
	switch field {
	case "critical_warning":
		return has("critical")
	case "temperature_c":
		return has("temp")
	case "available_spare":
		return has("spare") && !has("threshold")
	case "available_spare_threshold":
		return has("spare") && has("threshold")
	case "percentage_used":
		return has("used", "lifetime", "life", "wear", "percent", "endurance")
	case "power_on_hours":
		return has("power") && has("hour", "time")
	case "media_errors":
		return has("media", "integrity")
	case "unsafe_shutdowns":
		return has("unsafe", "shutdown")
	}
	return false
}

// attrNameOK guards the attributes the verdict depends on against values
// copied from a neighbouring row. Unknown IDs are accepted as-is.
func attrNameOK(id int, name string) bool {
	keywords := map[int][]string{
		5:   {"realloc", "retired"},
		9:   {"power", "hour"},
		10:  {"spin"},
		187: {"uncorrect", "reported"},
		190: {"temp"},
		194: {"temp"},
		196: {"realloc", "event"},
		197: {"pending", "current"},
		198: {"uncorrect", "offline"},
		199: {"crc", "udma", "interface"},
	}
	kw, critical := keywords[id]
	if !critical {
		return true
	}
	n := strings.ToLower(strings.ReplaceAll(name, " ", "_"))
	for _, k := range kw {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// ToReport converts the transcription into a report the verdict rules can use,
// plus a plain-English list of important values that weren't visible.
func (ex *Extracted) ToReport() (*smart.Report, []string) {
	r := &smart.Report{Kind: smart.KindUnknown, Model: ex.Model, Capacity: ex.Capacity, LastErrorHour: -1}
	if ex.IsSSD != nil {
		r.IsSSD = *ex.IsSSD
	}
	switch strings.ToUpper(ex.Overall) {
	case "PASSED", "FAILED":
		r.OverallHealth = strings.ToUpper(ex.Overall)
	}
	for _, t := range ex.SelfTests {
		if t.Failed {
			r.SelfTestFails = append(r.SelfTestFails, strings.TrimSpace(t.Type+" test: "+t.Result))
		}
	}

	nv := &ex.NVMe
	checkNVMeLabels(nv)
	hasNVMe := nv.CriticalWarning != nil || nv.PercentageUsed != nil || nv.AvailableSpare != nil || nv.MediaErrors != nil
	switch {
	case len(ex.Attributes) > 0:
		r.Kind = smart.KindATA
		for _, a := range ex.Attributes {
			if !attrNameOK(a.ID, a.Name) {
				continue // name doesn't match the ID: likely a misread row
			}
			at := smart.Attribute{ID: a.ID, Name: a.Name, RawText: a.Raw, Raw: smart.LeadingInt(a.Raw)}
			// Missing normalized columns stay 0, which the rules treat as "unknown".
			if a.Value != nil {
				at.Value = *a.Value
			}
			if a.Worst != nil {
				at.Worst = *a.Worst
			}
			if a.Thresh != nil {
				at.Thresh = *a.Thresh
			}
			if s := strings.ToLower(a.State); strings.Contains(s, "fail") || strings.Contains(s, "bad") {
				at.WhenFailed = "FAILING_NOW"
			}
			r.Attributes = append(r.Attributes, at)
		}
		if ex.IsSSD == nil {
			for _, id := range []int{173, 177, 202, 231, 233} {
				if _, ok := r.Attr(id); ok {
					r.IsSSD = true
				}
			}
		}
	case hasNVMe || strings.EqualFold(ex.Kind, "nvme"):
		r.Kind = smart.KindNVMe
		r.IsSSD = true
		h := &smart.NVMeHealth{}
		if nv.CriticalWarning != nil {
			h.CriticalWarning, h.HasCriticalWarning = *nv.CriticalWarning, true
		}
		if nv.TemperatureC != nil {
			h.TemperatureC = *nv.TemperatureC
		}
		if nv.AvailableSpare != nil {
			h.AvailableSpare, h.HasSpare = *nv.AvailableSpare, true
		}
		if nv.SpareThreshold != nil {
			h.AvailableSpareThr = *nv.SpareThreshold
		}
		if nv.PercentageUsed != nil {
			h.PercentageUsed, h.HasUsed = *nv.PercentageUsed, true
		}
		if nv.PowerOnHours != nil {
			h.PowerOnHours = *nv.PowerOnHours
		}
		if nv.MediaErrors != nil {
			h.MediaErrors = *nv.MediaErrors
		}
		if nv.UnsafeShutdowns != nil {
			h.UnsafeShutdowns = *nv.UnsafeShutdowns
		}
		r.NVMe = h
	}
	return r, missing(r, ex)
}

// missing lists the important values we couldn't see, in plain English.
func missing(r *smart.Report, ex *Extracted) []string {
	var m []string
	switch r.Kind {
	case smart.KindATA:
		need := []struct {
			id   int
			what string
		}{{5, "bad spots already replaced (ID 5)"}}
		if !r.IsSSD {
			need = append(need,
				struct {
					id   int
					what string
				}{197, "areas the drive can't read (ID 197)"},
				struct {
					id   int
					what string
				}{198, "areas that couldn't be repaired (ID 198)"})
		}
		for _, n := range need {
			if _, ok := r.Attr(n.id); !ok {
				m = append(m, n.what)
			}
		}
	case smart.KindNVMe:
		nv := &ex.NVMe
		if nv.CriticalWarning == nil {
			m = append(m, "critical warning")
		}
		if nv.PercentageUsed == nil {
			m = append(m, "percentage of lifetime used")
		}
		if nv.AvailableSpare == nil {
			m = append(m, "available spare")
		}
		if nv.MediaErrors == nil {
			m = append(m, "media and data integrity errors")
		}
	default:
		m = append(m, "the drive's health values (the S.M.A.R.T. attributes table)")
	}
	return m
}

// checkNVMeLabels drops any NVMe value whose on-screen label doesn't match it.
func checkNVMeLabels(nv *NVMeFields) {
	ok := func(f string) bool { return labelOK(f, nv.Labels[f]) }
	if !ok("critical_warning") {
		nv.CriticalWarning = nil
	}
	if !ok("temperature_c") {
		nv.TemperatureC = nil
	}
	if !ok("available_spare") {
		nv.AvailableSpare = nil
	}
	if !ok("available_spare_threshold") {
		nv.SpareThreshold = nil
	}
	if !ok("percentage_used") {
		nv.PercentageUsed = nil
	}
	if !ok("power_on_hours") {
		nv.PowerOnHours = nil
	}
	if !ok("media_errors") {
		nv.MediaErrors = nil
	}
	if !ok("unsafe_shutdowns") {
		nv.UnsafeShutdowns = nil
	}
}
