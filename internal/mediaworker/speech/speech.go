// Package speech is the speech side of the media worker: it transcribes
// the audio of the videos with an OpenAI-compatible transcription service
// (OpenAI, whisper.cpp server, faster-whisper-server...) and translates
// the transcriptions with LibreTranslate or an OpenAI-compatible chat
// completion service. Nothing runs locally: the services are configured
// by URL.
package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrPermanent marks a failure that a retry does not fix (a request the
// service refuses, an answer it cannot make).
var ErrPermanent = errors.New("speech: permanent failure")

// Segment is a timed part of a transcription, in seconds from the start of
// the audio.
type Segment struct {
	Start float64
	End   float64
	Text  string
}

// Transcript is the transcription of an audio file.
type Transcript struct {
	// Language is the BCP 47 tag of the spoken language ("" when the
	// service did not report it).
	Language string
	Segments []Segment
}

// Transcriber transcribes audio files.
type Transcriber interface {
	// Transcribe transcribes an audio file (M4A); lang is the spoken
	// language, "" to detect it.
	Transcribe(ctx context.Context, path, lang string) (*Transcript, error)
	// Engine names the service and model ("openai:whisper-1").
	Engine() string
}

// Translator translates subtitle lines.
type Translator interface {
	// Translate translates texts from a language to another (BCP 47
	// tags), one output per input, in order.
	Translate(ctx context.Context, texts []string, from, to string) ([]string, error)
	// Engine names the service (and model).
	Engine() string
}

// maxResponse bounds the answers of the services.
const maxResponse = 32 << 20

// attempts bounds the attempts of a request that fails for a transient
// reason (network, 429, 5xx).
const attempts = 3

// client sends the requests of a service.
type client struct {
	http    *http.Client
	apiKey  string
	backoff time.Duration
}

func newClient(c *http.Client, apiKey string, timeout time.Duration) client {
	if c == nil {
		c = &http.Client{Timeout: timeout}
	}
	return client{http: c, apiKey: apiKey, backoff: time.Second}
}

// do sends a request built by build (once per attempt) and returns the
// body of a 2xx answer.
func (c client) do(ctx context.Context, build func() (*http.Request, error)) ([]byte, error) {
	var last error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.backoff << (attempt - 1)):
			}
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
		_ = resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return body, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500:
			last = fmt.Errorf("speech: %s %s: %s", req.Method, redact(req), status(resp, body))
		default:
			return nil, fmt.Errorf("%w: %s %s: %s", ErrPermanent, req.Method, redact(req), status(resp, body))
		}
	}
	return nil, last
}

// redact names the URL of a request without its query (an API key may
// travel there).
func redact(req *http.Request) string {
	u := *req.URL
	u.RawQuery, u.User = "", nil
	return u.String()
}

// status describes an error answer: its status and the start of its body.
func status(resp *http.Response, body []byte) string {
	msg := strings.TrimSpace(string(bytes.ToValidUTF8(body, nil)))
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	if msg == "" {
		return resp.Status
	}
	return resp.Status + ": " + msg
}
