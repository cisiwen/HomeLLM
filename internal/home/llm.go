package home

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Image is a decoded image attached to a message.
type Image struct {
	MIME string
	Data []byte
}

// LLMMessage is one conversation turn with its images already resolved.
type LLMMessage struct {
	Role    string
	Content string
	Images  []Image
}

// Backend generates a streamed reply. onDelta is called for each piece of text in order;
// returning an error from it stops generation.
type Backend interface {
	Stream(ctx context.Context, model string, msgs []LLMMessage, onDelta func(string) error) error
	DefaultModel() string
	Vision() bool
}

// Ollama talks to Ollama's OpenAI-compatible endpoint. Any server exposing
// /v1/chat/completions with SSE streaming (llama.cpp's llama-server, vLLM, LM Studio) works too.
type Ollama struct {
	BaseURL string // e.g. http://127.0.0.1:11434
	Model   string
	// HasVision says whether Model accepts images; it's reported to the phone so it can hide
	// the attach button. Ollama doesn't expose this through the OpenAI API.
	HasVision bool
	Client    *http.Client
}

func (o *Ollama) DefaultModel() string { return o.Model }
func (o *Ollama) Vision() bool         { return o.HasVision }

func (o *Ollama) Stream(ctx context.Context, model string, msgs []LLMMessage, onDelta func(string) error) error {
	if model == "" {
		model = o.Model
	}
	type imageURL struct {
		URL string `json:"url"`
	}
	type part struct {
		Type     string    `json:"type"`
		Text     string    `json:"text,omitempty"`
		ImageURL *imageURL `json:"image_url,omitempty"`
	}
	type message struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	body := struct {
		Model    string    `json:"model"`
		Stream   bool      `json:"stream"`
		Messages []message `json:"messages"`
	}{Model: model, Stream: true}
	for _, m := range msgs {
		if len(m.Images) == 0 {
			body.Messages = append(body.Messages, message{Role: m.Role, Content: m.Content})
			continue
		}
		parts := []part{{Type: "text", Text: m.Content}}
		for _, img := range m.Images {
			uri := "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
			parts = append(parts, part{Type: "image_url", ImageURL: &imageURL{URL: uri}})
		}
		body.Messages = append(body.Messages, message{Role: m.Role, Content: parts})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("calling LLM: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("LLM returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			return nil
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Error != nil {
			return fmt.Errorf("LLM error: %s", ev.Error.Message)
		}
		if len(ev.Choices) > 0 && ev.Choices[0].Delta.Content != "" {
			if err := onDelta(ev.Choices[0].Delta.Content); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// Mock echoes the last user message back word by word. It lets the whole pipeline (pairing,
// WebRTC, streaming, images, cancel) be tested without installing a model.
type Mock struct{}

func (Mock) DefaultModel() string { return "mock" }
func (Mock) Vision() bool         { return true }

func (Mock) Stream(ctx context.Context, _ string, msgs []LLMMessage, onDelta func(string) error) error {
	last := LLMMessage{}
	if len(msgs) > 0 {
		last = msgs[len(msgs)-1]
	}
	reply := "You said: " + last.Content
	for i, img := range last.Images {
		reply += fmt.Sprintf(" [image %d: %s, %d bytes]", i+1, img.MIME, len(img.Data))
	}
	for i, w := range strings.SplitAfter(reply, " ") {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(40 * time.Millisecond):
			}
		}
		if err := onDelta(w); err != nil {
			return err
		}
	}
	return nil
}
