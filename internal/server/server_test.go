package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Scavenger503/drive-doctor/internal/ai"
)

// fakeAI stands in for Token Lab and records what it was sent.
func fakeAI(t *testing.T, got *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = string(b)
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"<think>hmm</think>**Back up now.** This drive is failing."}}],"usage":{"prompt_tokens":300,"completion_tokens":40}}`))
	}))
}

func post(t *testing.T, h http.Handler, body string) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest("POST", "/api/analyze", strings.NewReader(body))
	req.Header.Set("CF-Connecting-IP", "203.0.113.7")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func reportJSON(t *testing.T, file, lang string) string {
	b, err := os.ReadFile("../../testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(map[string]string{"report": string(b), "lang": lang})
	return string(j)
}

func TestAnalyzeEndToEnd(t *testing.T) {
	var sent string
	ts := fakeAI(t, &sent)
	defer ts.Close()

	s := New(ai.Config{BaseURL: ts.URL, APIKey: "test-key", Model: "DeepSeek-V4-Flash", Timeout: 5 * time.Second}, 2)
	h := s.Handler()

	rec, out := post(t, h, reportJSON(t, "failing_seagate.txt", "es"))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if out["verdict"].(map[string]any)["level"] != "replace" {
		t.Errorf("verdict = %v", out["verdict"])
	}
	exp := out["explanation"].(map[string]any)
	if exp["text"] != "Back up now. This drive is failing." || exp["fallback"] == true {
		t.Errorf("explanation = %v", exp)
	}
	// Serial must never reach the AI provider; language must be passed on.
	if strings.Contains(sent, "ZFL1XYZ9") {
		t.Error("serial number was sent to the AI")
	}
	if !strings.Contains(sent, "Spanish") {
		t.Error("language not passed to AI")
	}

	// Daily limit: 2 AI calls allowed, the 3rd falls back but still returns a verdict.
	post(t, h, reportJSON(t, "healthy_wd_red.txt", "en"))
	rec, out = post(t, h, reportJSON(t, "healthy_wd_red.txt", "en"))
	if rec.Code != 200 || out["explanation"].(map[string]any)["fallback"] != true {
		t.Errorf("expected fallback after limit, got %v", out["explanation"])
	}
	if out["verdict"].(map[string]any)["level"] != "healthy" {
		t.Errorf("verdict after limit = %v", out["verdict"])
	}
}

func TestCheckThenExplain(t *testing.T) {
	var sent string
	ts := fakeAI(t, &sent)
	defer ts.Close()
	h := New(ai.Config{BaseURL: ts.URL, APIKey: "test-key", Model: "m", Timeout: 5 * time.Second}, 1).Handler()
	body := reportJSON(t, "failing_seagate.txt", "en")

	call := func(path string) (int, map[string]any) {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// /api/check: verdict only, never calls the AI or uses the limit.
	for i := 0; i < 3; i++ {
		code, out := call("/api/check")
		if code != 200 || out["verdict"].(map[string]any)["level"] != "replace" || out["explanation"] != nil {
			t.Fatalf("check: %d %v", code, out)
		}
	}
	if sent != "" {
		t.Fatal("/api/check must not call the AI")
	}
	// /api/explain: AI text, limit of 1 then fallback.
	_, out := call("/api/explain")
	if out["explanation"].(map[string]any)["fallback"] == true {
		t.Fatalf("first explain should use AI: %v", out)
	}
	_, out = call("/api/explain")
	if out["explanation"].(map[string]any)["fallback"] != true {
		t.Fatalf("second explain should fall back: %v", out)
	}
}

func TestRejectsNonReport(t *testing.T) {
	h := New(ai.Config{}, 0).Handler()
	rec, out := post(t, h, `{"report":"hello world","lang":"en"}`)
	if rec.Code != http.StatusUnprocessableEntity || out["error"] == nil {
		t.Fatalf("want 422 with error, got %d %v", rec.Code, out)
	}
}

func TestNoKeyStillWorks(t *testing.T) {
	h := New(ai.Config{}, 0).Handler()
	rec, out := post(t, h, reportJSON(t, "cable_crc_x.txt", "en"))
	if rec.Code != 200 || out["verdict"].(map[string]any)["level"] != "watch" {
		t.Fatalf("got %d %v", rec.Code, out)
	}
	if out["explanation"].(map[string]any)["fallback"] != true {
		t.Error("expected fallback explanation without a key")
	}
}

func TestServesPageAndSamples(t *testing.T) {
	h := New(ai.Config{}, 0).Handler()
	for _, p := range []string{"/", "/samples/failing.txt"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 {
			t.Errorf("%s: status %d", p, rec.Code)
		}
	}
}

func TestSynologyWithoutDSat(t *testing.T) {
	h := New(ai.Config{}, 0).Handler()
	rec, out := post(t, h, reportJSON(t, "real/synology_no_dsat.txt", "en"))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "-d sat") {
		t.Fatalf("want -d sat hint, got %d %v", rec.Code, out)
	}
}

func TestLimitsSurviveRestartAndHideIPs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DD_DATA_DIR", dir)
	h := New(ai.Config{}, 3).Handler()
	post(t, h, reportJSON(t, "healthy_wd_red.txt", "en"))
	post(t, h, reportJSON(t, "healthy_wd_red.txt", "en"))

	b, err := os.ReadFile(dir + "/limits.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "203.0.113.7") {
		t.Fatal("raw IP stored in limits file")
	}
	// "Restart": a new server loads the saved counts.
	h2 := New(ai.Config{}, 3).Handler()
	_, out := post(t, h2, reportJSON(t, "healthy_wd_red.txt", "en"))
	if out["limit"].(map[string]any)["used"].(float64) != 3 {
		t.Fatalf("count didn't survive restart: %v", out["limit"])
	}
	_, out = post(t, h2, reportJSON(t, "healthy_wd_red.txt", "en"))
	if out["explanation"].(map[string]any)["fallback"] != true {
		t.Fatal("4th check should be over the limit")
	}
}

func TestScreenshotCostsTwo(t *testing.T) {
	l := newLimiter()
	if _, ok := l.take("1.2.3.4", 3, 2); !ok {
		t.Fatal("first screenshot should fit")
	}
	if _, ok := l.take("1.2.3.4", 3, 2); ok {
		t.Fatal("second screenshot (total 4) should exceed a limit of 3")
	}
	if _, ok := l.take("1.2.3.4", 3, 1); !ok {
		t.Fatal("a pasted check (total 3) should still fit")
	}
}
