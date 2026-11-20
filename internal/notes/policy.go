package notes

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

var (
	ErrNotAllowed = errors.New("note path or operation is not allowed")
	ErrTooLarge   = errors.New("note exceeds the size limit")
)

const auditDir = ".agent-bobik"
const tempPrefix = ".bobik-write-"

type policy struct {
	cwd      string
	base     string // relative to cwd
	segments []globSegment
}

type globSegment struct {
	recursive bool
	patterns  []string
}

func compilePolicy(pattern, cwd string) (policy, error) {
	var p policy
	var err error
	p.cwd, err = filepath.Abs(cwd)
	if err != nil {
		return p, err
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || strings.ContainsRune(pattern, 0) {
		return p, fmt.Errorf("--allow-path must be nonempty and contain no NUL")
	}
	if strings.HasPrefix(pattern, "!") {
		return p, fmt.Errorf("--allow-path does not support negated globs")
	}
	relative, err := relativePath(p.cwd, pattern)
	if err != nil {
		return p, fmt.Errorf("--allow-path must stay inside the working directory (%s): %w", p.cwd, err)
	}

	p.base = "."
	if relative == "." {
		return p, nil // cwd itself cannot be an eligible file
	}
	parts := strings.Split(relative, "/")
	baseLiteral := true
	for i, part := range parts {
		alternatives, err := globAlternatives(part)
		if err != nil {
			return p, fmt.Errorf("invalid --allow-path segment %q: %w", part, err)
		}
		p.segments = append(p.segments, globSegment{part == "**", alternatives})
		// A fully literal pattern points to a file; watch its parent, so
		// replacement or creation of a previously missing file is visible.
		if !strings.ContainsAny(part, "*?[]{}()!+@") && baseLiteral && i < len(parts)-1 {
			literal := unescape(part)
			if literal == "." || literal == ".." {
				return p, errors.New("escaped dot and parent-directory segments are unsupported")
			}
			p.base = filepath.Join(p.base, literal)
		} else {
			baseLiteral = false
		}
	}
	return p, nil
}

func relativePath(cwd, path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%w: invalid path %q", ErrNotAllowed, path)
	}
	if !filepath.IsAbs(path) {
		// A clean relative path must not have crossed the root, even if it
		// could get back inside it by spelling out some of cwd again.
		depth := 0
		for part := range strings.SplitSeq(path, "/") {
			switch part {
			case "", ".":
			case "..":
				depth--
			default:
				depth++
			}
			if depth < 0 {
				return "", fmt.Errorf("%w: %q is outside the working directory", ErrNotAllowed, path)
			}
		}
		path = filepath.Join(cwd, path)
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%w: %q is outside the working directory", ErrNotAllowed, path)
	}
	return rel, nil
}

func unescape(s string) string {
	var result strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		result.WriteByte(s[i])
	}
	return result.String()
}

// Brace alternatives stay within one segment, so each alternative can enforce
// its own leading-dot rule. Extglobs, brace ranges and cross-segment braces
// are deliberately unsupported; do not silently interpret them as literals.
func globAlternatives(segment string) ([]string, error) {
	var results []string
	var expand func(string) error
	expand = func(part string) error {
		const maxAlternatives = 256
		if len(results) == maxAlternatives {
			return errors.New("too many brace alternatives")
		}
		inClass := false
		depth, open, split := 0, 0, 0
		var options []string
		for i := 0; i < len(part); i++ {
			switch c := part[i]; {
			case c == '\\':
				i++
			case c == ']' && inClass:
				inClass = false
			case inClass:
			case c == '[':
				inClass = true
			case c == '(' || c == ')':
				return errors.New("extglobs and unescaped parentheses are unsupported")
			case c == '{':
				if depth == 0 {
					open, split = i, i+1
				}
				depth++
			case c == ',' && depth == 1:
				options = append(options, part[split:i])
				split = i + 1
			case c == '}':
				if depth == 0 {
					return errors.New("unexpected closing brace")
				}
				depth--
				if depth == 0 {
					if len(options) == 0 {
						return errors.New("brace expressions must contain comma-separated alternatives")
					}
					for _, opt := range append(options, part[split:i]) {
						if err := expand(part[:open] + opt + part[i+1:]); err != nil {
							return err
						}
					}
					return nil
				}
			}
		}
		if depth != 0 || !doublestar.ValidatePattern(part) {
			return errors.New("malformed glob")
		}
		results = append(results, part)
		return nil
	}
	err := expand(segment)
	return results, err
}

func (g globSegment) matches(name string) bool {
	for _, part := range g.patterns {
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(part, ".") && !strings.HasPrefix(part, `\.`) {
			continue
		}
		if doublestar.MatchUnvalidated(part, name) {
			return true
		}
	}
	return false
}

// candidates reports whether the path matches and whether any of its
// descendants could match. Globstars transition without consuming a segment
// or consume one non-hidden segment and stay in the same state.
func (p policy) candidates(path string) (matches, descends bool) {
	states := make([]bool, len(p.segments)+1)
	states[0] = true
	epsilon := func() {
		for i, segment := range p.segments {
			if states[i] && segment.recursive {
				states[i+1] = true
			}
		}
	}
	epsilon()
	if path != "." {
		next := make([]bool, len(states))
		for name := range strings.SplitSeq(path, "/") {
			clear(next)
			for i, segment := range p.segments {
				if !states[i] {
					continue
				}
				if segment.recursive {
					if !strings.HasPrefix(name, ".") {
						next[i] = true
					}
				} else if segment.matches(name) {
					next[i+1] = true
				}
			}
			states, next = next, states
			epsilon()
		}
	}
	for _, possible := range states[:len(p.segments)] {
		descends = descends || possible
	}
	return path != "." && states[len(p.segments)], descends
}

func beneath(path, ancestor string) bool {
	return ancestor == "." || path == ancestor || strings.HasPrefix(path, ancestor+"/")
}

func reserved(path string, excluded []string) bool {
	if beneath(path, auditDir) {
		return true
	}
	for part := range strings.SplitSeq(path, "/") {
		if strings.HasPrefix(part, tempPrefix) {
			return true
		}
	}
	for _, candidate := range excluded {
		if beneath(path, candidate) {
			return true
		}
	}
	return false
}
