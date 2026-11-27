package notes

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

const DefaultTrigger = "#bobik"

// State describes whether a note requests a run or contains a protected draft.
// A draft takes precedence over a ready request.
type State struct {
	Ready bool
	Draft bool
}

func Analyze(content, trigger string) State {
	if trigger == "" {
		trigger = DefaultTrigger
	}
	if !strings.Contains(content, trigger) {
		return State{}
	}
	prose := withoutCode(content)
	var state State
	commented := false
	for part := range strings.SplitSeq(prose, "%%") {
		matches := strings.Contains(part, trigger)
		if trigger == DefaultTrigger {
			matches = hasDefaultTag(part)
		}
		if matches {
			if commented {
				return State{Draft: true}
			}
			state.Ready = true
		}
		commented = !commented
	}
	return state
}

func hasDefaultTag(part string) bool {
	for start := 0; start < len(part); {
		idx := strings.Index(part[start:], DefaultTrigger)
		if idx < 0 {
			return false
		}
		idx += start
		start = idx + len(DefaultTrigger)
		after := part[start:]
		left, _ := utf8.DecodeLastRuneInString(part[:idx])
		right, _ := utf8.DecodeRuneInString(after)
		// Keep JavaScript's whitespace rules, which exclude U+0085.
		rightBoundary := after == "" || unicode.IsPunct(right) || unicode.IsSymbol(right) ||
			unicode.Is(unicode.Zs, right) || strings.ContainsRune("\t\n\v\f\r\u2028\u2029\ufeff", right)
		if !tagCharacter(left) && rightBoundary && !tagCharacter(right) {
			return true
		}
	}
	return false
}

func tagCharacter(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || strings.ContainsRune("_/#-", r)
}

type sourceRange struct{ start, stop int }

const codeRange = "bobik-code-range"

// Goldmark's AST contains code contents, but loses the delimiters and some
// whitespace. Wrap its parsers to capture those positions without reimplementing
// the rules for escaping, fence matching, and nested containers.
type codeSpanParser struct{ parser.InlineParser }

func (p codeSpanParser) Parse(parent ast.Node, reader text.Reader, pc parser.Context) ast.Node {
	_, start := reader.Position()
	node := p.InlineParser.Parse(parent, reader, pc)
	if node != nil && node.Kind() == ast.KindCodeSpan {
		_, end := reader.Position()
		node.SetAttributeString(codeRange, sourceRange{start.Start, end.Start})
	}
	return node
}

type fenceParser struct{ parser.BlockParser }

func (p fenceParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, segment := reader.PeekLine()
	offset := pc.BlockOffset()
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node != nil {
		start := segment.Start - segment.Padding + offset
		stop := segment.Start - segment.Padding + len(strings.TrimRight(string(line), "\r\n"))
		node.SetAttributeString(codeRange, sourceRange{start, stop})
	}
	return node, state
}

func (p fenceParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	line, segment := reader.PeekLine()
	state := p.BlockParser.Continue(node, reader, pc)
	if state == parser.Close {
		bounds, _ := node.AttributeString(codeRange)
		r := bounds.(sourceRange)
		r.stop = segment.Start - segment.Padding + len(strings.TrimRight(string(line), "\r\n"))
		node.SetAttributeString(codeRange, r)
	}
	return state
}

var markdown = func() parser.Parser {
	blocks := parser.DefaultBlockParsers()
	for i, block := range blocks {
		if block.Value == parser.NewFencedCodeBlockParser() {
			blocks[i].Value = fenceParser{parser.NewFencedCodeBlockParser()}
		}
	}
	inlines := parser.DefaultInlineParsers()
	for i, inline := range inlines {
		if inline.Value == parser.NewCodeSpanParser() {
			inlines[i].Value = codeSpanParser{parser.NewCodeSpanParser()}
		}
	}
	return parser.NewParser(
		parser.WithBlockParsers(blocks...),
		parser.WithInlineParsers(inlines...),
		parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
	)
}()

var normalizeLineEndings = strings.NewReplacer("\r\n", "\r\n", "\r", "\n")

func withoutCode(content string) string {
	// Goldmark recognizes LF and CRLF but not standalone CR line endings.
	// Replacing each standalone CR with an LF preserves byte offsets.
	source := normalizeLineEndings.Replace(content)
	tree := markdown.Parse(text.NewReader([]byte(source)))
	var codes []sourceRange
	_ = ast.Walk(tree, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var bounds sourceRange
		switch node.Kind() {
		case ast.KindCodeSpan:
			r, _ := node.AttributeString(codeRange)
			bounds = r.(sourceRange)
		case ast.KindFencedCodeBlock, ast.KindCodeBlock:
			lines := node.Lines()
			if r, ok := node.AttributeString(codeRange); ok {
				bounds = r.(sourceRange)
			} else if lines.Len() > 0 {
				bounds.start = lines.At(0).Start
			}
			if lines.Len() > 0 {
				last := lines.At(lines.Len() - 1)
				stop := last.Start + len(strings.TrimRight(content[last.Start:last.Stop], "\r\n"))
				bounds.stop = max(bounds.stop, stop)
			}
		default:
			return ast.WalkContinue, nil
		}
		if bounds.stop > bounds.start {
			codes = append(codes, bounds)
		}
		return ast.WalkSkipChildren, nil
	})

	sort.Slice(codes, func(i, j int) bool { return codes[i].start < codes[j].start })
	var prose strings.Builder
	end := 0
	for _, code := range codes {
		if code.start >= end {
			prose.WriteString(content[end:code.start])
			prose.WriteByte(' ')
		}
		end = max(end, code.stop)
	}
	prose.WriteString(content[end:])
	return prose.String()
}
