package control

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/region"
)

// A miscounted INSERT is invisible until production logs it: Postgres rejects
// the statement at run time and every edge-reported batch is dropped. This test
// reads the statement itself and checks the three things that must agree —
// target columns, SELECT expressions, and the bind parameters the caller
// passes — so the next edit to it fails here instead of in the logs.

var insertPlaceholderPattern = regexp.MustCompile(`\$(\d+)`)

func TestUsageInsertArity(t *testing.T) {
	sql := usageInsertSQL
	columns := usageInsertColumnList(t, sql)
	expressions := usageInsertSelectExpressions(t, sql)

	if len(columns) != len(expressions) {
		t.Fatalf("INSERT lists %d columns but selects %d expressions:\ncolumns: %v\nexpressions: %v",
			len(columns), len(expressions), columns, expressions)
	}
	// The statement carries the request_id guard twice (WHERE NOT EXISTS plus
	// the dedupe SELECT), so the same columns are meaningfully present.
	for _, required := range []string{"id", "request_id", "provider_account_id", "provider_id", "user_id"} {
		if !contains(columns, required) {
			t.Fatalf("usage_logs insert lost the %q column: %v", required, columns)
		}
	}

	// Bind parameters must be a dense range starting at 1: the caller passes a
	// slice positionally, so a gap or an extra number silently shifts values.
	maximum := 0
	seen := map[int]bool{}
	for _, match := range insertPlaceholderPattern.FindAllStringSubmatch(sql, -1) {
		index, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("bad placeholder %q", match[0])
		}
		seen[index] = true
		if index > maximum {
			maximum = index
		}
	}
	for i := 1; i <= maximum; i++ {
		if !seen[i] {
			t.Fatalf("placeholder $%d is unused; parameters would shift", i)
		}
	}
	args := usageInsertArgs(region.UsageEvent{}, time.Now(), decimal.Zero, "balance", "ok", uuid.Nil)
	if len(args) != maximum {
		t.Fatalf("statement binds $1..$%d but the caller passes %d arguments", maximum, len(args))
	}
}

// usageInsertColumnList extracts the parenthesised column list after
// `INSERT INTO usage_logs`.
func usageInsertColumnList(t *testing.T, sql string) []string {
	t.Helper()
	start := strings.Index(sql, "INSERT INTO usage_logs")
	if start < 0 {
		t.Fatal("statement does not target usage_logs")
	}
	open := strings.Index(sql[start:], "(")
	if open < 0 {
		t.Fatal("statement has no column list")
	}
	rest := sql[start+open+1:]
	close := strings.Index(rest, ")")
	if close < 0 {
		t.Fatal("column list is unterminated")
	}
	return splitTopLevel(rest[:close])
}

// usageInsertSelectExpressions extracts the SELECT list of the statement,
// splitting on top-level commas so a nested SELECT stays one expression.
func usageInsertSelectExpressions(t *testing.T, sql string) []string {
	t.Helper()
	start := strings.Index(sql, "\n\t\tSELECT ")
	if start < 0 {
		start = strings.Index(sql, "SELECT ")
	}
	if start < 0 {
		t.Fatal("statement has no SELECT list")
	}
	rest := sql[start:]
	end := strings.Index(rest, "WHERE NOT EXISTS")
	if end < 0 {
		t.Fatal("SELECT list is unterminated")
	}
	body := strings.TrimSpace(rest[len("SELECT"):end])
	return splitTopLevel(body)
}

// splitTopLevel splits on commas that are not inside parentheses, brackets or
// string literals.
func splitTopLevel(body string) []string {
	var (
		out     []string
		depth   int
		inQuote bool
		current strings.Builder
	)
	for i := 0; i < len(body); i++ {
		ch := body[i]
		switch {
		case ch == '\'':
			inQuote = !inQuote
			current.WriteByte(ch)
		case inQuote:
			current.WriteByte(ch)
		case ch == '(' || ch == '[':
			depth++
			current.WriteByte(ch)
		case ch == ')' || ch == ']':
			depth--
			current.WriteByte(ch)
		case ch == ',' && depth == 0:
			out = append(out, strings.TrimSpace(current.String()))
			current.Reset()
		default:
			current.WriteByte(ch)
		}
	}
	if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
		out = append(out, trimmed)
	}
	// A trailing whitespace-only fragment after the last comma is not an
	// expression, and neither is an empty entry.
	cleaned := make([]string, 0, len(out))
	for _, item := range out {
		if item != "" {
			cleaned = append(cleaned, item)
		}
	}
	return cleaned
}

func contains(list []string, needle string) bool {
	for _, item := range list {
		if item == needle {
			return true
		}
	}
	return false
}
