package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

// translateBatch bounds the lines of a translation request.
const translateBatch = 40

// LibreTranslateConfig configures a LibreTranslate service.
type LibreTranslateConfig struct {
	// URL is the translate endpoint (http://libretranslate:5000/translate).
	URL string
	// APIKey is sent as api_key (empty: none).
	APIKey string
	HTTP   *http.Client
}

type libreTranslator struct {
	cfg LibreTranslateConfig
	c   client
}

// NewLibreTranslator returns the Translator of a LibreTranslate service.
func NewLibreTranslator(cfg LibreTranslateConfig) (Translator, error) {
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return nil, fmt.Errorf("speech: the translation URL %q must be an http(s) URL", cfg.URL)
	}
	// The key travels in the body, not as a bearer token.
	return &libreTranslator{cfg: cfg, c: newClient(cfg.HTTP, "", 5*time.Minute)}, nil
}

func (t *libreTranslator) Engine() string { return "libretranslate" }

// libreLanguage is the code of a language for LibreTranslate: the base
// language, and the script of Chinese (zh-Hans, zh-Hant).
func libreLanguage(tag string) string {
	t, err := language.Parse(tag)
	if err != nil {
		return tag
	}
	base, _ := t.Base()
	if base.String() == "zh" {
		if script, _ := t.Script(); script.String() == "Hant" {
			return "zt"
		}
	}
	return base.String()
}

func (t *libreTranslator) Translate(ctx context.Context, texts []string, from, to string) ([]string, error) {
	return batches(texts, func(batch []string) ([]string, error) {
		in := map[string]any{"q": batch, "source": libreLanguage(from), "target": libreLanguage(to), "format": "text"}
		if t.cfg.APIKey != "" {
			in["api_key"] = t.cfg.APIKey
		}
		data, err := t.c.do(ctx, jsonRequest(t.cfg.URL, in))
		if err != nil {
			return nil, err
		}
		var out struct {
			TranslatedText []string `json:"translatedText"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("%w: LibreTranslate answered %w", ErrPermanent, err)
		}
		return out.TranslatedText, nil
	})
}

// ChatConfig configures an OpenAI-compatible chat completion service used
// to translate (OpenAI, Ollama, vLLM, LocalAI...).
type ChatConfig struct {
	// URL is the chat completions endpoint
	// (https://api.openai.com/v1/chat/completions).
	URL    string
	APIKey string
	// Model names the model (required).
	Model string
	HTTP  *http.Client
}

type chatTranslator struct {
	cfg ChatConfig
	c   client
}

// NewChatTranslator returns the Translator of an OpenAI-compatible chat
// completion service: the lines go as a JSON array and must come back as
// a JSON array of the same length.
func NewChatTranslator(cfg ChatConfig) (Translator, error) {
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return nil, fmt.Errorf("speech: the translation URL %q must be an http(s) URL", cfg.URL)
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("speech: the translation model is required")
	}
	return &chatTranslator{cfg: cfg, c: newClient(cfg.HTTP, cfg.APIKey, 10*time.Minute)}, nil
}

func (t *chatTranslator) Engine() string { return "chat:" + t.cfg.Model }

// languageName is the English name of a language ("Italian").
func languageName(tag string) string {
	t, err := language.Parse(tag)
	if err != nil {
		return tag
	}
	if n := display.English.Tags().Name(t); n != "" {
		return n
	}
	return tag
}

func (t *chatTranslator) Translate(ctx context.Context, texts []string, from, to string) ([]string, error) {
	system := fmt.Sprintf("You translate video subtitles from %s to %s. The user sends a JSON array of subtitle lines. "+
		"Reply with only a JSON array of strings: the translation of each line, in the same order, as many as the input. "+
		"Keep line breaks and WebVTT tags such as <i> and <b>. Do not merge or split lines.", languageName(from), languageName(to))
	return batches(texts, func(batch []string) ([]string, error) {
		user, err := json.Marshal(batch)
		if err != nil {
			return nil, err
		}
		in := map[string]any{"model": t.cfg.Model, "temperature": 0, "messages": []map[string]string{
			{"role": "system", "content": system}, {"role": "user", "content": string(user)},
		}}
		data, err := t.c.do(ctx, jsonRequest(t.cfg.URL, in))
		if err != nil {
			return nil, err
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &out); err != nil || len(out.Choices) == 0 {
			return nil, fmt.Errorf("%w: the chat service answered no choice", ErrPermanent)
		}
		content := strings.TrimSpace(out.Choices[0].Message.Content)
		content = strings.TrimPrefix(strings.TrimPrefix(content, "```json"), "```")
		content = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(content), "```"))
		var lines []string
		if err := json.Unmarshal([]byte(content), &lines); err != nil {
			return nil, fmt.Errorf("%w: the chat service answered no JSON array of strings", ErrPermanent)
		}
		return lines, nil
	})
}

// batches translates texts in batches of translateBatch lines; a batch
// answered with another number of lines fails.
func batches(texts []string, translate func([]string) ([]string, error)) ([]string, error) {
	out := make([]string, 0, len(texts))
	for i := 0; i < len(texts); i += translateBatch {
		batch := texts[i:min(i+translateBatch, len(texts))]
		got, err := translate(batch)
		if err != nil {
			return nil, err
		}
		if len(got) != len(batch) {
			return nil, fmt.Errorf("%w: %d lines translated into %d", ErrPermanent, len(batch), len(got))
		}
		out = append(out, got...)
	}
	return out, nil
}

// jsonRequest builds a JSON POST.
func jsonRequest(url string, body any) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	}
}
