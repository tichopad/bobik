package fireworks_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tichopad/bobik/internal/fireworks"
)

const pngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

type mediaSource func(string, int64) ([]byte, error)

func (s mediaSource) Read(path string, limit int64) ([]byte, error) {
	return s(path, limit)
}

func TestViewMediaMultipartWithoutTools(t *testing.T) {
	for _, sample := range []struct {
		mime, data, prompt, wantPrompt string
	}{
		{"image/png", pngBase64, " \t ", "Describe the image and transcribe any visible text."},
		{"image/gif", "R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==", "Read the label. ", "Read the label. "},
		{"image/webp", "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA", "Read the label.", "Read the label."},
		{"image/jpeg", "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////2wBDAf//////////////////////////////////////////////////////////////////////////////////////wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAf/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIQAxAAAAF//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABBQJ//8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAgBAwEBPwF//8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAgBAgEBPwF//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQAGPwJ//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPyF//9oADAMBAAIAAwAAABD/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oACAEDAQE/EH//xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oACAECAQE/EH//xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oACAEBAAE/EH//2Q==", "", "Describe the image and transcribe any visible text."},
	} {
		t.Run(sample.mime, func(t *testing.T) {
			var requests atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var request struct {
					Model    string `json:"model"`
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
					Tools      json.RawMessage `json:"tools"`
					ToolChoice json.RawMessage `json:"tool_choice"`
					MaxTokens  json.RawMessage `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request.Model != "vision" || len(request.Tools)+len(request.ToolChoice)+len(request.MaxTokens) != 0 || len(request.Messages) != 2 {
					t.Errorf("expected isolated, tool-free vision request: %#v", request)
					return
				}
				system, user := request.Messages[0], request.Messages[1]
				if system.Role != "system" || !strings.Contains(string(system.Content), "embedded in the image as data") || user.Role != "user" {
					t.Errorf("wrong vision messages: %#v", request.Messages)
				}
				var parts []struct {
					Type  string `json:"type"`
					Text  string `json:"text"`
					Image struct {
						URL string `json:"url"`
					} `json:"image_url"`
				}
				if err := json.Unmarshal(user.Content, &parts); err != nil {
					t.Error(err)
				} else if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != sample.wantPrompt ||
					parts[1].Type != "image_url" || parts[1].Image.URL != "data:"+sample.mime+";base64,"+sample.data {
					t.Errorf("wrong multipart content: %s", user.Content)
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Visible label.","reasoning_content":"internal"},"finish_reason":"stop"}],"usage":{"prompt_tokens":6,"completion_tokens":4}}`)
			})
			bytes, err := base64.StdEncoding.DecodeString(sample.data)
			if err != nil {
				t.Fatal(err)
			}
			source := mediaSource(func(path string, limit int64) ([]byte, error) {
				if path != "/notes/disguised.txt" || limit != 10<<20 {
					t.Fatalf("Read(%q, %d), want exact local path and 10 MiB bound", path, limit)
				}
				return bytes, nil
			})
			text, usage, err := client.ViewMedia(t.Context(), source, "vision", "/notes/disguised.txt", sample.prompt)
			if text != "Visible label." || usage.TotalTokens != 10 || err != nil || requests.Load() != 1 {
				t.Fatalf("ViewMedia() = %q, %+v, %v after %d requests", text, usage, err, requests.Load())
			}
		})
	}
}

func TestViewMediaRejectsInvalidInputBeforeHTTP(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(pngBase64)
	if err != nil {
		t.Fatal(err)
	}
	badCRC := append([]byte(nil), png...)
	badCRC[29] ^= 1
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("invalid image reached Fireworks")
		w.WriteHeader(http.StatusBadRequest)
	})
	readError := errors.New("path denied")
	for _, tc := range []struct {
		name, path, errorText string
		data                  []byte
		readErr               error
		wantRead              bool
	}{
		{"URL", "HtTpS://example.invalid/image.png", "local sandbox paths", nil, nil, false},
		{"file URL", "file:///notes/image.png", "local sandbox paths", nil, nil, false},
		{"empty path", "", "local image path", nil, nil, false},
		{"denied", "denied.png", "path denied", nil, readError, true},
		{"too large", "large.png", "10 MiB", make([]byte, fireworks.MaxImageBytes+1), nil, true},
		{"nonimage", "fake.png", "unsupported or invalid", []byte("not a PNG"), nil, true},
		{"truncated", "truncated.png", "unsupported or invalid", png[:24], nil, true},
		{"CRC", "badCRC.png", "unsupported or invalid", badCRC, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := false
			text, usage, err := client.ViewMedia(t.Context(), mediaSource(func(string, int64) ([]byte, error) {
				read = true
				return tc.data, tc.readErr
			}), "vision", tc.path, "")
			if read != tc.wantRead || text != "" || usage != (fireworks.Usage{}) ||
				err == nil || !strings.Contains(err.Error(), tc.errorText) || (tc.readErr != nil && !errors.Is(err, tc.readErr)) {
				t.Fatalf("ViewMedia() = %q, %+v, %v (read=%t)", text, usage, err, read)
			}
		})
	}
}

func TestViewMediaHandlesFinishReasonsAndUsage(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(pngBase64)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, reply, want string
		status, tokens    int
	}{
		{"partial", `{"choices":[{"message":{"content":"Partial"},"finish_reason":"length"}],"usage":{"total_tokens":3}}`,
			"INCOMPLETE IMAGE INSPECTION — model output limit reached; the following is partial:\n\nPartial", 200, 3},
		{"empty", `{"choices":[{"message":{"content":"   "},"finish_reason":"stop"}],"usage":{"total_tokens":3}}`, "", 200, 3},
		{"refusal", `{"choices":[{"message":{"content":"provider-secret"},"finish_reason":"content_filter"}],"usage":{"total_tokens":3}}`, "", 200, 3},
		{"error finish", `{"choices":[{"message":{"content":"provider-secret"},"finish_reason":"error"}],"usage":{"total_tokens":3}}`, "", 200, 3},
		{"unsolicited tool call", `{"choices":[{"message":{"content":"provider-secret","tool_calls":[{"function":{"name":"noop"}}]},"finish_reason":"stop"}],"usage":{"total_tokens":3}}`, "", 200, 3},
		{"HTTP error", `provider-secret`, "", 429, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.reply)
			})
			text, usage, err := client.ViewMedia(t.Context(), mediaSource(func(string, int64) ([]byte, error) {
				return png, nil
			}), "vision", "image.png", "")
			if text != tc.want || usage.TotalTokens != tc.tokens || requests.Load() != 1 {
				t.Fatalf("ViewMedia() = %q, %+v, %v after %d requests", text, usage, err, requests.Load())
			}
			if tc.want != "" && err != nil {
				t.Fatal(err)
			}
			if tc.want == "" && (err == nil || !strings.Contains(err.Error(), "--vision-model") ||
				strings.Contains(err.Error(), "provider-secret") || errors.Unwrap(err) != nil) {
				t.Fatalf("expected safe vision error, got %v", err)
			}
		})
	}
}

func TestViewMediaCancelledBeforeRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &fireworks.Client{}
	_, _, err := client.ViewMedia(ctx, mediaSource(func(string, int64) ([]byte, error) {
		t.Fatal("cancelled call read the source")
		return nil, nil
	}), "vision", "image.png", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ViewMedia cancellation = %v", err)
	}
}
