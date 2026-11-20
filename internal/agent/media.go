package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tichopad/bobik/internal/fireworks"
)

const (
	mediaFailure    = "Image inspection failed or returned no usable analysis. Check image validity and model support; set --vision-model to a Fireworks model that supports images. No fallback model was used."
	mediaCancelled  = "Image inspection cancelled because the run was cancelled or timed out."
	mediaIncomplete = "INCOMPLETE IMAGE INSPECTION — model output limit reached; the following is partial:\n\n"
)

type visionSource struct {
	read func(string, int64) ([]byte, error)
}

func (s visionSource) Read(path string, max int64) ([]byte, error) { return s.read(path, max) }

func (s *runState) viewMedia(ctx context.Context, id, path, prompt string) (any, error, error) {
	if strings.TrimSpace(prompt) == "" {
		prompt = fireworks.DefaultMediaPrompt
	}
	event := map[string]any{"toolCallId": id, "path": path, "prompt": prompt, "model": s.cfg.VisionModel}
	source := visionSource{read: func(name string, max int64) ([]byte, error) {
		if max > fireworks.MaxImageBytes || max <= 0 {
			max = fireworks.MaxImageBytes
		}
		data, err := s.cfg.Notes.Read(notePath(name), max)
		if err == nil {
			event["byteCount"] = len(data)
			event["sha256"] = fmt.Sprintf("%x", sha256.Sum256(data))
			switch mime := http.DetectContentType(data); mime {
			case "image/png", "image/jpeg", "image/webp", "image/gif":
				event["mediaType"] = mime
			}
		}
		return data, err
	}}

	var text string
	var err error
	if s.cfg.VisionClient == nil {
		err = errors.New("no vision client is configured")
	} else {
		var usage fireworks.Usage
		text, usage, err = s.cfg.VisionClient.ViewMedia(ctx, source, s.cfg.VisionModel, path, prompt)
		s.visionUsage = addUsage(s.visionUsage, usage)
		event["usage"] = usage
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				err = errors.New(mediaCancelled)
			} else if _, safe := s.cfg.VisionClient.(*fireworks.Client); !safe {
				// A custom provider may include the image or its credentials
				// in its error. The built-in adapter already sanitizes errors.
				err = errors.New(mediaFailure)
			}
		}
	}
	if err == nil {
		switch {
		case strings.TrimSpace(text) == "":
			err = errors.New(mediaFailure)
		case len(text) > MaxToolOutput/2:
			err = errors.New("image inspection output exceeds the tool result limit")
		default:
			if encoded, _ := json.Marshal(text); len(encoded) > MaxToolOutput {
				err = errors.New("image inspection output exceeds the tool result limit")
			}
		}
	}
	if err != nil {
		event["status"], event["error"] = "error", err.Error()
	} else {
		partial, incomplete := strings.CutPrefix(text, mediaIncomplete)
		event["status"], event["text"], event["output"] = "success", partial, text
		event["finishReason"] = "stop"
		if incomplete {
			event["finishReason"] = "length"
		}
	}
	if logErr := s.log.Event("vision-inspection", event); logErr != nil {
		return nil, nil, logErr
	}
	return text, err, nil
}
