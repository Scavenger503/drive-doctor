// Package ai turns the rule-based verdict into a plain-language explanation
// using any OpenAI-compatible chat API (AMD Token Lab by default).
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Scavenger503/drive-doctor/internal/smart"
	"github.com/Scavenger503/drive-doctor/internal/verdict"
)

// Config selects the provider. Swap BaseURL/Model to move off Token Lab.
type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	// BackupModel is tried once if Model fails or times out ("" = no backup).
	BackupModel string
	// Timeout is per attempt.
	Timeout time.Duration
}

// ConfigFromEnv reads DD_AI_* variables, falling back to OPENAI_* ones.
func ConfigFromEnv() Config {
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" {
				return v
			}
		}
		return ""
	}
	c := Config{
		BaseURL:     get("DD_AI_BASE_URL", "OPENAI_BASE_URL"),
		APIKey:      get("DD_AI_API_KEY", "OPENAI_API_KEY"),
		Model:       get("DD_AI_MODEL"),
		BackupModel: get("DD_AI_BACKUP_MODEL"),
		Timeout:     25 * time.Second,
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://api.tokenlab.amd.com/v1"
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.Model == "" {
		c.Model = "DeepSeek-V4-Flash"
	}
	if c.BackupModel == "" && os.Getenv("DD_AI_BACKUP_MODEL") == "" {
		c.BackupModel = "Gemma-4-31B-IT"
	}
	if strings.EqualFold(c.BackupModel, "none") || c.BackupModel == c.Model {
		c.BackupModel = ""
	}
	return c
}

// Enabled reports whether an API key is configured.
func (c Config) Enabled() bool { return c.APIKey != "" }

// Explanation is what the model returned.
type Explanation struct {
	Text      string  `json:"text"`
	Model     string  `json:"model"`
	Seconds   float64 `json:"seconds"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	Fallback  bool    `json:"fallback"` // true when the AI was unavailable
}

const systemPrompt = `You explain hard drive and SSD health reports to people who are not technical.

You are given the drive details and the verdict and findings from a rule engine. The verdict is final: never change it, soften it, or contradict it.

Strict rules:
- Use only the facts in the findings and notes. Never invent causes, numbers, or reassurances that are not there.
- Only say a problem is probably not the drive itself (for example a cable or cooling) when that finding has "drive_ok": true. Otherwise never suggest the cause is something other than the drive.
- Never mention the words "verdict", "healthy", "watch", or "replace" as labels, and never put them in quotes. Describe the situation in plain words instead.
- Notes are background details, not problems, and never mean the check is incomplete. Never lead with a note, and never make a note sound like something went wrong. If there are no findings, you may mention a note briefly at most, or leave it out.
- If "missing_values" is present, the check is not complete: say clearly that some important values weren't visible, so you can't fully confirm the drive is fine yet, and ask them to add a screenshot that shows those values. Do not call the drive healthy or fine without that caveat.
- If "unreadable_values" is present, those values could not be read reliably from the screenshot. They are NOT problems with the drive: do not warn about them or call them errors. Say that a few values couldn't be read clearly and suggest a sharper screenshot or pasting the full report.
- No jargon, no markdown, no lists.

Write 3 to 5 short, friendly sentences:
1. Say clearly what this means for the drive.
2. Explain the most important finding in everyday terms.
3. Give the next step. If the drive needs replacing, tell them to back up their files right away before anything else, then replace it. If it needs watching, suggest keeping a backup and checking again in a few weeks. If it is fine, reassure them and remind them that backups still matter.`

// Explain asks the model to explain the verdict in the given language ("en" or "es").
// If the main model fails or times out, the backup model gets one try; if that
// fails too, a rule-based fallback is returned along with the error.
func Explain(ctx context.Context, cfg Config, rep *smart.Report, res verdict.Result, lang string) (Explanation, error) {
	if !cfg.Enabled() {
		return Fallback(res, lang), errors.New("no API key configured")
	}
	models := []string{cfg.Model}
	if cfg.BackupModel != "" {
		models = append(models, cfg.BackupModel)
	}
	var errs []error
	for _, m := range models {
		if ctx.Err() != nil {
			break // the visitor left; don't keep calling the API
		}
		exp, err := explainOnce(ctx, cfg, m, rep, res, lang)
		if err == nil {
			return exp, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", m, err))
		log.Printf("%s: %s failed, trying next model if any: %v", "ai", m, err)
	}
	return Fallback(res, lang), errors.Join(errs...)
}

func explainOnce(ctx context.Context, cfg Config, model string, rep *smart.Report, res verdict.Result, lang string) (Explanation, error) {

	payload := map[string]any{
		"drive": map[string]any{
			"model": rep.Model, "family": rep.Family, "capacity": rep.Capacity,
			"type": map[bool]string{true: "SSD", false: "hard drive"}[rep.IsSSD],
		},
		"verdict":  res.Level.String(),
		"findings": res.Findings,
		"notes":    res.Notes,
	}
	if len(res.Incomplete) > 0 {
		payload["missing_values"] = res.Incomplete
	}
	if len(res.Unconfirmed) > 0 {
		var t []string
		for _, f := range res.Unconfirmed {
			t = append(t, f.Title)
		}
		payload["unreadable_values"] = t
	}
	input, _ := json.Marshal(payload)

	language := "English"
	if lang == "es" {
		language = "Spanish (neutral Latin American Spanish)"
	}

	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": fmt.Sprintf("Answer in %s.\n\n%s", language, input)},
		},
		"max_tokens":  400,
		"temperature": 0.2,
	})

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Explanation{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Explanation{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Explanation{}, fmt.Errorf("AI API returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return Explanation{}, fmt.Errorf("unexpected AI response: %s", truncate(string(raw), 200))
	}
	text := cleanText(out.Choices[0].Message.Content)
	if text == "" {
		return Explanation{}, errors.New("empty AI response")
	}
	return Explanation{
		Text:      text,
		Model:     model,
		Seconds:   time.Since(start).Round(10 * time.Millisecond).Seconds(),
		TokensIn:  out.Usage.PromptTokens,
		TokensOut: out.Usage.CompletionTokens,
	}, nil
}

// Fallback builds a simple explanation without AI, so the app still works
// when the provider is down or access has ended.
func Fallback(res verdict.Result, lang string) Explanation {
	var b strings.Builder
	es := lang == "es"
	switch res.Level {
	case verdict.Replace:
		if es {
			b.WriteString("Este disco muestra señales serias de falla. Haz una copia de seguridad de tus archivos ahora mismo y luego reemplaza el disco.")
		} else {
			b.WriteString("This drive shows serious signs of failure. Back up your files right away, then replace the drive.")
		}
	case verdict.Watch:
		if es {
			b.WriteString("Este disco funciona, pero tiene señales de advertencia. Mantén una copia de seguridad y vuelve a revisarlo en unas semanas.")
		} else {
			b.WriteString("This drive is working, but it shows some warning signs. Keep a backup and check it again in a few weeks.")
		}
	default:
		if len(res.Incomplete) > 0 || len(res.Unconfirmed) > 0 {
			if es {
				b.WriteString("No vimos nada preocupante en los valores visibles.")
			} else {
				b.WriteString("Nothing worrying in the values we could see.")
			}
			break
		}
		if es {
			b.WriteString("Este disco se ve sano. Aun así, las copias de seguridad siempre son importantes.")
		} else {
			b.WriteString("This drive looks healthy. Backups still matter, though.")
		}
	}
	if len(res.Incomplete) > 0 || len(res.Unconfirmed) > 0 {
		if es {
			b.WriteString(" Ojo: no se vieron todos los valores importantes, así que la revisión no está completa. Agrega una captura que muestre los que faltan.")
		} else {
			b.WriteString(" Note: not all of the important values were visible, so this check isn't complete. Add a screenshot that shows the missing ones.")
		}
	}
	if len(res.Findings) > 0 && !es {
		b.WriteString(" Main finding: " + res.Findings[0].Detail)
	}
	return Explanation{Text: b.String(), Fallback: true}
}

// cleanText strips hidden reasoning blocks and markdown some models add.
func cleanText(s string) string {
	if i := strings.Index(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	s = strings.ReplaceAll(s, "**", "")
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
