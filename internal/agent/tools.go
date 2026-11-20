package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tichopad/bobik/internal/fireworks"
	"github.com/tichopad/bobik/internal/notes"
	"github.com/tichopad/bobik/internal/sandbox"
)

const (
	MaxNoteRead   = 128 << 10
	MaxToolOutput = 256 << 10  // JSON-encoded response bytes
	maxArguments  = 1024 << 10 // JSON-encoded argument bytes
)

type field struct {
	name     string
	required bool
	nonempty bool
	boolean  bool
}

type tool struct {
	name, description string
	fields            []field
}

var pathField = field{name: "path", required: true, nonempty: true}

var tools = []tool{
	{"readFile", "Read the contents of an allowed file.", []field{pathField}},
	{"writeFile", "Create or fully replace an allowed note with text.", []field{pathField, {name: "content", required: true}}},
	{"editFile", "Replace exact literal text. By default oldString must occur exactly once; set replaceAll to replace every occurrence.", []field{
		pathField, {name: "oldString", required: true, nonempty: true}, {name: "newString", required: true}, {name: "replaceAll", boolean: true},
	}},
	{"deleteFile", "Delete one allowed note. This does not delete directories.", []field{pathField}},
	{"bash", "Run Bash against a filtered, read-only copy of the allowed files. Returns bounded stdout, stderr, and exit status.", []field{{name: "command", required: true}}},
	{"viewMedia", "Inspect an allowed PNG, JPEG, WebP, or GIF image up to 10 MiB. Only local file paths are accepted. Returns plain-text analysis, not image bytes.", []field{pathField, {name: "prompt"}}},
}

func toolDefinitions() []fireworks.Tool {
	defs := make([]fireworks.Tool, 0, len(tools))
	for _, tool := range tools {
		properties := make(map[string]map[string]any, len(tool.fields))
		required := []string{}
		for _, f := range tool.fields {
			property := map[string]any{"type": "string"}
			if f.boolean {
				property["type"] = "boolean"
			}
			if f.nonempty {
				property["minLength"] = 1
			}
			if f.required {
				required = append(required, f.name)
			}
			properties[f.name] = property
		}
		schema, _ := json.Marshal(map[string]any{
			"type": "object", "properties": properties, "required": required, "additionalProperties": false,
		})
		defs = append(defs, fireworks.Tool{Name: tool.name, Description: tool.description, Parameters: schema})
	}
	return defs
}

func parseInput(call fireworks.Call) (map[string]any, error) {
	var spec *tool
	for i := range tools {
		if tools[i].name == call.Name {
			spec = &tools[i]
			break
		}
	}
	if spec == nil {
		return nil, errors.New("unknown tool")
	}
	if len(call.Arguments) > maxArguments {
		return nil, errors.New("tool arguments exceed the 1 MiB limit")
	}
	d := json.NewDecoder(strings.NewReader(call.Arguments))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("tool arguments must be a JSON object")
	}
	input := make(map[string]any, len(spec.fields))
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid tool arguments JSON")
		}
		i := slices.IndexFunc(spec.fields, func(f field) bool { return f.name == name })
		if i < 0 {
			return nil, errors.New("unexpected tool argument")
		}
		if _, exists := input[name]; exists {
			return nil, errors.New("duplicate tool argument")
		}
		value, err := d.Token()
		if err != nil {
			return nil, errors.New("invalid tool argument value")
		}
		f := spec.fields[i]
		if f.boolean {
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("tool argument %s must be a boolean", name)
			}
		} else {
			text, ok := value.(string)
			if !ok || (f.nonempty && text == "") {
				return nil, fmt.Errorf("invalid string tool argument %s", name)
			}
		}
		input[name] = value
	}
	last, err := d.Token()
	if err != nil || last != json.Delim('}') {
		return nil, errors.New("invalid tool arguments JSON")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after tool arguments")
	}
	for _, f := range spec.fields {
		if _, ok := input[f.name]; !ok && f.required {
			return nil, fmt.Errorf("missing required tool argument %s", f.name)
		}
	}
	return input, nil
}

func (s *runState) runTool(ctx context.Context, step int, call fireworks.Call) (fireworks.Message, error) {
	input, toolErr := parseInput(call)
	fields := func() map[string]any {
		return map[string]any{"step": step, "toolName": call.Name, "toolCallId": call.ID}
	}
	event := fields()
	if input != nil {
		event["input"] = input
	}
	if err := s.log.Event("tool-call", event); err != nil {
		return fireworks.Message{}, err
	}
	if s.cfg.Verbose != nil {
		// Do not print note contents, credentials, or provider error details.
		fmt.Fprintf(s.cfg.Verbose, "bobik: step %d tool %q\n", step, call.Name)
	}
	var output any
	if toolErr == nil {
		var fatal error
		output, toolErr, fatal = s.dispatch(ctx, call, input)
		if fatal != nil {
			return fireworks.Message{}, fatal
		}
	}
	result := fields()
	var encoded []byte
	if toolErr == nil {
		var err error
		encoded, err = json.Marshal(output)
		if err != nil {
			return fireworks.Message{}, fmt.Errorf("encode tool result: %w", err)
		}
		if len(encoded) > MaxToolOutput {
			toolErr = errors.New("tool result exceeds the 256 KiB limit")
		}
	}
	if toolErr != nil {
		message := toolErr.Error()
		if len(message) > 4096 {
			message = message[:4096] + " (truncated)"
		}
		result["error"] = message
		encoded, _ = json.Marshal(map[string]string{"error": message})
	} else {
		result["output"] = json.RawMessage(encoded)
		if call.Name == "writeFile" {
			result["input"] = input
		}
	}
	if err := s.log.Event("tool-result", result); err != nil {
		return fireworks.Message{}, err
	}
	return fireworks.Message{Role: "tool", ToolCallID: call.ID, Content: string(encoded)}, nil
}

func (s *runState) dispatch(ctx context.Context, call fireworks.Call, input map[string]any) (output any, toolErr, fatal error) {
	text := func(key string) string {
		v, _ := input[key].(string)
		return v
	}
	path := notePath(text("path"))
	switch call.Name {
	case "readFile":
		data, err := s.cfg.Notes.Read(path, MaxNoteRead)
		if err != nil {
			return nil, err, nil
		}
		return map[string]string{"content": string(data)}, nil, nil
	case "writeFile":
		if err := s.cfg.Notes.Write(path, []byte(text("content"))); err != nil {
			return nil, err, nil
		}
		s.recordWrite(path)
	case "editFile":
		all, _ := input["replaceAll"].(bool)
		count, err := s.cfg.Notes.EditWithOptions(path, text("oldString"), text("newString"), all)
		if err != nil {
			return nil, err, nil
		}
		s.recordWrite(path)
		return map[string]any{"success": true, "replacements": count}, nil, nil
	case "deleteFile":
		if err := s.cfg.Notes.Delete(path); err != nil {
			return nil, err, nil
		}
		s.recordWrite(path)
	case "bash":
		result, err := sandbox.Run(ctx, shellSource{s.cfg.Notes}, text("command"))
		if errors.Is(err, sandbox.ErrUnavailable) {
			return nil, errors.New("Bash sandbox unavailable; commands are never run without isolation. Ask the operator to check Bubblewrap, its runtime dependencies, and the host namespace policy"), nil
		}
		if err != nil {
			return nil, err, nil
		}
		return shellOutput(result), nil, nil
	case "viewMedia":
		return s.viewMedia(ctx, call.ID, text("path"), text("prompt"))
	default:
		return nil, errors.New("unknown tool"), nil
	}
	return map[string]bool{"success": true}, nil, nil
}

func shellOutput(result sandbox.Result) map[string]any {
	const streamsBudget = MaxToolOutput - 512 // reserve space for keys and exit status
	stdout, stderr := strings.ToValidUTF8(result.Stdout, "\uFFFD"), strings.ToValidUTF8(result.Stderr, "\uFFFD")
	outSize, errSize := jsonStringSize(stdout), jsonStringSize(stderr)
	if outSize+errSize > streamsBudget {
		outBudget := min(outSize, streamsBudget/2)
		errBudget := min(errSize, streamsBudget-outBudget)
		outBudget = streamsBudget - errBudget
		if outSize > outBudget {
			stdout, result.StdoutTruncated = jsonStringPrefix(stdout, outBudget), true
		}
		if errSize > errBudget {
			stderr, result.StderrTruncated = jsonStringPrefix(stderr, errBudget), true
		}
	}
	return map[string]any{
		"stdout": stdout, "stderr": stderr, "exitCode": result.ExitCode,
		"stdoutTruncated": result.StdoutTruncated, "stderrTruncated": result.StderrTruncated,
	}
}

func jsonStringSize(text string) int {
	encoded, _ := json.Marshal(text)
	return len(encoded)
}

func jsonStringPrefix(text string, budget int) string {
	boundary := func(n int) int {
		for n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		return n
	}
	n := sort.Search(len(text), func(n int) bool {
		return jsonStringSize(text[:boundary(n+1)]) > budget
	})
	return text[:boundary(n)]
}

// Typed tools keep the legacy virtual-root path convention; the notes store
// independently validates the remaining CWD-relative path.
func notePath(path string) string {
	if filepath.IsAbs(path) {
		return strings.TrimLeft(path, "/")
	}
	return path
}

type shellSource struct{ *notes.Store }

// Walk supplies paths relative to Base. Read those same names from Base, not
// from the working directory used by the typed tools.
func (s shellSource) Read(name string, limit int64) ([]byte, error) {
	if !fs.ValidPath(name) || name == "." {
		return nil, notes.ErrNotAllowed
	}
	return s.Store.ReadRaw(filepath.Join(s.Base(), name), limit)
}
