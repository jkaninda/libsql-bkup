package libsql

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jkaninda/libsql-bkup/internal/hrana"
)

// quoteIdent quotes an SQL identifier: "name" with embedded quotes doubled.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteText quotes a text value: 'it”s'. Text holding NUL bytes (which an
// SQL text literal cannot carry) is written as CAST(X'…' AS TEXT).
func quoteText(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		return "CAST(X'" + strings.ToUpper(hex.EncodeToString([]byte(s))) + "' AS TEXT)"
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// formatFloat renders a REAL so that SQLite reads back the same double and
// keeps it a REAL: shortest round-trip form, ".0" added to integral values,
// 9e999 / -9e999 for infinities (as the sqlite3 shell does) and NULL for NaN
// (SQLite cannot store NaN; it stores NULL).
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NULL"
	case math.IsInf(f, 1):
		return "9e999"
	case math.IsInf(f, -1):
		return "-9e999"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// literal renders a Hrana value as an SQLite literal. A float the server
// could not encode (±Inf arrives as null) is reported with ok=false; the
// caller asks the server for its sign.
func literal(v hrana.Value) (string, bool, error) {
	switch v.Type {
	case hrana.TypeNull:
		return "NULL", true, nil
	case hrana.TypeInteger:
		s := v.Str()
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			return "", false, fmt.Errorf("invalid integer %q from server", s)
		}
		return s, true, nil
	case hrana.TypeFloat:
		if v.IsNullFloat() {
			return "", false, nil
		}
		var f float64
		if err := json.Unmarshal(v.Value, &f); err != nil {
			return "", false, fmt.Errorf("invalid float %s from server: %w", v.Value, err)
		}
		return formatFloat(f), true, nil
	case hrana.TypeText:
		return quoteText(v.Str()), true, nil
	case hrana.TypeBlob:
		b, err := base64.StdEncoding.DecodeString(padBase64(v.Base64))
		if err != nil {
			b, err = base64.URLEncoding.DecodeString(padBase64(v.Base64))
		}
		if err != nil {
			return "", false, fmt.Errorf("invalid blob from server: %w", err)
		}
		return "X'" + strings.ToUpper(hex.EncodeToString(b)) + "'", true, nil
	}
	return "", false, fmt.Errorf("unknown value type %q from server", v.Type)
}

// padBase64 restores the padding sqld omits.
func padBase64(s string) string {
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return s
}

// valueSize approximates the in-memory size of a value, for page sizing.
func valueSize(v hrana.Value) int {
	return len(v.Value) + len(v.Base64) + 16
}
