// Package server exposes the web page and the /api/analyze endpoint.
package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Scavenger503/drive-doctor/internal/ai"
	"github.com/Scavenger503/drive-doctor/internal/smart"
	"github.com/Scavenger503/drive-doctor/internal/verdict"
	"github.com/Scavenger503/drive-doctor/internal/vision"
	"github.com/Scavenger503/drive-doctor/web"
)

const maxReportBytes = 256 << 10

// Server holds config and the per-visitor daily limiter.
type Server struct {
	AI         ai.Config
	Vision     vision.Config
	DailyLimit int
	limiter    *limiter
	results    *resultCache
}

// New creates a server. If DD_DATA_DIR is set, daily limits are saved there.
func New(cfg ai.Config, dailyLimit int) *Server {
	lim := newLimiter()
	if dir := os.Getenv("DD_DATA_DIR"); dir != "" {
		lim = loadLimiter(filepath.Join(dir, "limits.json"))
	}
	return &Server{AI: cfg, Vision: vision.ConfigFromEnv(cfg), DailyLimit: dailyLimit,
		limiter: lim, results: newResultCache()}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(web.Files))
	mux.HandleFunc("POST /api/analyze", s.analyze)
	mux.HandleFunc("POST /api/check", s.check)
	mux.HandleFunc("POST /api/explain", s.explainOnly)
	mux.HandleFunc("POST /api/screenshot", s.screenshot)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return securityHeaders(mux)
}

type analyzeRequest struct {
	Report   string `json:"report"`
	Lang     string `json:"lang"`
	ResultID string `json:"result_id,omitempty"` // explain a screenshot result
}

type analyzeResponse struct {
	Drive       driveInfo       `json:"drive"`
	Verdict     verdict.Result  `json:"verdict"`
	Explanation *ai.Explanation `json:"explanation,omitempty"`
	Limit       *limitInfo      `json:"limit,omitempty"`
}

type driveInfo struct {
	Model    string `json:"model"`
	Family   string `json:"family,omitempty"`
	Capacity string `json:"capacity,omitempty"`
	Type     string `json:"type"`
	Firmware string `json:"firmware,omitempty"`
}

type limitInfo struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}

// parse decodes the request and runs the rules. It writes an error and
// returns ok=false when the input isn't usable.
func parse(w http.ResponseWriter, r *http.Request) (req analyzeRequest, rep *smart.Report, res verdict.Result, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxReportBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "The report is too large or not readable.")
		return
	}
	if req.Lang != "es" {
		req.Lang = "en"
	}
	if smart.NoSMARTData(req.Report) && !strings.Contains(req.Report, "ATTRIBUTE_NAME") && !strings.Contains(req.Report, "SMART/Health Information") {
		writeErr(w, http.StatusUnprocessableEntity, "This report has no health data: smartctl couldn't tell what kind of drive it is. Run it again with -d sat added, for example: sudo smartctl -a -d sat /dev/sata1 (Synology) or sudo smartctl -a -d sat /dev/sda.")
		return
	}
	if !smart.LooksLikeReport(req.Report) {
		writeErr(w, http.StatusUnprocessableEntity, "That doesn't look like a drive health (SMART) report. Paste the full output of smartctl -a.")
		return
	}
	// Identifiers are removed before parsing so they are never logged or sent anywhere.
	rep = smart.Parse(smart.Scrub(req.Report))
	if rep.Kind == smart.KindUnknown {
		writeErr(w, http.StatusUnprocessableEntity, "Couldn't find the health values in that report. Make sure you pasted the whole thing.")
		return
	}
	return req, rep, verdict.Evaluate(rep), true
}

func drive(rep *smart.Report) driveInfo {
	return driveInfo{Model: rep.Model, Family: rep.Family, Capacity: rep.Capacity,
		Firmware: rep.Firmware, Type: map[bool]string{true: "SSD", false: "HDD"}[rep.IsSSD]}
}

// explain runs the AI step, respecting the per-visitor daily limit.
func (s *Server) explain(r *http.Request, rep *smart.Report, res verdict.Result, lang string) (ai.Explanation, *limitInfo) {
	used, ok := s.limiter.take(clientIP(r), s.DailyLimit, 1)
	var lim *limitInfo
	if s.DailyLimit > 0 {
		lim = &limitInfo{Used: used, Limit: s.DailyLimit}
	}
	if !ok {
		// Over the daily AI limit: the verdict still works, only the AI wording is skipped.
		return ai.Fallback(res, lang), lim
	}
	exp, err := ai.Explain(r.Context(), s.AI, rep, res, lang)
	if err != nil {
		log.Printf("ai: %v", err)
	}
	return exp, lim
}

// check returns the rule-based verdict instantly (no AI, not rate limited).
func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	_, rep, res, ok := parse(w, r)
	if !ok {
		return
	}
	writeJSON(w, analyzeResponse{Drive: drive(rep), Verdict: res})
}

// explainOnly returns just the AI explanation (the page calls it after /api/check).
func (s *Server) explainOnly(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReportBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "The report is too large or not readable.")
		return
	}
	var peek analyzeRequest
	json.Unmarshal(body, &peek)
	if peek.ResultID != "" {
		// Screenshot results were already counted toward the daily limit when read.
		e, ok := s.results.get(peek.ResultID)
		if !ok {
			writeErr(w, http.StatusNotFound, "That result has expired. Please check the drive again.")
			return
		}
		lang := peek.Lang
		if lang != "es" {
			lang = "en"
		}
		exp, err := ai.Explain(r.Context(), s.AI, e.rep, e.res, lang)
		if err != nil {
			log.Printf("ai: %v", err)
		}
		writeJSON(w, map[string]any{"explanation": exp})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	req, rep, res, ok := parse(w, r)
	if !ok {
		return
	}
	exp, lim := s.explain(r, rep, res, req.Lang)
	writeJSON(w, map[string]any{"explanation": exp, "limit": lim})
}

const (
	screenshotCost = 2
	maxImages      = 4
	maxImageBytes  = 6 << 20
)

// screenshot reads drive health values from up to 4 screenshots.
func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) {
	if !s.AI.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "Reading screenshots needs the AI service, which isn't set up here. Paste the smartctl report instead.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImages*maxImageBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "The screenshots are too large. Use up to 4 images, 6 MB each.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["images"]
	if len(files) == 0 || len(files) > maxImages {
		writeErr(w, http.StatusBadRequest, "Add between 1 and 4 screenshots.")
		return
	}
	var imgs []vision.Image
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Couldn't read one of the screenshots.")
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
		f.Close()
		if err != nil || len(data) > maxImageBytes {
			writeErr(w, http.StatusBadRequest, "Each screenshot must be 6 MB or smaller.")
			return
		}
		mime := http.DetectContentType(data)
		switch mime {
		case "image/png", "image/jpeg", "image/webp", "image/gif":
		default:
			writeErr(w, http.StatusBadRequest, "Screenshots must be PNG, JPG, WebP, or GIF images.")
			return
		}
		imgs = append(imgs, vision.Image{MIME: mime, Data: data})
	}

	// A screenshot check costs 2: reading the images (plus a possible second
	// opinion) is heavier than explaining a pasted report.
	used, ok := s.limiter.take(clientIP(r), s.DailyLimit, screenshotCost)
	if !ok {
		writeErr(w, http.StatusTooManyRequests, "You've reached today's limit for AI checks. You can still paste a smartctl report, or try again tomorrow.")
		return
	}
	ex, model, err := vision.Extract(r.Context(), s.Vision, imgs)
	if err != nil {
		log.Printf("vision: %v", err)
		writeErr(w, http.StatusBadGateway, "Couldn't read the screenshots right now. Please try again in a minute, or paste the smartctl report instead.")
		return
	}
	rep, missing := ex.ToReport()
	res := verdict.Evaluate(rep)
	res.Incomplete = missing

	// Bad news from a screenshot gets a second, independent read by the other
	// model. A warning only stands if both readings agree on the number.
	confirmedBy := ""
	if len(res.Findings) > 0 {
		other := s.Vision.BackupModel
		if model == s.Vision.BackupModel {
			other = s.Vision.Model
		}
		if other != "" && other != model {
			if ex2, err := vision.ExtractWithModel(r.Context(), s.Vision, other, imgs); err == nil {
				rep2, _ := ex2.ToReport()
				res = verdict.Confirm(res, verdict.Evaluate(rep2))
				res.Incomplete = missing
				confirmedBy = other
			} else {
				log.Printf("vision: second opinion from %s failed: %v", other, err)
				res.Notes = append(res.Notes, verdict.Note{Code: "unconfirmed",
					Text: "The warnings above couldn't be double-checked by a second reading. A pasted smartctl report gives the most reliable result."})
			}
		}
	}

	resp := map[string]any{
		"drive":     drive(rep),
		"verdict":   res,
		"result_id": s.results.put(rep, res),
		"read_by":   model,
	}
	if confirmedBy != "" {
		resp["confirmed_by"] = confirmedBy
	}
	if s.DailyLimit > 0 {
		resp["limit"] = limitInfo{Used: used, Limit: s.DailyLimit}
	}
	writeJSON(w, resp)
}

// resultCache keeps screenshot results briefly so the page can ask for the
// explanation without uploading the images again.
type resultCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	rep     *smart.Report
	res     verdict.Result
	expires time.Time
}

func newResultCache() *resultCache { return &resultCache{m: map[string]cacheEntry{}} }

func (c *resultCache) put(rep *smart.Report, res verdict.Result) string {
	id := randomHex(16)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, e := range c.m {
		if now.After(e.expires) {
			delete(c.m, k)
		}
	}
	c.m[id] = cacheEntry{rep: rep, res: res, expires: now.Add(15 * time.Minute)}
	return id
}

func (c *resultCache) get(id string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok || time.Now().After(e.expires) {
		return cacheEntry{}, false
	}
	return e, true
}

// analyze does both in one call (handy for scripts and the API).
func (s *Server) analyze(w http.ResponseWriter, r *http.Request) {
	req, rep, res, ok := parse(w, r)
	if !ok {
		return
	}
	exp, lim := s.explain(r, rep, res, req.Lang)
	writeJSON(w, analyzeResponse{Drive: drive(rep), Verdict: res, Explanation: &exp, Limit: lim})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data: blob:")
		h.ServeHTTP(w, r)
	})
}

// clientIP prefers Cloudflare's header (the app is meant to sit behind a Cloudflare Tunnel).
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter counts AI checks per visitor per UTC day. Visitors are stored as
// salted hashes, never raw IPs. If a data file is set, counts survive restarts.
type limiter struct {
	mu   sync.Mutex
	Day  string
	Seen map[string]int
	path string
	salt string
}

func newLimiter() *limiter { return &limiter{Seen: map[string]int{}, salt: randomHex(16)} }

// loadLimiter restores today's counts from path (best effort).
func loadLimiter(path string) *limiter {
	l := newLimiter()
	l.path = path
	if b, err := os.ReadFile(path); err == nil {
		var saved limiterFile
		if json.Unmarshal(b, &saved) == nil && saved.Seen != nil {
			l.Day, l.Seen = saved.Day, saved.Seen
			if saved.Salt != "" {
				l.salt = saved.Salt
			}
		}
	}
	return l
}

func (l *limiter) key(ip string) string {
	h := sha256.Sum256([]byte(l.salt + "|" + ip))
	return hex.EncodeToString(h[:12])
}

// take uses `cost` checks for the visitor. It returns the new total and whether
// the request is allowed.
func (l *limiter) take(ip string, max, cost int) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	today := time.Now().UTC().Format("2006-01-02")
	if today != l.Day {
		// New day: fresh counts and a fresh salt, so old hashes can't be linked.
		l.Day, l.Seen, l.salt = today, map[string]int{}, randomHex(16)
	}
	k := l.key(ip)
	if max > 0 && l.Seen[k]+cost > max {
		return l.Seen[k], false
	}
	l.Seen[k] += cost
	l.save()
	return l.Seen[k], true
}

func (l *limiter) save() {
	if l.path == "" {
		return
	}
	b, _ := json.Marshal(limiterFile{l.Day, l.Seen, l.salt})
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("limits: can't save (%v); counts will reset on restart", err)
		l.path = "" // warn once, then keep working in memory
		return
	}
	os.Rename(tmp, l.path)
}

type limiterFile struct {
	Day  string         `json:"day"`
	Seen map[string]int `json:"seen"`
	Salt string         `json:"salt"`
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
