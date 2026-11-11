package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Scavenger503/drive-doctor/internal/smart"
	"github.com/Scavenger503/drive-doctor/internal/verdict"
)

// slowMain simulates Token Lab: the main model hangs, the backup answers.
func slowMain(t *testing.T, mainDown bool) (*httptest.Server, *[]string) {
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct{ Model string }
		json.Unmarshal(b, &req)
		calls = append(calls, req.Model)
		if req.Model == "main" && mainDown {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"Answer from ` + req.Model + `"}}],"usage":{}}`))
	}))
	return ts, &calls
}

func testInput() (*smart.Report, verdict.Result) {
	return &smart.Report{Model: "X"}, verdict.Result{Level: verdict.Healthy}
}

func TestBackupModelOnTimeout(t *testing.T) {
	ts, calls := slowMain(t, true)
	defer ts.Close()
	cfg := Config{BaseURL: ts.URL, APIKey: "k", Model: "main", BackupModel: "backup", Timeout: 200 * time.Millisecond}
	rep, res := testInput()
	exp, err := Explain(context.Background(), cfg, rep, res, "en")
	if err != nil || exp.Fallback || exp.Model != "backup" || exp.Text != "Answer from backup" {
		t.Fatalf("got %+v err=%v", exp, err)
	}
	if strings.Join(*calls, ",") != "main,backup" {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestMainModelUsedWhenHealthy(t *testing.T) {
	ts, calls := slowMain(t, false)
	defer ts.Close()
	cfg := Config{BaseURL: ts.URL, APIKey: "k", Model: "main", BackupModel: "backup", Timeout: time.Second}
	rep, res := testInput()
	exp, _ := Explain(context.Background(), cfg, rep, res, "en")
	if exp.Model != "main" || len(*calls) != 1 {
		t.Fatalf("got %+v calls=%v", exp, *calls)
	}
}

func TestBothFailGivesFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer ts.Close()
	cfg := Config{BaseURL: ts.URL, APIKey: "k", Model: "main", BackupModel: "backup", Timeout: time.Second}
	rep, res := testInput()
	exp, err := Explain(context.Background(), cfg, rep, res, "en")
	if err == nil || !exp.Fallback || exp.Text == "" {
		t.Fatalf("got %+v err=%v", exp, err)
	}
}
