package sqlscript

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func split(t *testing.T, in string) []string {
	t.Helper()
	var out []string
	sp := NewSplitter(strings.NewReader(in))
	for {
		st, err := sp.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("split %q: %v", in, err)
		}
		out = append(out, st.Text)
	}
}

func TestSplit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"simple", "SELECT 1; SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"no trailing semicolon", "SELECT 1;\nSELECT 2", []string{"SELECT 1", "SELECT 2"}},
		{"empty statements", ";;  ;\n-- c\n;SELECT 1;;", []string{"SELECT 1"}},
		{"string with semicolon", "INSERT INTO t VALUES('a;b');", []string{"INSERT INTO t VALUES('a;b')"}},
		{"escaped quote", "INSERT INTO t VALUES('it''s; ok');X;", []string{"INSERT INTO t VALUES('it''s; ok')", "X"}},
		{"double quoted identifier", `CREATE TABLE "a;""b"(x);`, []string{`CREATE TABLE "a;""b"(x)`}},
		{"backtick identifier", "SELECT `a;b` FROM t;", []string{"SELECT `a;b` FROM t"}},
		{"bracket identifier", "SELECT [a;b] FROM t;", []string{"SELECT [a;b] FROM t"}},
		{"line comment", "SELECT 1 -- ; not here\n;SELECT 2;", []string{"SELECT 1 -- ; not here", "SELECT 2"}},
		{"leading comments stripped", "-- header\n/* block; */\n  SELECT 1;", []string{"SELECT 1"}},
		{"block comment", "SELECT /* ; */ 1;", []string{"SELECT /* ; */ 1"}},
		{"column named begin", "CREATE TABLE t(begin INT, end INT);INSERT INTO t(begin,end) VALUES(1,2);",
			[]string{"CREATE TABLE t(begin INT, end INT)", "INSERT INTO t(begin,end) VALUES(1,2)"}},
		{"trigger", "CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO u VALUES(1); UPDATE u SET x=2; END;SELECT 1;",
			[]string{"CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO u VALUES(1); UPDATE u SET x=2; END", "SELECT 1"}},
		{"temp trigger lower case", "create temp trigger tr after insert on t begin select 1; end;x;",
			[]string{"create temp trigger tr after insert on t begin select 1; end", "x"}},
		{"temporary trigger", "CREATE TEMPORARY TRIGGER IF NOT EXISTS tr BEFORE DELETE ON t BEGIN SELECT 1; END;",
			[]string{"CREATE TEMPORARY TRIGGER IF NOT EXISTS tr BEFORE DELETE ON t BEGIN SELECT 1; END"}},
		{"trigger with case end", "CREATE TRIGGER tr AFTER UPDATE ON t BEGIN UPDATE t SET x = CASE WHEN new.y > 0 THEN 1 ELSE CASE WHEN 1 THEN 2 END END; SELECT CASE 1 WHEN 1 THEN 'end;' END; END;Z;",
			[]string{"CREATE TRIGGER tr AFTER UPDATE ON t BEGIN UPDATE t SET x = CASE WHEN new.y > 0 THEN 1 ELSE CASE WHEN 1 THEN 2 END END; SELECT CASE 1 WHEN 1 THEN 'end;' END; END", "Z"}},
		{"trigger with end column", "CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE t SET end = 1, x = CASE WHEN 1 THEN end END; END;",
			[]string{"CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE t SET end = 1, x = CASE WHEN 1 THEN end END; END"}},
		{"trigger with comments before end", "CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; -- c;\n /* ; */ END /* x */ ;Y;",
			[]string{"CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; -- c;\n /* ; */ END /* x */", "Y"}},
		{"quoted trigger keyword is not a trigger", `CREATE "TRIGGER" x; SELECT 1;`, []string{`CREATE "TRIGGER" x`, "SELECT 1"}},
		{"create table named trigger_x", "CREATE TABLE trigger_x(a);SELECT 1;", []string{"CREATE TABLE trigger_x(a)", "SELECT 1"}},
		{"blob literal", "INSERT INTO t VALUES(X'00ff', x'');", []string{"INSERT INTO t VALUES(X'00ff', x'')"}},
		{"utf8", "INSERT INTO t VALUES('héllo; 世界');", []string{"INSERT INTO t VALUES('héllo; 世界')"}},
		{"crlf", "SELECT 1;\r\nSELECT 2;\r\n", []string{"SELECT 1", "SELECT 2"}},
		{"explain create trigger", "EXPLAIN CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END;", []string{"EXPLAIN CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := split(t, c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestKeywordsAndTermination(t *testing.T) {
	sp := NewSplitter(strings.NewReader("-- x\nPRAGMA foreign_keys=OFF;\nBEGIN TRANSACTION;\n\"q\" x;\nCOMMIT"))
	var got [][]string
	var terminated []bool
	for {
		st, err := sp.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, st.Keywords)
		terminated = append(terminated, st.Terminated)
	}
	want := [][]string{{"PRAGMA", "FOREIGN_KEYS", "OFF"}, {"BEGIN", "TRANSACTION"}, {"X"}, {"COMMIT"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keywords %q", got)
	}
	if !reflect.DeepEqual(terminated, []bool{true, true, true, false}) {
		t.Fatalf("terminated %v", terminated)
	}
}

func TestIncomplete(t *testing.T) {
	for _, in := range []string{
		"SELECT 'abc",
		`SELECT "abc`,
		"SELECT [abc",
		"SELECT /* abc",
		"CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1;",
		"CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END",
	} {
		sp := NewSplitter(strings.NewReader("SELECT 0;" + in))
		if _, err := sp.Next(); err != nil {
			t.Fatal(err)
		}
		_, err := sp.Next()
		if !errors.Is(err, ErrIncomplete) {
			t.Errorf("%q: got %v, want ErrIncomplete", in, err)
		}
	}
}

func TestReaderErrorIsReturned(t *testing.T) {
	boom := errors.New("checksum mismatch")
	r := io.MultiReader(strings.NewReader("SELECT 1; SELECT 2"), iotest.ErrReader(boom))
	sp := NewSplitter(r)
	if st, err := sp.Next(); err != nil || st.Text != "SELECT 1" {
		t.Fatalf("%q %v", st.Text, err)
	}
	// The unterminated "SELECT 2" must not come out as a statement.
	if _, err := sp.Next(); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if _, err := sp.Next(); !errors.Is(err, boom) {
		t.Fatalf("error not sticky: %v", err)
	}
}

func TestMaxStatement(t *testing.T) {
	sp := NewSplitter(strings.NewReader("SELECT 1;SELECT '" + strings.Repeat("x", 100) + "';"))
	sp.Max = 50
	if _, err := sp.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Next(); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("got %v", err)
	}
}

func TestOneByteReader(t *testing.T) {
	in := "CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 'a;b'; END; INSERT INTO t VALUES('x''y'); -- c\n"
	want := split(t, in)
	var got []string
	sp := NewSplitter(iotest.OneByteReader(strings.NewReader(in)))
	for {
		st, err := sp.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, st.Text)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q != %q", got, want)
	}
}

// ---- comparison with SQLite's own sqlite3_complete() ----

// sqliteComplete is a direct port of sqlite3_complete() from SQLite's
// src/complete.c (the version without SQLITE_OMIT_TRIGGER). It is the
// reference the splitter is checked against.
func sqliteComplete(z []byte) bool {
	const (
		tkSEMI = iota
		tkWS
		tkOTHER
		tkEXPLAIN
		tkCREATE
		tkTEMP
		tkTRIGGER
		tkEND
	)
	trans := [8][8]int{
		/* 0 INVALID: */ {1, 0, 2, 3, 4, 2, 2, 2},
		/* 1   START: */ {1, 1, 2, 3, 4, 2, 2, 2},
		/* 2  NORMAL: */ {1, 2, 2, 2, 2, 2, 2, 2},
		/* 3 EXPLAIN: */ {1, 3, 3, 2, 4, 2, 2, 2},
		/* 4  CREATE: */ {1, 4, 2, 2, 2, 4, 5, 2},
		/* 5 TRIGGER: */ {6, 5, 5, 5, 5, 5, 5, 5},
		/* 6    SEMI: */ {6, 6, 5, 5, 5, 5, 5, 7},
		/* 7     END: */ {1, 7, 5, 5, 5, 5, 5, 5},
	}
	state := 0
	i := 0
	for i < len(z) {
		var token int
		switch c := z[i]; c {
		case ';':
			token = tkSEMI
		case ' ', '\r', '\t', '\n', '\f':
			token = tkWS
		case '/':
			if i+1 >= len(z) || z[i+1] != '*' {
				token = tkOTHER
				break
			}
			i += 2
			for i+1 < len(z) && (z[i] != '*' || z[i+1] != '/') {
				i++
			}
			if i+1 >= len(z) {
				return false
			}
			i++
			token = tkWS
		case '-':
			if i+1 >= len(z) || z[i+1] != '-' {
				token = tkOTHER
				break
			}
			for i < len(z) && z[i] != '\n' {
				i++
			}
			if i >= len(z) {
				return state == 1
			}
			token = tkWS
		case '[':
			i++
			for i < len(z) && z[i] != ']' {
				i++
			}
			if i >= len(z) {
				return false
			}
			token = tkOTHER
		case '`', '"', '\'':
			i++
			for i < len(z) && z[i] != c {
				i++
			}
			if i >= len(z) {
				return false
			}
			token = tkOTHER
		default:
			if isIDChar(c) {
				n := 1
				for i+n < len(z) && isIDChar(z[i+n]) {
					n++
				}
				switch strings.ToLower(string(z[i : i+n])) {
				case "create":
					token = tkCREATE
				case "trigger":
					token = tkTRIGGER
				case "temp", "temporary":
					token = tkTEMP
				case "end":
					token = tkEND
				case "explain":
					token = tkEXPLAIN
				default:
					token = tkOTHER
				}
				i += n - 1
			} else {
				token = tkOTHER
			}
		}
		state = trans[state][token]
		i++
	}
	return state == 1
}

// splitPoints returns the offsets just after each ';' that ends a statement,
// per the splitter, stopping at the first error.
func splitPoints(in []byte) []int {
	var out []int
	sp := NewSplitter(bytes.NewReader(in))
	for {
		st, empty, err := sp.next()
		if err != nil {
			return out
		}
		if st.Terminated || empty {
			out = append(out, int(st.End))
		}
	}
}

// referencePoints applies sqlite3_complete() the way the sqlite3 shell does:
// after each ';', is the text since the last statement complete?
func referencePoints(in []byte) []int {
	var out []int
	start := 0
	for i, c := range in {
		if c != ';' {
			continue
		}
		if sqliteComplete(in[start : i+1]) {
			out = append(out, i+1)
			start = i + 1
		}
	}
	return out
}

var fuzzTokens = []string{
	"CREATE ", "create ", "TRIGGER ", "trigger", "TEMP ", "TEMPORARY ", "EXPLAIN ", "END", "end ", "END;",
	"CASE ", "BEGIN ", "begin", "SELECT ", "x", "1", "_", "$", ";", ";", ";", " ", "\n", "\t", "'", "''", "\"", "`",
	"[", "]", "--", "-", "/*", "*/", "/", "*", "(", ")", ",", "é", "\r\n", "trigger_x ", "END_ ",
}

func randomScript(r *rand.Rand) []byte {
	var b bytes.Buffer
	n := 1 + r.Intn(40)
	for i := 0; i < n; i++ {
		b.WriteString(fuzzTokens[r.Intn(len(fuzzTokens))])
	}
	return b.Bytes()
}

func checkAgainstSQLite(t *testing.T, in []byte) {
	t.Helper()
	got, want := splitPoints(in), referencePoints(in)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("input %q\nsplitter %v\nsqlite3_complete %v", in, got, want)
	}
}

func TestMatchesSQLiteComplete(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200000; i++ {
		checkAgainstSQLite(t, randomScript(r))
	}
}

func FuzzSplitMatchesSQLiteComplete(f *testing.F) {
	for _, s := range []string{
		"SELECT 1;", "CREATE TRIGGER t BEGIN SELECT 1; END;", "CREATE TEMP TRIGGER x BEGIN SELECT CASE 1 WHEN 1 THEN 2 END; END; SELECT 2;",
		"'a;b';", "-- x;\n;", "/* ; */;", "[a;];", "EXPLAIN CREATE TRIGGER x BEGIN ; END ;",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		checkAgainstSQLite(t, in)
		// Streaming one byte at a time gives the same statements.
		var a, b []string
		for _, rd := range []struct {
			r   io.Reader
			dst *[]string
		}{{bytes.NewReader(in), &a}, {iotest.OneByteReader(bytes.NewReader(in)), &b}} {
			sp := NewSplitter(rd.r)
			for {
				st, err := sp.Next()
				if err != nil {
					break
				}
				*rd.dst = append(*rd.dst, st.Text)
			}
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("one-byte reader differs: %q vs %q", a, b)
		}
	})
}
