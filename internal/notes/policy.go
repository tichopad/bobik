package notes

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tichopad/bobik/internal/audit"
)

var (
	ErrNotAllowed = errors.New("note path or operation is not allowed")
	ErrTooLarge   = errors.New("note exceeds the size limit")
)

const tempPrefix = ".bobik-write-"

// Mode controls access to a directory in the vault.
type Mode string

const (
	Hide     Mode = "hide"
	ReadOnly Mode = "read-only"
	Pin      Mode = "pin"
)

// Rule applies Mode to a directory relative to the vault root.
type Rule struct {
	Path string
	Mode Mode
}

type policy struct {
	root     string
	dirs     map[string]Mode
	children map[string][]string
}

func compileVault(root string, rules []Rule) (policy, error) {
	if root == "" || strings.ContainsRune(root, 0) {
		return policy{}, fmt.Errorf("%w: invalid vault root %q", ErrNotAllowed, root)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return policy{}, err
	}
	if abs == "/" {
		return policy{}, fmt.Errorf("%w: filesystem root cannot be a vault", ErrNotAllowed)
	}
	p := policy{root: abs, dirs: make(map[string]Mode, len(rules)+1)}
	var dirs []string
	for _, rule := range rules {
		path, err := rulePath(rule.Path)
		if err != nil {
			return policy{}, err
		}
		switch rule.Mode {
		case Hide, ReadOnly, Pin:
		default:
			return policy{}, fmt.Errorf("%w: invalid mode %q for vault rule %q", ErrNotAllowed, rule.Mode, path)
		}
		if path == audit.Directory && rule.Mode != Hide {
			return policy{}, fmt.Errorf("%w: %q is reserved for audit logs", ErrNotAllowed, path)
		}
		prior, exists := p.dirs[path]
		if exists && prior != rule.Mode {
			return policy{}, fmt.Errorf("%w: conflicting vault rules for %q: %s and %s", ErrNotAllowed, path, prior, rule.Mode)
		}
		if !exists {
			dirs = append(dirs, path)
			p.dirs[path] = rule.Mode
		}
	}
	p.dirs[audit.Directory] = Hide
	for _, path := range dirs {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			switch p.dirs[parent] {
			case Hide:
				return policy{}, fmt.Errorf("%w: vault rule %q is inside hidden directory %q", ErrNotAllowed, path, parent)
			case "":
				p.dirs[parent] = Pin
			}
		}
	}
	p.children = make(map[string][]string)
	for _, rule := range p.rules() {
		parent := filepath.Dir(rule.Path)
		p.children[parent] = append(p.children[parent], rule.Path)
	}
	return p, nil
}

func rulePath(path string) (string, error) {
	invalid := func() (string, error) {
		return "", fmt.Errorf("%w: vault rule must name a relative directory: %q", ErrNotAllowed, path)
	}
	clean := filepath.Clean(path)
	if filepath.IsAbs(path) || strings.ContainsRune(path, 0) || clean == "." {
		return invalid()
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == ".." || strings.HasPrefix(part, tempPrefix) {
			return invalid()
		}
	}
	return clean, nil
}

func (p policy) rules() []Rule {
	out := make([]Rule, 0, len(p.dirs))
	for path, mode := range p.dirs {
		out = append(out, Rule{Path: path, Mode: mode})
	}
	slices.SortFunc(out, func(a, b Rule) int { return strings.Compare(a.Path, b.Path) })
	return out
}

func (p policy) hidden(path string) bool {
	for path != "." {
		if p.dirs[path] == Hide || strings.HasPrefix(filepath.Base(path), tempPrefix) {
			return true
		}
		path = filepath.Dir(path)
	}
	return false
}

func (p policy) writable(path string) bool {
	if p.dirs[path] == Pin {
		return false
	}
	for path != "." {
		if p.dirs[path] == ReadOnly {
			return false
		}
		path = filepath.Dir(path)
	}
	return true
}

func relativePath(root, path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%w: invalid path %q", ErrNotAllowed, path)
	}
	if !filepath.IsAbs(path) {
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
				return "", fmt.Errorf("%w: %q is outside the vault", ErrNotAllowed, path)
			}
		}
		path = filepath.Join(root, path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%w: %q is outside the vault", ErrNotAllowed, path)
	}
	return rel, nil
}
