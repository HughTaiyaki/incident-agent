package telemetry

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxMessageRunes bounds free text kept from a log body or span status.
const maxMessageRunes = 200

var (
	// Text naming a credential or an experiment control point is dropped whole: masking
	// could not be trusted to leave nothing behind.
	unsafeMessage = regexp.MustCompile(`(?i)failpoint|password|passwd|secret|bearer|token|api[_-]?key|private key|authorization|cookie|` +
		`goods\.reserve\.after-commit|order\.create\.after-insert|order\.payment\.before-record|order\.task\.after-claim|eventbus\.(relay|inbox)\.after-`)
	messageMasks = []struct {
		pattern     *regexp.Regexp
		replacement string
	}{
		{regexp.MustCompile("'[^']*'|\"[^\"]*\"|`[^`]*`"), "[value]"},
		{regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://\S+`), "[url]"},
		{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+`), "[email]"},
		{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`), "[ip]"},
		{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F-]{8,}\b`), "[id]"},
		{regexp.MustCompile(`\b[0-9a-fA-F]*\d[0-9a-fA-F]*[a-fA-F][0-9a-fA-F]*\b|\b[0-9a-fA-F]*[a-fA-F][0-9a-fA-F]*\d[0-9a-fA-F]*\b`), "[id]"},
		{regexp.MustCompile(`\b[A-Za-z]{0,4}\d{6,}\b`), "[id]"},
		{regexp.MustCompile(`[^\x00-\x7F]+`), "[text]"},
	}
	maskedShortHex = regexp.MustCompile(`^[0-9a-fA-F]{1,7}$`)
)

// SanitizeMessage keeps the first line of operational text with identifiers and personal
// data masked. It is a reduction, not a guarantee: ok is false when the text should not
// leave at all, and text it keeps still counts as pseudonymized, not anonymous.
func SanitizeMessage(text string) (string, bool) {
	if !utf8.ValidString(text) {
		return "", false
	}
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if line == "" || unsafeMessage.MatchString(line) {
		return "", false
	}
	for _, char := range line {
		if unicode.IsControl(char) && char != '\t' {
			return "", false
		}
	}
	for _, mask := range messageMasks {
		line = mask.pattern.ReplaceAllStringFunc(line, func(match string) string {
			// Short hex-looking words such as "add" or status codes are ordinary text.
			if mask.replacement == "[id]" && maskedShortHex.MatchString(match) {
				return match
			}
			return mask.replacement
		})
	}
	if runes := []rune(line); len(runes) > maxMessageRunes {
		line = string(runes[:maxMessageRunes])
	}
	return line, true
}
