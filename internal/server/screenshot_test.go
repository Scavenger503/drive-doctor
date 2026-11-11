package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Scavenger503/drive-doctor/internal/ai"
)

func tinyPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.White)
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func multipartBody(t *testing.T, files ...[]byte) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i, f := range files {
		fw, _ := mw.CreateFormFile("images", "shot"+string(rune('a'+i))+".png")
		fw.Write(f)
	}
	mw.WriteField("lang", "en")
	mw.Close()
	return &buf, mw.FormDataContentType()
}

// fakeVisionAI answers vision requests with a fixture and text requests with prose.
func fakeVisionAI(t *testing.T, fixture string) (*httptest.Server, *string) {
	fx, err := os.ReadFile("../../testdata/vision/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	var lastText string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		content := "Explanation text."
		if strings.Contains(string(b), "image_url") {
			content = string(fx)
		} else {
			lastText = string(b)
		}
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
		w.Write(out)
	}))
	return ts, &lastText
}

func TestScreenshotFlow(t *testing.T) {
	ts, lastText := fakeVisionAI(t, "ugreen_nvme_partial.json")
	defer ts.Close()
	s := New(ai.Config{BaseURL: ts.URL, APIKey: "k", Model: "m", Timeout: 5 * time.Second}, 5)
	s.Vision.AI = s.AI
	h := s.Handler()

	body, ct := multipartBody(t, tinyPNG(), tinyPNG())
	req := httptest.NewRequest("POST", "/api/screenshot", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	v := out["verdict"].(map[string]any)
	if v["level"] != "healthy" || len(v["incomplete"].([]any)) != 2 {
		t.Fatalf("verdict = %v", v)
	}
	id, _ := out["result_id"].(string)
	if len(id) != 32 {
		t.Fatalf("result_id = %q", id)
	}

	// Explanation by result_id, and the AI is told what's missing.
	ereq := httptest.NewRequest("POST", "/api/explain", strings.NewReader(`{"result_id":"`+id+`","lang":"en"}`))
	erec := httptest.NewRecorder()
	h.ServeHTTP(erec, ereq)
	if erec.Code != 200 || !strings.Contains(erec.Body.String(), "Explanation text.") {
		t.Fatalf("explain: %d %s", erec.Code, erec.Body.String())
	}
	if !strings.Contains(*lastText, "missing_values") {
		t.Fatal("AI was not told about the missing values")
	}

	// Unknown id -> 404.
	ereq = httptest.NewRequest("POST", "/api/explain", strings.NewReader(`{"result_id":"nope"}`))
	erec = httptest.NewRecorder()
	h.ServeHTTP(erec, ereq)
	if erec.Code != 404 {
		t.Fatalf("want 404, got %d", erec.Code)
	}
}

func TestScreenshotRejectsNonImage(t *testing.T) {
	ts, _ := fakeVisionAI(t, "ugreen_nvme_partial.json")
	defer ts.Close()
	s := New(ai.Config{BaseURL: ts.URL, APIKey: "k", Model: "m", Timeout: time.Second}, 5)
	s.Vision.AI = s.AI
	body, ct := multipartBody(t, []byte("#!/bin/sh\necho not an image"))
	req := httptest.NewRequest("POST", "/api/screenshot", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestScreenshotNeedsKey(t *testing.T) {
	body, ct := multipartBody(t, tinyPNG())
	req := httptest.NewRequest("POST", "/api/screenshot", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	New(ai.Config{}, 5).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
}

// twoReaders returns different transcriptions per model, like tonight's real run.
func twoReaders(t *testing.T, byModel map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct{ Model string }
		json.Unmarshal(b, &req)
		content := "Explanation."
		if strings.Contains(string(b), "image_url") {
			content = byModel[req.Model]
		}
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
		w.Write(out)
	}))
}

const nvmeTmpl = `{"kind":"nvme","is_ssd":true,"nvme":{"critical_warning":0,"temperature_c":33,"available_spare":100,"available_spare_threshold":10,"percentage_used":3,"media_errors":%s,
"labels":{"critical_warning":"Critical warning","temperature_c":"Temperature","available_spare":"Available spare space","available_spare_threshold":"Available spare threshold","percentage_used":"Percentage of rated lifetime used","media_errors":"Media and data integrity errors"}}}`

func screenshotVerdict(t *testing.T, byModel map[string]string) map[string]any {
	ts := twoReaders(t, byModel)
	t.Cleanup(ts.Close)
	s := New(ai.Config{BaseURL: ts.URL, APIKey: "k", Model: "text", Timeout: 5 * time.Second}, 10)
	s.Vision.AI, s.Vision.Model, s.Vision.BackupModel = s.AI, "readerA", "readerB"
	body, ct := multipartBody(t, tinyPNG())
	req := httptest.NewRequest("POST", "/api/screenshot", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestSecondOpinionDisagreesDropsWarning(t *testing.T) {
	out := screenshotVerdict(t, map[string]string{
		"readerA": strings.Replace(nvmeTmpl, "%s", "4678", 1),
		"readerB": strings.Replace(nvmeTmpl, "%s", "0", 1),
	})
	v := out["verdict"].(map[string]any)
	if v["level"] == "replace" {
		t.Fatalf("disagreeing reads must not produce replace: %v", v)
	}
	if u, _ := v["unconfirmed"].([]any); len(u) != 1 {
		t.Fatalf("want 1 unconfirmed finding, got %v", v["unconfirmed"])
	}
	if out["confirmed_by"] != "readerB" {
		t.Fatalf("confirmed_by = %v", out["confirmed_by"])
	}
}

func TestSecondOpinionAgreesKeepsWarning(t *testing.T) {
	bad := strings.Replace(nvmeTmpl, "%s", "24", 1)
	out := screenshotVerdict(t, map[string]string{"readerA": bad, "readerB": bad})
	v := out["verdict"].(map[string]any)
	if v["level"] != "replace" || v["unconfirmed"] != nil {
		t.Fatalf("agreeing reads should keep replace: %v", v)
	}
}

func TestHealthyScreenshotSkipsSecondRead(t *testing.T) {
	good := strings.Replace(nvmeTmpl, "%s", "0", 1)
	out := screenshotVerdict(t, map[string]string{"readerA": good, "readerB": "not json"})
	if out["confirmed_by"] != nil || out["verdict"].(map[string]any)["level"] != "healthy" {
		t.Fatalf("healthy result shouldn't need a second read: %v", out)
	}
}
