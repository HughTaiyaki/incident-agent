package telemetry

import (
	"strings"
	"testing"
)

func TestSanitizeMessageMasksIdentifiersAndPersonalData(t *testing.T) {
	for input, want := range map[string]string{
		"order O2026110212560538669776: reserve outcome unknown, the worker will retry: unavailable":                    "order [id]: reserve outcome unknown, the worker will retry: unavailable",
		"consumer goods.cart-order-placed: event 1fab9758-aa11-4c2d-9e10-0123456789ab already processed; acknowledging": "consumer goods.cart-order-placed: event [id] already processed; acknowledging",
		"database error: Error 1062 (23000)":                    "database error: Error 1062 (23000)",
		"Duplicate entry 'alice-13800001111' for key 'uk_user'": "Duplicate entry [value] for key [value]",
		"notify alice@example.com failed":                       "notify [email] failed",
		"dial tcp 10.1.2.3:3306: connect refused":               "dial tcp [ip]: connect refused",
		"call https://pay.example/cb?u=alice failed":            "call [url] failed",
		"用户 张三 手机 下单失败 code=5":                                  "[text] [text] [text] [text] code=5",
		"user 13800001111 rejected":                             "user [id] rejected",
		"trace 7106f3fd8e3e8100b5fdba15d22829d0 rolled back":    "trace [id] rolled back",
		"first line\nstack frame with order O20261102125605":    "first line",
		"request id reused with different parameters":           "request id reused with different parameters",
	} {
		got, ok := SanitizeMessage(input)
		if !ok || got != want {
			t.Errorf("%q: got %q (%v), want %q", input, got, ok, want)
		}
	}
}

func TestSanitizeMessageDropsUnsafeText(t *testing.T) {
	for _, input := range []string{"", "  \n", "password=hunter2 rejected", "Authorization: Bearer abc", "api_key invalid",
		"failpoint goods.reserve.after-commit: error", "eventbus.inbox.after-commit hit", "bad\x01control", "bad\xffutf8"} {
		if got, ok := SanitizeMessage(input); ok {
			t.Errorf("%q kept as %q", input, got)
		}
	}
}

func TestSanitizeMessageKeepsTheErrorAfterALongStatement(t *testing.T) {
	statement := "reserve failed: UPDATE inventory SET " + strings.Repeat("available=available + 1,", 30) + " WHERE id=5: Error 1054 (42S22): Unknown column"
	got, ok := SanitizeMessage(statement)
	if !ok || !strings.HasPrefix(got, "reserve failed") || !strings.HasSuffix(got, "Error 1054 (42S22): Unknown column") || len([]rune(got)) > maxMessageRunes {
		t.Fatalf("%d runes: %q", len([]rune(got)), got)
	}
}

func TestSanitizeMessageIsBoundedAndStable(t *testing.T) {
	got, ok := SanitizeMessage(strings.Repeat("word ", 100))
	if !ok || len([]rune(got)) > maxMessageRunes {
		t.Fatalf("%d runes", len([]rune(got)))
	}
	once, _ := SanitizeMessage("order O2026110212560538669776 for 10.1.2.3 from 'x' failed")
	twice, ok := SanitizeMessage(once)
	if !ok || once != twice {
		t.Fatalf("%q then %q", once, twice)
	}
}
