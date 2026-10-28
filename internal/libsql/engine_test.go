package libsql

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/jkaninda/bkupkit/config"
	"github.com/jkaninda/libsql-bkup/internal/hrana"
	"github.com/jkaninda/libsql-bkup/internal/sqlscript"
)

func values(t *testing.T, env map[string]string) *config.Values {
	t.Helper()
	r := config.NewRegistry()
	config.RegisterCommon(r)
	New().Settings(r)
	v, err := r.Resolve(config.ResolveOptions{Env: config.MapEnv(env)})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestResolveEndpoint(t *testing.T) {
	cases := []struct {
		env       map[string]string
		url, tok  string
		wantError string
	}{
		{env: map[string]string{}, url: "http://localhost:8080"},
		{env: map[string]string{"DB_HOST": "db", "DB_PORT": "9000", "LIBSQL_AUTH_TOKEN": "t"}, url: "http://db:9000", tok: "t"},
		{env: map[string]string{"DB_HOST": "::1"}, url: "http://[::1]:8080"},
		{env: map[string]string{"DB_AUTH_TOKEN": "old"}, url: "http://localhost:8080", tok: "old"},
		{env: map[string]string{"DB_URL": "http://h:8080", "DB_HOST": "ignored"}, url: "http://h:8080"},
		{env: map[string]string{"DB_URL": "libsql://mydb-org.turso.io"}, url: "https://mydb-org.turso.io"},
		{env: map[string]string{"DB_URL": "libsql://h:8080?tls=0"}, url: "http://h:8080"},
		{env: map[string]string{"DB_URL": "libsql://h?tls=0"}, wantError: "explicit port"},
		{env: map[string]string{"DB_URL": "wss://h/"}, url: "https://h"},
		{env: map[string]string{"DB_URL": "ws://h:8080"}, url: "http://h:8080"},
		{env: map[string]string{"DB_URL": "http://h:8080?authToken=fromurl"}, url: "http://h:8080", tok: "fromurl"},
		{env: map[string]string{"DB_URL": "http://h:8080?jwt=fromurl", "LIBSQL_AUTH_TOKEN": "env"}, url: "http://h:8080", tok: "env"},
		{env: map[string]string{"DB_URL": "http://h:8080?foo=1"}, wantError: "unknown query parameter"},
		{env: map[string]string{"DB_URL": "https://h?tls=0"}, wantError: "tls=0"},
		{env: map[string]string{"DB_URL": "file:///tmp/x.db"}, wantError: "missing host"},
		{env: map[string]string{"DB_URL": "postgres://h"}, wantError: "unsupported scheme"},
		{env: map[string]string{"DB_HOST": "https://ns.example.com"}, url: "https://ns.example.com"},
	}
	for _, c := range cases {
		ep, err := resolveEndpoint(values(t, c.env))
		if c.wantError != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantError) {
				t.Errorf("%v: got %v, want error %q", c.env, err, c.wantError)
			}
			continue
		}
		if err != nil || ep.URL != c.url || ep.Token != c.tok {
			t.Errorf("%v: got %+v %v, want %s %s", c.env, ep, err, c.url, c.tok)
		}
	}
}

func TestURLErrorDoesNotLeakToken(t *testing.T) {
	_, err := parseURL("http://h:80%zz?authToken=secret", "")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("got %v", err)
	}
}

func TestTargetName(t *testing.T) {
	for env, want := range map[string]string{
		"DB_NAME=shop":                        "shop",
		"DB_URL=libsql://mydb-org.turso.io":   "mydb-org",
		"DB_URL=http://127.0.0.1:8080":        "libsql",
		"DB_HOST=localhost":                   "libsql",
		"DB_URL=http://tenant1.sqld.internal": "tenant1",
	} {
		k, val, _ := strings.Cut(env, "=")
		v := values(t, map[string]string{k: val})
		ts, err := New().Targets(context.Background(), v)
		if err != nil || len(ts) != 1 || ts[0].Name != want {
			t.Errorf("%s: %+v %v, want %s", env, ts, err, want)
		}
	}
}

func TestSettings(t *testing.T) {
	r := config.NewRegistry()
	config.RegisterCommon(r)
	New().Settings(r)
	for _, k := range []string{"DB_USERNAME", "DB_PASSWORD", "TARGET_DB_URL", "BACKUP_ALL_DATABASES"} {
		if r.Get(k) != nil {
			t.Errorf("%s should not be registered", k)
		}
	}
	if s := r.Get(AuthToken); s == nil || !s.Secret {
		t.Fatal("LIBSQL_AUTH_TOKEN must be a secret")
	}
	v := values(t, map[string]string{"RESTORE_BATCH_SIZE": "0"})
	if err := New().Configure(context.Background(), v); err == nil {
		t.Fatal("batch size 0 accepted")
	}
	v = values(t, map[string]string{})
	if v.Int(RestoreBatchSize) != 500 || v.Bool(RestoreAtomic) {
		t.Fatal("defaults")
	}
}

func TestLiterals(t *testing.T) {
	raw := func(typ, js string) hrana.Value { return hrana.Value{Type: typ, Value: json.RawMessage(js)} }
	cases := []struct {
		v    hrana.Value
		want string
	}{
		{hrana.Value{Type: "null"}, "NULL"},
		{raw("integer", `"9223372036854775807"`), "9223372036854775807"},
		{raw("integer", `"-9223372036854775808"`), "-9223372036854775808"},
		{raw("float", `3`), "3.0"},
		{raw("float", `-0.5`), "-0.5"},
		{raw("float", `0.1`), "0.1"},
		{raw("float", `1e300`), "1e+300"},
		{raw("float", `1.7976931348623157e308`), "1.7976931348623157e+308"},
		{raw("float", `5e-324`), "5e-324"},
		{raw("float", `123456789012345680000`), "1.2345678901234568e+20"},
		{raw("text", `"it's"`), "'it''s'"},
		{raw("text", `"a\u0000b"`), "CAST(X'610062' AS TEXT)"},
		{raw("text", `"é;\n"`), "'é;\n'"},
		{hrana.Value{Type: "blob", Base64: "AP8"}, "X'00FF'"},
		{hrana.Value{Type: "blob", Base64: "-_8"}, "X'FBFF'"},
		{hrana.Value{Type: "blob"}, "X''"},
	}
	for _, c := range cases {
		got, ok, err := literal(c.v)
		if err != nil || !ok || got != c.want {
			t.Errorf("%+v: got %q %v %v, want %q", c.v, got, ok, err, c.want)
		}
	}
	if _, ok, err := literal(raw("float", "null")); ok || err != nil {
		t.Error("null float must ask for the sign")
	}
	if _, _, err := literal(raw("integer", `"1.5"`)); err == nil {
		t.Error("bad integer accepted")
	}
	if formatFloat(math.Inf(1)) != "9e999" || formatFloat(math.Inf(-1)) != "-9e999" || formatFloat(math.NaN()) != "NULL" {
		t.Error("special floats")
	}
	if quoteIdent(`a"b`) != `"a""b"` {
		t.Error("quoteIdent")
	}
}

func TestTerminate(t *testing.T) {
	for in, want := range map[string]string{
		"CREATE TABLE t(a)":                    "CREATE TABLE t(a);\n",
		"CREATE TABLE t(a);":                   "CREATE TABLE t(a);\n",
		"CREATE TABLE t(a -- note\n)":          "CREATE TABLE t(a -- note\n);\n",
		"CREATE TABLE t(a) -- trailing":        "CREATE TABLE t(a) -- trailing\n;\n",
		"CREATE TRIGGER x BEGIN SELECT 1; END": "CREATE TRIGGER x BEGIN SELECT 1; END;\n",
	} {
		if got := terminate(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestWithoutRowid(t *testing.T) {
	for sql, want := range map[string]bool{
		"CREATE TABLE t(a PRIMARY KEY) WITHOUT ROWID":         true,
		"CREATE TABLE t(a PRIMARY KEY)without rowid,strict":   true,
		"CREATE TABLE t(a PRIMARY KEY) STRICT, WITHOUT ROWID": true,
		"CREATE TABLE t(without_rowid INT)":                   false,
		"CREATE TABLE t(a)":                                   false,
	} {
		if withoutRowid(sql) != want {
			t.Errorf("%q", sql)
		}
	}
}

func TestVirtualModule(t *testing.T) {
	for sql, want := range map[string]string{
		"CREATE VIRTUAL TABLE docs USING fts5(title, body)": "fts5",
		"create virtual table x using FTS4 (a)":             "fts4",
		"CREATE VIRTUAL TABLE rt USING rtree(id, a, b)":     "rtree",
		"CREATE TABLE t(a)":                                 "",
	} {
		if got := virtualModule(sql); got != want {
			t.Errorf("%q: %q", sql, got)
		}
	}
}

func TestNextPage(t *testing.T) {
	if n := nextPage(256, 256, 256*100); n != 1024 {
		t.Errorf("small rows: %d", n) // capped at 4x growth
	}
	if n := nextPage(256, 256, 256*1<<20); n != 4 {
		t.Errorf("1 MiB rows: %d", n)
	}
	if n := nextPage(1, 1, 100<<20); n != pageMinRows {
		t.Errorf("huge rows: %d", n)
	}
	if n := nextPage(5000, 5000, 5000); n != pageMaxRows {
		t.Errorf("max: %d", n)
	}
}

func TestClassify(t *testing.T) {
	sp := sqlscript.NewSplitter(strings.NewReader("PRAGMA foreign_keys=OFF; BEGIN TRANSACTION; COMMIT; END; ROLLBACK TO sp; ROLLBACK TRANSACTION TO SAVEPOINT sp; SELECT 1; ROLLBACK; -- due to errors"))
	want := []string{"skip", "skip", "skip", "skip", "run", "run", "run", "error"}
	for i := 0; ; i++ {
		st, err := sp.Next()
		if err == io.EOF {
			break
		}
		skip, cerr := classify(st)
		got := "run"
		if cerr != nil {
			got = "error"
		} else if skip {
			got = "skip"
		}
		if got != want[i] {
			t.Errorf("%q: %s, want %s", st.Text, got, want[i])
		}
	}
}

// recorder is a fake Hrana server recording what a restore sends. A
// statement containing FAIL errors.
type recorder struct {
	mu       sync.Mutex
	executes []string   // single statements
	batches  [][]string // batch step SQL
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var req struct {
		Requests []struct {
			Type  string      `json:"type"`
			Stmt  *hrana.Stmt `json:"stmt"`
			Batch *struct {
				Steps []hrana.Step `json:"steps"`
			} `json:"batch"`
		} `json:"requests"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var results []any
	for _, rq := range req.Requests {
		switch rq.Type {
		case "execute":
			rc.executes = append(rc.executes, rq.Stmt.SQL)
			results = append(results, map[string]any{"type": "ok", "response": map[string]any{"type": "execute", "result": map[string]any{"cols": []any{}, "rows": []any{}}}})
		case "batch":
			var sqls []string
			res := make([]any, len(rq.Batch.Steps))
			errs := make([]any, len(rq.Batch.Steps))
			failed := -1
			for i, st := range rq.Batch.Steps {
				sqls = append(sqls, st.Stmt.SQL)
				run := st.Condition == nil
				if c := st.Condition; c != nil {
					switch c.Type {
					case "ok":
						run = res[*c.Step] != nil
					case "not":
						run = res[*c.Cond.Step] == nil
					}
				}
				if !run {
					continue
				}
				if strings.Contains(st.Stmt.SQL, "FAIL") {
					errs[i] = map[string]any{"message": "boom"}
					failed = i
					continue
				}
				res[i] = map[string]any{"cols": []any{}, "rows": []any{}}
			}
			_ = failed
			rc.batches = append(rc.batches, sqls)
			results = append(results, map[string]any{"type": "ok", "response": map[string]any{"type": "batch", "result": map[string]any{"step_results": res, "step_errors": errs}}})
		case "close":
			results = append(results, map[string]any{"type": "ok", "response": map[string]any{"type": "close"}})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"baton": "b", "results": results})
}

func runReplay(t *testing.T, in io.Reader, o restoreOptions) (*recorder, restoreStats, error) {
	t.Helper()
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	s := (&hrana.Client{URL: srv.URL}).OpenStream()
	st, err := replay(context.Background(), s, in, o)
	return rc, st, err
}

const script = "PRAGMA foreign_keys=OFF;\nBEGIN TRANSACTION;\nS1;\nS2;\nS3;\nS4;\nS5;\nCOMMIT;\n"

func TestReplayBatches(t *testing.T) {
	rc, st, err := runReplay(t, strings.NewReader(script), restoreOptions{BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if st.Statements != 5 || st.Batches != 3 || st.Skipped != 3 {
		t.Fatalf("stats %+v", st)
	}
	if strings.Join(rc.executes, "|") != "PRAGMA foreign_keys=OFF" {
		t.Fatalf("executes %q", rc.executes)
	}
	want := [][]string{
		{"BEGIN", "S1", "S2", "COMMIT", "ROLLBACK"},
		{"BEGIN", "S3", "S4", "COMMIT", "ROLLBACK"},
		{"BEGIN", "S5", "COMMIT", "ROLLBACK"},
	}
	if len(rc.batches) != len(want) {
		t.Fatalf("batches %q", rc.batches)
	}
	for i := range want {
		if strings.Join(rc.batches[i], "|") != strings.Join(want[i], "|") {
			t.Fatalf("batch %d: %q", i, rc.batches[i])
		}
	}
}

func TestReplayAtomic(t *testing.T) {
	rc, _, err := runReplay(t, strings.NewReader(script), restoreOptions{BatchSize: 3, Atomic: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rc.executes, "|") != "PRAGMA foreign_keys=OFF|BEGIN|COMMIT" {
		t.Fatalf("executes %q", rc.executes)
	}
	if strings.Join(rc.batches[0], "|") != "S1|S2|S3" || strings.Join(rc.batches[1], "|") != "S4|S5" {
		t.Fatalf("batches %q", rc.batches)
	}
}

func TestReplayReaderErrorSendsNoPartialBatch(t *testing.T) {
	boom := errors.New("gpg: MDC check failed")
	for _, atomic := range []bool{false, true} {
		in := io.MultiReader(strings.NewReader("S1;S2;S3;S4"), iotest.ErrReader(boom))
		rc, st, err := runReplay(t, in, restoreOptions{BatchSize: 2, Atomic: atomic})
		if !errors.Is(err, boom) {
			t.Fatalf("atomic=%v: got %v", atomic, err)
		}
		// S1,S2 were a complete batch; S3 and the cut "S4" never leave.
		if len(rc.batches) != 1 || st.Statements != 2 {
			t.Fatalf("atomic=%v: batches %q", atomic, rc.batches)
		}
		last := rc.executes[len(rc.executes)-1]
		if atomic && last != "ROLLBACK" {
			t.Fatalf("atomic restore not rolled back: %q", rc.executes)
		}
		if !atomic && last == "ROLLBACK" {
			t.Fatalf("unexpected rollback: %q", rc.executes)
		}
	}
}

func TestReplayStatementError(t *testing.T) {
	rc, _, err := runReplay(t, strings.NewReader("S1;\nS2;\nFAIL here;\nS4;"), restoreOptions{BatchSize: 10})
	if err == nil || !strings.Contains(err.Error(), "line 3") || !strings.Contains(err.Error(), "FAIL here") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
	if len(rc.batches) != 1 {
		t.Fatalf("batches %q", rc.batches)
	}
	_, _, err = runReplay(t, strings.NewReader("S1;\nFAIL;"), restoreOptions{BatchSize: 10, Atomic: true})
	if err == nil {
		t.Fatal("atomic: no error")
	}
}

func TestReplayRejectsFailedDump(t *testing.T) {
	_, _, err := runReplay(t, strings.NewReader("S1;\nROLLBACK; -- due to errors\n"), restoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK") {
		t.Fatalf("got %v", err)
	}
}

func TestReplayTruncatedInput(t *testing.T) {
	_, _, err := runReplay(t, strings.NewReader("S1;\nINSERT INTO t VALUES('abc"), restoreOptions{})
	if !errors.Is(err, sqlscript.ErrIncomplete) {
		t.Fatalf("got %v", err)
	}
}
