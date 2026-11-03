// Package deepseektoken counts prompt tokens the way DeepSeek-V4 encodes them, before a
// request is sent. It covers only what the official tokenizer.json uses: added tokens, three
// fixed split rules and byte-level BPE. Go's regexp has no lookahead, which one rule needs, so
// the rules are written by hand and checked against the official Rust library's output.
package deepseektoken

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	_ "embed" // tokenizer.json.gz is the official file, compressed and otherwise unchanged.
)

// Name identifies the tokenizer file (by SHA-256 prefix) and this implementation in call audits.
const Name = "deepseek-v4-tokenizer-8f9f37ca-v1"

// Template tokens from the official encoding_dsv4.py: BOS, <｜User｜>, <｜Assistant｜> and the
// thinking token that follows it (</think> in chat mode, <think> in thinking mode).
const templateTokens = 4

//go:embed tokenizer.json.gz
var embedded []byte

// Counter is immutable after loading and safe for concurrent use.
type Counter struct {
	merges     map[[2]int32]merge
	byteSymbol [256]int32
	added      map[string]struct{}
	addedSizes map[byte][]int // Candidate lengths by first byte, longest first.
}

type merge struct{ rank, symbol int32 }

// Load parses the embedded tokenizer once; every caller shares the result.
var Load = sync.OnceValues(func() (*Counter, error) { return parse(embedded) })

type tokenizerFile struct {
	AddedTokens []struct {
		Content string `json:"content"`
	} `json:"added_tokens"`
	Model struct {
		Vocab  map[string]int32 `json:"vocab"`
		Merges []string         `json:"merges"`
	} `json:"model"`
}

func parse(compressed []byte) (*Counter, error) {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("open tokenizer: %w", err)
	}
	var file tokenizerFile
	if err := json.NewDecoder(reader).Decode(&file); err != nil {
		return nil, fmt.Errorf("decode tokenizer: %w", err)
	}
	c := &Counter{merges: make(map[[2]int32]merge, len(file.Model.Merges)), added: map[string]struct{}{}, addedSizes: map[byte][]int{}}
	for rank, line := range file.Model.Merges {
		left, right, ok := strings.Cut(line, " ")
		a, aok := file.Model.Vocab[left]
		b, bok := file.Model.Vocab[right]
		joined, jok := file.Model.Vocab[left+right]
		if !ok || !aok || !bok || !jok {
			return nil, errors.New("tokenizer merge references an unknown symbol")
		}
		c.merges[[2]int32{a, b}] = merge{rank: int32(rank), symbol: joined}
	}
	for value, char := range byteLevelChars() {
		symbol, ok := file.Model.Vocab[string(char)]
		if !ok {
			return nil, errors.New("tokenizer lacks a byte-level symbol")
		}
		c.byteSymbol[value] = symbol
	}
	for _, token := range file.AddedTokens {
		if token.Content == "" {
			return nil, errors.New("tokenizer has an empty added token")
		}
		c.added[token.Content] = struct{}{}
		first := token.Content[0]
		if !slices.Contains(c.addedSizes[first], len(token.Content)) {
			c.addedSizes[first] = append(c.addedSizes[first], len(token.Content))
		}
	}
	for first := range c.addedSizes {
		slices.SortFunc(c.addedSizes[first], func(a, b int) int { return b - a })
	}
	return c, nil
}

// byteLevelChars is the GPT-2 byte-to-character table tokenizer.json's vocabulary is written in.
func byteLevelChars() [256]rune {
	var table [256]rune
	shifted := rune(256)
	for value := range 256 {
		if value >= '!' && value <= '~' || value >= 0xA1 && value <= 0xAC || value >= 0xAE {
			table[value] = rune(value)
			continue
		}
		table[value] = shifted
		shifted++
	}
	return table
}

// Name implements the investigation package's token counter.
func (c *Counter) Name() string { return Name }

// CountPrompt returns the tokens of one system and one user message in the official template.
func (c *Counter) CountPrompt(instructions string, input []byte) int {
	return c.Count(instructions) + c.Count(string(input)) + templateTokens
}

// Count returns the tokens of text with no template around it. Input is walked by runes, so
// invalid UTF-8 cannot panic; it does not occur here because prompts are marshaled JSON.
func (c *Counter) Count(text string) int {
	total := 0
	words := map[string]int{} // Telemetry repeats its words, and BPE is the expensive step.
	start := 0
	for i := 0; i < len(text); i++ {
		for _, size := range c.addedSizes[text[i]] {
			if i+size > len(text) {
				continue
			}
			if _, ok := c.added[text[i:i+size]]; ok {
				total += c.countPlain(text[start:i], words) + 1
				start = i + size
				i = start - 1
				break
			}
		}
	}
	return total + c.countPlain(text[start:], words)
}

func (c *Counter) countPlain(text string, words map[string]int) int {
	total := 0
	splitIsolated([]rune(text), func(number []rune) {
		splitIsolatedCJK(number, func(script []rune) {
			splitWords(script, func(word []rune) {
				key := string(word)
				count, seen := words[key]
				if !seen {
					count = c.countWord(key)
					words[key] = count
				}
				total += count
			})
		})
	})
	return total
}

// countWord applies BPE: repeatedly merge the lowest-ranked adjacent pair, leftmost on a tie.
func (c *Counter) countWord(word string) int {
	symbols := make([]int32, len(word))
	for i := range len(word) {
		symbols[i] = c.byteSymbol[word[i]]
	}
	for len(symbols) > 1 {
		best, at := merge{rank: -1}, -1
		for i := range len(symbols) - 1 {
			if found, ok := c.merges[[2]int32{symbols[i], symbols[i+1]}]; ok && (best.rank < 0 || found.rank < best.rank) {
				best, at = found, i
			}
		}
		if at < 0 {
			break
		}
		symbols[at] = best.symbol
		symbols = slices.Delete(symbols, at+1, at+2)
	}
	return len(symbols)
}

// splitIsolated is rule one, \p{N}{1,3}: digits in groups of up to three, the rest between them.
func splitIsolated(text []rune, emit func([]rune)) {
	start := 0
	for i := 0; i < len(text); {
		if !unicode.IsNumber(text[i]) {
			i++
			continue
		}
		if i > start {
			emit(text[start:i])
		}
		end := i + 1
		for end < len(text) && end < i+3 && unicode.IsNumber(text[end]) {
			end++
		}
		emit(text[i:end])
		i, start = end, end
	}
	if start < len(text) {
		emit(text[start:])
	}
}

func isCJK(r rune) bool {
	return r >= 0x4E00 && r <= 0x9FA5 || r >= 0x3040 && r <= 0x309F || r >= 0x30A0 && r <= 0x30FF
}

// splitIsolatedCJK is rule two: runs of ideographs and kana stand alone.
func splitIsolatedCJK(text []rune, emit func([]rune)) {
	for i := 0; i < len(text); {
		end := i + 1
		for end < len(text) && isCJK(text[end]) == isCJK(text[i]) {
			end++
		}
		emit(text[i:end])
		i = end
	}
}

// isSpace matches Oniguruma's Unicode \s, which differs from Go's unicode.IsSpace.
func isSpace(r rune) bool {
	return r >= 0x09 && r <= 0x0D || r == 0x85 || unicode.Is(unicode.Z, r)
}

func isNewline(r rune) bool { return r == '\r' || r == '\n' }

func isSymbol(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsMark(r) }

func isASCIIPunct(r rune) bool {
	return r < utf8.RuneSelf && strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", r)
}

func isASCIILetter(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }

// splitWords is rule three. Text matched by no alternative still forms a piece of its own.
func splitWords(text []rune, emit func([]rune)) {
	pending := 0
	for i := 0; i < len(text); {
		end := matchWord(text, i)
		if end <= i {
			i++
			continue
		}
		if i > pending {
			emit(text[pending:i])
		}
		emit(text[i:end])
		i, pending = end, end
	}
	if pending < len(text) {
		emit(text[pending:])
	}
}

// matchWord returns where the rule's first matching alternative ends at i, or i if none does:
//
//	[ASCII punctuation][A-Za-z]+ | [^\r\n\p{L}\p{P}\p{S}]?[\p{L}\p{M}]+ | ' '?[\p{P}\p{S}]+[\r\n]* |
//	\s*[\r\n]+ | \s+(?!\S) | \s+
func matchWord(text []rune, i int) int {
	run := func(from int, in func(rune) bool) int {
		for from < len(text) && in(text[from]) {
			from++
		}
		return from
	}
	r := text[i]
	if isASCIIPunct(r) {
		if end := run(i+1, isASCIILetter); end > i+1 {
			return end
		}
	}
	if !isNewline(r) && !unicode.IsLetter(r) && !isSymbol(r) {
		if end := run(i+1, isWordRune); end > i+1 {
			return end
		}
	}
	if isWordRune(r) {
		return run(i, isWordRune)
	}
	from := i
	if r == ' ' {
		from = i + 1
	}
	if end := run(from, isSymbol); end > from {
		return run(end, isNewline)
	}
	if !isSpace(r) {
		return i
	}
	spaces := run(i, isSpace)
	for end := spaces; end > i; end-- {
		if isNewline(text[end-1]) {
			return end
		}
	}
	// Followed by a non-space, \s+(?!\S) gives one space back; if that leaves nothing, \s+ takes it.
	if spaces < len(text) && spaces-1 > i {
		return spaces - 1
	}
	return spaces
}
