package speech

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func audioFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audio-0.m4a")
	require.NoError(t, os.WriteFile(path, []byte("m4a bytes"), 0o600))
	return path
}

func TestOpenAITranscriber(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable) // retried
			return
		}
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		require.NoError(t, r.ParseMultipartForm(1<<20))
		assert.Equal(t, "whisper-large", r.FormValue("model"))
		assert.Equal(t, "verbose_json", r.FormValue("response_format"))
		assert.Equal(t, "pt", r.FormValue("language"))
		f, h, err := r.FormFile("file")
		require.NoError(t, err)
		data, _ := io.ReadAll(f)
		assert.Equal(t, "m4a bytes", string(data))
		assert.Equal(t, "audio-0.m4a", h.Filename)
		_, _ = io.WriteString(w, `{"language":"portuguese","duration":4,"text":"Olá. Tudo bem?",
			"segments":[{"start":0,"end":1.5,"text":" Olá."},{"start":1.5,"end":1.5,"text":"empty span"},{"start":2,"end":3.9,"text":" Tudo bem? "}]}`)
	}))
	defer srv.Close()
	tr, err := NewOpenAITranscriber(OpenAIConfig{URL: srv.URL, APIKey: "sk-test", Model: "whisper-large"})
	require.NoError(t, err)
	tr.(*openAITranscriber).c.backoff = time.Millisecond
	assert.Equal(t, "openai:whisper-large", tr.Engine())
	got, err := tr.Transcribe(context.Background(), audioFile(t), "pt-BR")
	require.NoError(t, err)
	assert.Equal(t, &Transcript{Language: "pt", Segments: []Segment{{0, 1.5, "Olá."}, {2, 3.9, "Tudo bem?"}}}, got)
	assert.EqualValues(t, 2, calls.Load())
}

func TestTranscriberErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("bad") != "" {
			http.Error(w, `{"error":"invalid file"}`, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"language":"en","duration":2,"text":" Just text. "}`)
	}))
	defer srv.Close()
	tr, err := NewOpenAITranscriber(OpenAIConfig{URL: srv.URL + "?bad=1"})
	require.NoError(t, err)
	_, err = tr.Transcribe(context.Background(), audioFile(t), "")
	require.ErrorIs(t, err, ErrPermanent, "a 400 is not retried")
	assert.NotContains(t, err.Error(), "bad=1", "the query is redacted")

	tr, err = NewOpenAITranscriber(OpenAIConfig{URL: srv.URL})
	require.NoError(t, err)
	got, err := tr.Transcribe(context.Background(), audioFile(t), "")
	require.NoError(t, err)
	assert.Equal(t, &Transcript{Language: "en", Segments: []Segment{{0, 2, "Just text."}}}, got, "a text without segments spans the audio")

	_, err = NewOpenAITranscriber(OpenAIConfig{URL: "ftp://x"})
	require.Error(t, err)
}

func TestLanguageTag(t *testing.T) {
	for in, want := range map[string]string{"english": "en", "Italian": "it", "en": "en", "pt-BR": "pt-BR", "": "", "klingon": ""} {
		assert.Equal(t, want, languageTag(in), in)
	}
}

func TestLibreTranslator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Q      []string `json:"q"`
			Source string   `json:"source"`
			Target string   `json:"target"`
			APIKey string   `json:"api_key"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "en", in.Source)
		assert.Equal(t, "zt", in.Target)
		assert.Equal(t, "lt-key", in.APIKey)
		out := make([]string, len(in.Q))
		for i, q := range in.Q {
			out[i] = strings.ToUpper(q)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"translatedText": out})
	}))
	defer srv.Close()
	tr, err := NewLibreTranslator(LibreTranslateConfig{URL: srv.URL, APIKey: "lt-key"})
	require.NoError(t, err)
	texts := make([]string, 95)
	for i := range texts {
		texts[i] = "line " + string(rune('a'+i%26))
	}
	got, err := tr.Translate(context.Background(), texts, "en-US", "zh-Hant")
	require.NoError(t, err)
	require.Len(t, got, 95, "in batches of 40")
	assert.Equal(t, "LINE A", got[0])
	assert.Equal(t, "LINE Q", got[94])
}

func TestChatTranslator(t *testing.T) {
	var reply string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model    string `json:"model"`
			Messages []struct {
				Role, Content string
			} `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "gpt-test", in.Model)
		assert.Contains(t, in.Messages[0].Content, "from English to Italian")
		assert.JSONEq(t, `["Hello","<i>world</i>"]`, in.Messages[1].Content)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": reply}}}})
	}))
	defer srv.Close()
	tr, err := NewChatTranslator(ChatConfig{URL: srv.URL, Model: "gpt-test"})
	require.NoError(t, err)
	assert.Equal(t, "chat:gpt-test", tr.Engine())

	reply = "```json\n[\"Ciao\",\"<i>mondo</i>\"]\n```"
	got, err := tr.Translate(context.Background(), []string{"Hello", "<i>world</i>"}, "en", "it")
	require.NoError(t, err)
	assert.Equal(t, []string{"Ciao", "<i>mondo</i>"}, got)

	reply = `["Ciao mondo"]`
	_, err = tr.Translate(context.Background(), []string{"Hello", "<i>world</i>"}, "en", "it")
	require.ErrorIs(t, err, ErrPermanent, "another number of lines")
	reply = "Ciao"
	_, err = tr.Translate(context.Background(), []string{"Hello", "<i>world</i>"}, "en", "it")
	require.ErrorIs(t, err, ErrPermanent)

	_, err = NewChatTranslator(ChatConfig{URL: srv.URL})
	require.Error(t, err, "a model is required")
}
