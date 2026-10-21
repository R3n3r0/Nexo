package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/text/language"
)

// OpenAIConfig configures an OpenAI-compatible transcription service.
type OpenAIConfig struct {
	// URL is the transcription endpoint
	// (https://api.openai.com/v1/audio/transcriptions, or the
	// /inference of a whisper.cpp server).
	URL string
	// APIKey is sent as a bearer token (empty: none).
	APIKey string
	// Model names the model (default whisper-1).
	Model string
	// HTTP performs the requests (default a client with a 30 minutes
	// timeout).
	HTTP *http.Client
}

type openAITranscriber struct {
	cfg OpenAIConfig
	c   client
}

// NewOpenAITranscriber returns the Transcriber of an OpenAI-compatible
// service: a multipart POST of the audio, verbose_json answers with timed
// segments.
func NewOpenAITranscriber(cfg OpenAIConfig) (Transcriber, error) {
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return nil, fmt.Errorf("speech: the transcription URL %q must be an http(s) URL", cfg.URL)
	}
	if cfg.Model == "" {
		cfg.Model = "whisper-1"
	}
	return &openAITranscriber{cfg: cfg, c: newClient(cfg.HTTP, cfg.APIKey, 30*time.Minute)}, nil
}

func (t *openAITranscriber) Engine() string { return "openai:" + t.cfg.Model }

// verboseJSON is the verbose_json answer of a transcription.
type verboseJSON struct {
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
	Text     string  `json:"text"`
	Segments []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

func (t *openAITranscriber) Transcribe(ctx context.Context, path, lang string) (*Transcript, error) {
	audio, err := os.ReadFile(path) // #nosec G304 -- a file of the job directory
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(path)))
	h.Set("Content-Type", "audio/mp4")
	part, err := mw.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(audio); err != nil {
		return nil, err
	}
	fields := [][2]string{{"model", t.cfg.Model}, {"response_format", "verbose_json"}, {"timestamp_granularities[]", "segment"}, {"temperature", "0"}}
	if lang != "" {
		fields = append(fields, [2]string{"language", whisperLanguage(lang)})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	data, err := t.c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, t.cfg.URL, bytes.NewReader(body.Bytes()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	var v verboseJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("%w: the transcription is not verbose_json: %w", ErrPermanent, err)
	}
	out := &Transcript{Language: languageTag(v.Language)}
	for _, s := range v.Segments {
		if text := strings.TrimSpace(s.Text); text != "" && s.End > s.Start {
			out.Segments = append(out.Segments, Segment{Start: s.Start, End: s.End, Text: text})
		}
	}
	if len(out.Segments) == 0 && strings.TrimSpace(v.Text) != "" && v.Duration > 0 {
		out.Segments = []Segment{{Start: 0, End: v.Duration, Text: strings.TrimSpace(v.Text)}}
	}
	return out, nil
}

// whisperLanguage is the language parameter of a transcription: the ISO
// 639-1 code Whisper knows.
func whisperLanguage(tag string) string {
	t, err := language.Parse(tag)
	if err != nil {
		return tag
	}
	base, _ := t.Base()
	return base.String()
}

// languageTag maps the language a service reports (a code, or the English
// name Whisper uses: "english") to a BCP 47 tag; "" when unknown.
func languageTag(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if code, ok := whisperNames()[s]; ok {
		s = code
	}
	t, err := language.Parse(s)
	if err != nil || t == language.Und {
		return ""
	}
	return t.String()
}

// whisperNames maps the English names of the languages Whisper reports in
// verbose_json to their codes.
func whisperNames() map[string]string {
	return map[string]string{
		"afrikaans": "af", "albanian": "sq", "amharic": "am", "arabic": "ar", "armenian": "hy", "assamese": "as",
		"azerbaijani": "az", "bashkir": "ba", "basque": "eu", "belarusian": "be", "bengali": "bn", "bosnian": "bs",
		"breton": "br", "bulgarian": "bg", "burmese": "my", "cantonese": "yue", "castilian": "es", "catalan": "ca",
		"chinese": "zh", "croatian": "hr", "czech": "cs", "danish": "da", "dutch": "nl", "english": "en",
		"estonian": "et", "faroese": "fo", "finnish": "fi", "flemish": "nl", "french": "fr", "galician": "gl",
		"georgian": "ka", "german": "de", "greek": "el", "gujarati": "gu", "haitian": "ht", "haitian creole": "ht",
		"hausa": "ha", "hawaiian": "haw", "hebrew": "he", "hindi": "hi", "hungarian": "hu", "icelandic": "is",
		"indonesian": "id", "italian": "it", "japanese": "ja", "javanese": "jv", "kannada": "kn", "kazakh": "kk",
		"khmer": "km", "korean": "ko", "lao": "lo", "latin": "la", "latvian": "lv", "letzeburgesch": "lb",
		"lingala": "ln", "lithuanian": "lt", "luxembourgish": "lb", "macedonian": "mk", "malagasy": "mg",
		"malay": "ms", "malayalam": "ml", "maltese": "mt", "maori": "mi", "marathi": "mr", "moldavian": "ro",
		"moldovan": "ro", "mongolian": "mn", "myanmar": "my", "nepali": "ne", "norwegian": "no", "nynorsk": "nn",
		"occitan": "oc", "panjabi": "pa", "pashto": "ps", "persian": "fa", "polish": "pl", "portuguese": "pt",
		"punjabi": "pa", "pushto": "ps", "romanian": "ro", "russian": "ru", "sanskrit": "sa", "serbian": "sr",
		"shona": "sn", "sindhi": "sd", "sinhala": "si", "sinhalese": "si", "slovak": "sk", "slovenian": "sl",
		"somali": "so", "spanish": "es", "sundanese": "su", "swahili": "sw", "swedish": "sv", "tagalog": "tl",
		"tajik": "tg", "tamil": "ta", "tatar": "tt", "telugu": "te", "thai": "th", "tibetan": "bo", "turkish": "tr",
		"turkmen": "tk", "ukrainian": "uk", "urdu": "ur", "uzbek": "uz", "valencian": "ca", "vietnamese": "vi",
		"welsh": "cy", "yiddish": "yi", "yoruba": "yo",
	}
}
