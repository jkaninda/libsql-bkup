//go:build integration && unix

package libsql

// Integration tests against a real libSQL server (sqld) and MinIO, started
// through the docker CLI:
//
//	go test -tags integration ./...
//
// They skip when docker is unavailable. LIBSQL_BKUP_SQLD_IMAGE and
// LIBSQL_BKUP_MINIO_IMAGE override the images.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jkaninda/bkupkit/cli"
	"github.com/jkaninda/libsql-bkup/internal/hrana"
)

func requireDocker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	// Stdout only: "docker run" prints image pull progress on stderr, which would
	// otherwise end up in the container id on a runner without the image.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.Bytes())
	}
	return strings.TrimSpace(string(out))
}

func image(envKey, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return def
}

func publishedPort(t *testing.T, id, port string) string {
	t.Helper()
	hp := strings.Split(docker(t, "port", id, port), "\n")[0]
	_, p, err := net.SplitHostPort(hp)
	if err != nil {
		t.Fatalf("docker port %q: %v", hp, err)
	}
	return p
}

// sqld is a running libSQL server.
type sqld struct {
	host, port string
	token      string
}

func (s *sqld) url() string { return "http://" + s.host + ":" + s.port }

func (s *sqld) stream() *hrana.Stream {
	return (&hrana.Client{URL: s.url(), Token: s.token}).OpenStream()
}

// startSQLD runs sqld; with auth, clients need a JWT signed by a fresh key.
func startSQLD(t *testing.T, auth bool) *sqld {
	t.Helper()
	requireDocker(t)
	args := []string{"run", "-d", "-p", "127.0.0.1::8080"}
	s := &sqld{host: "127.0.0.1"}
	if auth {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, "-e", "SQLD_AUTH_JWT_KEY="+base64.RawURLEncoding.EncodeToString(pub))
		s.token = jwt(t, priv)
	}
	args = append(args, image("LIBSQL_BKUP_SQLD_IMAGE", "ghcr.io/tursodatabase/libsql-server:latest"))
	id := docker(t, args...)
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", "--tail", "30", id).CombinedOutput()
			t.Logf("sqld logs:\n%s", out)
		}
		_ = exec.Command("docker", "rm", "-f", "-v", id).Run()
	})
	s.port = publishedPort(t, id, "8080/tcp")
	// Wait until several consecutive queries succeed (the published port
	// accepts connections before sqld serves them).
	deadline := time.Now().Add(60 * time.Second)
	okCount := 0
	for okCount < 3 {
		st := s.stream()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := st.Query(ctx, "SELECT 1")
		_ = st.Close(ctx)
		cancel()
		if err == nil {
			okCount++
			continue
		}
		okCount = 0
		if time.Now().After(deadline) {
			t.Fatalf("sqld not ready: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return s
}

func jwt(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	msg := enc(map[string]string{"alg": "EdDSA", "typ": "JWT"}) + "." + enc(map[string]any{"iat": time.Now().Unix(), "a": "rw"})
	return msg + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(msg)))
}

// exec runs statements on one stream, failing the test on error.
func (s *sqld) exec(t *testing.T, stmts ...string) {
	t.Helper()
	st := s.stream()
	defer func() { _ = st.Close(context.Background()) }()
	for _, q := range stmts {
		if err := st.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func (s *sqld) scalar(t *testing.T, q string) string {
	t.Helper()
	st := s.stream()
	defer func() { _ = st.Close(context.Background()) }()
	res, err := st.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("%s: %d rows", q, len(res.Rows))
	}
	return res.Rows[0][0].Str()
}

// dumpText dumps the server with the engine's dumper.
func (s *sqld) dumpText(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	st := s.stream()
	defer func() { _ = st.Close(context.Background()) }()
	if err := dump(context.Background(), st, &b, nil); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

const bigRows = 60000

// seed creates every kind of object and value the dumper must handle.
func seed(t *testing.T, s *sqld) {
	t.Helper()
	s.exec(t,
		`CREATE TABLE parent(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE "odd ""name"""(id INTEGER PRIMARY KEY AUTOINCREMENT, "begin" TEXT, [end] REAL, `+"`case`"+` BLOB,
			parent_id INTEGER REFERENCES parent(id),
			doubled INT GENERATED ALWAYS AS (id * 2) VIRTUAL, tripled INT GENERATED ALWAYS AS (id * 3) STORED)`,
		// The child rows come before the parent rows in the dump (table order);
		// foreign_keys=OFF during restore makes that fine.
		`PRAGMA foreign_keys=ON`,
		`INSERT INTO parent VALUES (1, 'p1'), (2, 'p2')`,
		`INSERT INTO "odd ""name"""("begin", [end], `+"`case`"+`, parent_id) VALUES
			('it''s; a ''test''', 1.5, X'00FF10', 1),
			('BEGIN; END; CASE', 9e999, x'', 2),
			(NULL, -9e999, NULL, NULL),
			('héllo 世界 🎉', 0.1, randomblob(32), 1),
			('line1
line2', -0.0, NULL, NULL),
			('gone', 1e-300, NULL, NULL)`,
		`DELETE FROM "odd ""name""" WHERE "begin" = 'gone'`,
		`CREATE TABLE types(i INTEGER, r REAL, t TEXT, b BLOB, n NUMERIC, any_col)`,
		`INSERT INTO types VALUES
			(9223372036854775807, 1.7976931348623157e308, '', X'', 1e3, 'text'),
			(-9223372036854775808, 5e-324, 'x', X'00', '12.50', 42),
			(0, 3.0, 'nul'||char(0)||'byte', zeroblob(3), NULL, 2.5),
			(NULL, NULL, NULL, NULL, NULL, X'DEADBEEF')`,
		`CREATE TABLE kv(k1 TEXT, k2 INTEGER, v, PRIMARY KEY (k2, k1)) WITHOUT ROWID`,
		`INSERT INTO kv VALUES ('b', 1, 'one-b'), ('a', 1, 'one-a'), ('a', 2, 9e999)`,
		`CREATE TABLE strict_t(id INTEGER PRIMARY KEY, x TEXT, y REAL) STRICT`,
		`INSERT INTO strict_t VALUES (1, 'a', 1.0), (2, NULL, 2.25)`,
		`CREATE TABLE noipk(a, b)`,
		`INSERT INTO noipk VALUES (1, 'x'), (2, 'y'), (3, 'z')`,
		`DELETE FROM noipk WHERE a = 2`,
		`CREATE TABLE log(id INTEGER PRIMARY KEY AUTOINCREMENT, msg TEXT)`,
		`CREATE TRIGGER "trg ""quoted""" AFTER INSERT ON parent BEGIN
			INSERT INTO log(msg) VALUES (CASE WHEN new.id > 10 THEN 'big; ' || new.name ELSE CASE new.id WHEN 1 THEN 'one' ELSE 'small' END END);
			UPDATE log SET msg = msg || ' end' WHERE id = last_insert_rowid();
		END`,
		`INSERT INTO parent VALUES (11, 'p11')`,
		`CREATE VIEW v_odd AS SELECT id, "begin" FROM "odd ""name""" WHERE id > 1`,
		`CREATE VIEW v_v AS SELECT count(*) AS n FROM v_odd`,
		`CREATE INDEX "idx ""odd""" ON "odd ""name"""("begin", [end])`,
		`CREATE UNIQUE INDEX idx_kv_v ON kv(v) WHERE v IS NOT NULL`,
		`CREATE VIRTUAL TABLE docs USING fts5(title, body)`,
		`INSERT INTO docs(rowid, title, body) VALUES (5, 'a', 'hello world'), (9, 'b', 'bye; end')`,
		`CREATE VIRTUAL TABLE rt USING rtree(id, minx, maxx)`,
		`INSERT INTO rt VALUES (1, 0, 10), (7, -5, 5)`,
		`CREATE TABLE big(id INTEGER PRIMARY KEY, payload BLOB, label TEXT, score REAL)`,
		fmt.Sprintf(`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < %d)
			INSERT INTO big SELECT x, randomblob(16), 'row ' || x || ';', x * 0.25 FROM c`, bigRows),
	)
}

// run executes the CLI in process with the current environment and returns
// its combined output and exit code.
var runMu sync.Mutex

func run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	runMu.Lock()
	defer runMu.Unlock()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	code := (&cli.App{Engine: New(), Version: "test"}).Run(args)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = w.Close()
	out := string(<-done)
	return out, code
}

// useServer points the environment at s and isolates local state.
func useServer(t *testing.T, s *sqld) string {
	t.Helper()
	store := t.TempDir()
	t.Setenv("STORAGE_PATH", store)
	t.Setenv("HEALTH_FILE", filepath.Join(t.TempDir(), "health.json"))
	t.Setenv("TMP_DIR", t.TempDir())
	t.Setenv("TEMPLATES_DIR", "")
	t.Setenv("DB_URL", s.url())
	t.Setenv("LIBSQL_AUTH_TOKEN", s.token)
	return store
}

var miabiDB = regexp.MustCompile(`[\w.\-]+\.(?:sql|archive)\.gz(?:\.gpg)?`)

func lastArtifact(t *testing.T, out string) string {
	t.Helper()
	m := miabiDB.FindAllString(out, -1)
	if len(m) == 0 {
		t.Fatalf("no artifact name in output:\n%s", out)
	}
	return m[len(m)-1]
}

var fileRe = regexp.MustCompile(`file=([\w.\-]+)`)

func lastFile(t *testing.T, out string) string {
	t.Helper()
	m := fileRe.FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		t.Fatalf("no file= in output:\n%s", out)
	}
	return m[len(m)-1][1]
}

// TestIntegrationRoundTrip: dump → restore → dump gives the same text, with
// AUTOINCREMENT counters, generated columns, triggers and 60k rows.
func TestIntegrationRoundTrip(t *testing.T) {
	src, dst := startSQLD(t, false), startSQLD(t, false)
	seed(t, src)
	want := src.dumpText(t)
	for _, needle := range []string{
		"DELETE FROM sqlite_sequence WHERE name = 'odd \"name\"';",
		"INSERT INTO sqlite_sequence(name,seq) VALUES('odd \"name\"',6);",
		"9e999", "-9e999", "-9223372036854775808", "CAST(X'6E756C0062797465' AS TEXT)",
		`INSERT INTO "noipk"(rowid,"a","b") VALUES(3,3,'z');`,
	} {
		if !strings.Contains(want, needle) {
			t.Errorf("dump lacks %q", needle)
		}
	}
	if strings.Contains(want, `"doubled"`) || strings.Contains(want, `"tripled",`) {
		t.Error("generated columns must not be in INSERTs")
	}

	store := useServer(t, src)
	out, code := run(t, "backup", "-d", "shop")
	if code != 0 {
		t.Fatalf("backup exit %d\n%s", code, out)
	}
	file := lastArtifact(t, out)
	if !strings.HasSuffix(file, ".sql.gz") {
		t.Fatalf("artifact %q", file)
	}
	if _, err := os.Stat(filepath.Join(store, file)); err != nil {
		t.Fatal(err)
	}

	// Restore twice into a server that already has conflicting objects: the
	// dump's DROP … IF EXISTS replaces them.
	dst.exec(t, `CREATE TABLE big(x)`, `INSERT INTO big VALUES ('old')`, `CREATE VIEW v_odd AS SELECT 1`)
	t.Setenv("DB_URL", dst.url())
	for i := 0; i < 2; i++ {
		if out, code := run(t, "restore", "-d", "shop", "-f", file); code != 0 {
			t.Fatalf("restore exit %d\n%s", code, out)
		}
	}
	if got := dst.dumpText(t); got != want {
		t.Fatalf("round trip differs:\n%s", firstDiff(want, got))
	}
	if n := dst.scalar(t, "SELECT count(*) FROM big"); n != fmt.Sprint(bigRows) {
		t.Fatalf("big rows %s", n)
	}
	// AUTOINCREMENT continues after the deleted row 6, generated columns compute.
	if got := dst.scalar(t, `INSERT INTO "odd ""name"""("begin") VALUES ('new') RETURNING id || ',' || doubled || ',' || tripled`); got != "7,14,21" {
		t.Fatalf("after restore: %s", got)
	}
	// The trigger works (and was not fired by the restore).
	if got := dst.scalar(t, "SELECT count(*) FROM log"); got != "1" {
		t.Fatalf("log rows %s", got)
	}
	dst.exec(t, `INSERT INTO parent VALUES (1000, 'p1000')`)
	if got := dst.scalar(t, "SELECT msg FROM log ORDER BY id DESC LIMIT 1"); got != "big; p1000 end" {
		t.Fatalf("trigger: %q", got)
	}
	if got := dst.scalar(t, "SELECT rowid FROM docs WHERE docs MATCH 'hello'"); got != "5" {
		t.Fatalf("fts: %s", got)
	}
	if got := dst.scalar(t, "SELECT id FROM rt WHERE minx < -1"); got != "7" {
		t.Fatalf("rtree: %s", got)
	}
	if got := dst.scalar(t, "SELECT typeof(i) || typeof(r) FROM types WHERE i < 0"); got != "integerreal" {
		t.Fatalf("types %s", got)
	}

	// Atomic restore of the same artifact, small batches.
	dst2 := startSQLD(t, false)
	t.Setenv("DB_URL", dst2.url())
	if out, code := run(t, "restore", "-d", "shop", "-f", file, "--atomic", "--batch-size", "50"); code != 0 {
		t.Fatalf("atomic restore exit %d\n%s", code, out)
	}
	if got := dst2.dumpText(t); got != want {
		t.Fatalf("atomic round trip differs:\n%s", firstDiff(want, got))
	}
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return fmt.Sprintf("line %d:\nwant %s\ngot  %s", i+1, al[i], bl[i])
		}
	}
	return fmt.Sprintf("lengths %d vs %d lines", len(al), len(bl))
}

// TestIntegrationSnapshot: the dump reads one snapshot while other
// connections write, so a Hrana stream keeps its transaction.
func TestIntegrationSnapshot(t *testing.T) {
	s := startSQLD(t, false)
	s.exec(t, `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 20000) INSERT INTO t SELECT x, hex(randomblob(20)) FROM c`)

	// Plain transaction pinning.
	ctx := context.Background()
	a := s.stream()
	defer func() { _ = a.Close(ctx) }()
	if err := a.Exec(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	count := func(st *hrana.Stream) string {
		res, err := st.Query(ctx, "SELECT count(*) FROM t")
		if err != nil {
			t.Fatal(err)
		}
		return res.Rows[0][0].Str()
	}
	if count(a) != "20000" {
		t.Fatal("count")
	}
	s.exec(t, `INSERT INTO t(v) VALUES ('concurrent')`)
	if got := count(a); got != "20000" {
		t.Fatalf("stream sees a concurrent write inside its transaction: %s", got)
	}
	if err := a.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if got := count(a); got != "20001" {
		t.Fatalf("after rollback: %s", got)
	}

	// A dump during writes: rows inserted after the dump started are absent.
	w := &writeHook{hook: func() { s.exec(t, `INSERT INTO t(v) SELECT 'late' FROM t LIMIT 500`) }}
	st := s.stream()
	defer func() { _ = st.Close(ctx) }()
	if err := dump(ctx, st, w, nil); err != nil {
		t.Fatal(err)
	}
	if !w.fired {
		t.Fatal("hook did not run during the dump")
	}
	if n := strings.Count(w.buf.String(), `INSERT INTO "t"`); n != 20001 {
		t.Fatalf("dump has %d rows, want the 20001 of its snapshot", n)
	}
	if got := s.scalar(t, "SELECT count(*) FROM t"); got != "20501" {
		t.Fatalf("server rows %s", got)
	}
}

type writeHook struct {
	buf   bytes.Buffer
	hook  func()
	fired bool
}

func (w *writeHook) Write(p []byte) (int, error) {
	if !w.fired {
		w.fired = true
		w.hook()
	}
	return w.buf.Write(p)
}

// TestIntegrationEncryption: GPG passphrase and age recipient backups restore.
func TestIntegrationEncryption(t *testing.T) {
	src, dst := startSQLD(t, false), startSQLD(t, false)
	seed(t, src)
	want := src.dumpText(t)
	useServer(t, src)

	t.Run("gpg", func(t *testing.T) {
		t.Setenv("DB_URL", src.url())
		t.Setenv("GPG_PASSPHRASE", "s3cret")
		out, code := run(t, "backup", "-d", "shop")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		file := lastArtifact(t, out)
		if !strings.HasSuffix(file, ".sql.gz.gpg") {
			t.Fatalf("artifact %q", file)
		}
		t.Setenv("DB_URL", dst.url())
		t.Setenv("GPG_PASSPHRASE", "wrong")
		if out, code := run(t, "restore", "-d", "shop", "-f", file); code != cli.ExitCrypto {
			t.Fatalf("wrong passphrase: exit %d\n%s", code, out)
		}
		t.Setenv("GPG_PASSPHRASE", "s3cret")
		if out, code := run(t, "restore", "-d", "shop", "-f", file); code != 0 {
			t.Fatalf("restore exit %d\n%s", code, out)
		}
		if got := dst.dumpText(t); got != want {
			t.Fatalf("differs:\n%s", firstDiff(want, got))
		}
	})

	t.Run("age", func(t *testing.T) {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		dst.exec(t, "DROP TABLE big")
		t.Setenv("DB_URL", src.url())
		t.Setenv("GPG_PASSPHRASE", "")
		t.Setenv("AGE_RECIPIENTS", id.Recipient().String())
		out, code := run(t, "backup", "-d", "shop")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		file := lastFile(t, out)
		if !strings.HasSuffix(file, ".sql.gz.age") {
			t.Fatalf("artifact %q", file)
		}
		t.Setenv("DB_URL", dst.url())
		t.Setenv("AGE_IDENTITY", id.String())
		if out, code := run(t, "restore", "-d", "shop", "-f", file); code != 0 {
			t.Fatalf("restore exit %d\n%s", code, out)
		}
		if got := dst.dumpText(t); got != want {
			t.Fatalf("differs:\n%s", firstDiff(want, got))
		}
	})
}

// TestIntegrationCorruptInput: damaged artifacts and failing statements
// fail the restore; atomic restores leave the database unchanged.
func TestIntegrationCorruptInput(t *testing.T) {
	src, dst := startSQLD(t, false), startSQLD(t, false)
	seed(t, src)
	store := useServer(t, src)
	t.Setenv("GPG_PASSPHRASE", "pw")
	out, code := run(t, "backup", "-d", "shop")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	file := lastArtifact(t, out)
	t.Setenv("GPG_PASSPHRASE", "")
	out, code = run(t, "backup", "-d", "shop", "--name", "plain")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	plain := lastArtifact(t, out)

	t.Setenv("DB_URL", dst.url())
	dst.exec(t, `CREATE TABLE sentinel(x)`, `INSERT INTO sentinel VALUES (1), (2)`)
	before := dst.dumpText(t)
	unchanged := func(t *testing.T) {
		t.Helper()
		if got := dst.dumpText(t); got != before {
			t.Fatalf("database changed:\n%s", firstDiff(before, got))
		}
	}
	data, err := os.ReadFile(filepath.Join(store, file))
	if err != nil {
		t.Fatal(err)
	}
	put := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(store, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("gpg flipped byte, atomic", func(t *testing.T) {
		t.Setenv("GPG_PASSPHRASE", "pw")
		bad := bytes.Clone(data)
		bad[len(bad)*2/3] ^= 0xFF
		put("flipped.sql.gz.gpg", bad)
		out, code := run(t, "restore", "-d", "shop", "-f", "flipped.sql.gz.gpg", "--atomic")
		if code == 0 {
			t.Fatalf("corrupted restore succeeded\n%s", out)
		}
		unchanged(t)
	})

	t.Run("gpg truncated, atomic", func(t *testing.T) {
		t.Setenv("GPG_PASSPHRASE", "pw")
		put("cut.sql.gz.gpg", data[:len(data)/2])
		if out, code := run(t, "restore", "-d", "shop", "-f", "cut.sql.gz.gpg", "--atomic"); code == 0 {
			t.Fatalf("truncated restore succeeded\n%s", out)
		}
		unchanged(t)
	})

	t.Run("gzip truncated, batches", func(t *testing.T) {
		pd, err := os.ReadFile(filepath.Join(store, plain))
		if err != nil {
			t.Fatal(err)
		}
		put("cut.sql.gz", pd[:len(pd)*3/4])
		out, code := run(t, "restore", "-d", "shop", "-f", "cut.sql.gz")
		if code == 0 {
			t.Fatalf("truncated restore succeeded\n%s", out)
		}
		if !strings.Contains(out, "already committed") {
			t.Fatalf("partial restore not reported:\n%s", out)
		}
		// Reset the target for the next cases.
		dst.exec(t, "DROP TABLE IF EXISTS big")
		before = dst.dumpText(t)
	})

	t.Run("checksum mismatch", func(t *testing.T) {
		// Point the index at a different checksum: the restore must fail.
		ix := filepath.Join(store, "libsql-bkup-metadata.json")
		b, err := os.ReadFile(ix)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		for _, e := range doc["entries"].([]any) {
			m := e.(map[string]any)
			if m["file"] == plain {
				m["sha256"] = strings.Repeat("0", 64)
			}
		}
		b, _ = json.Marshal(doc)
		put("libsql-bkup-metadata.json", b)
		out, code := run(t, "restore", "-d", "shop", "-f", plain, "--atomic")
		if code == 0 || !strings.Contains(out, "checksum") {
			t.Fatalf("checksum mismatch accepted (exit %d)\n%s", code, out)
		}
		unchanged(t)
	})

	t.Run("failing statement, atomic", func(t *testing.T) {
		put("bad.sql", []byte("CREATE TABLE fresh(a);\nINSERT INTO fresh VALUES (1);\nINSERT INTO missing_table VALUES (1);\n"))
		out, code := run(t, "restore", "-d", "shop", "-f", "bad.sql", "--atomic")
		if code == 0 || !strings.Contains(out, "line 3") || !strings.Contains(out, "missing_table") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		unchanged(t)
	})

	t.Run("failing statement, batch rolled back", func(t *testing.T) {
		put("bad2.sql", []byte("CREATE TABLE fresh(a);\nINSERT INTO fresh VALUES (1);\nINSERT INTO missing_table VALUES (1);\n"))
		if out, code := run(t, "restore", "-d", "shop", "-f", "bad2.sql"); code == 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		unchanged(t)
	})
}

type minio struct {
	endpoint string
	bucket   string
	client   *s3.Client
}

func startMinIO(t *testing.T) *minio {
	t.Helper()
	requireDocker(t)
	id := docker(t, "run", "-d", "-p", "127.0.0.1::9000",
		"-e", "MINIO_ROOT_USER=minioadmin", "-e", "MINIO_ROOT_PASSWORD=minioadmin",
		image("LIBSQL_BKUP_MINIO_IMAGE", "pgsty/minio:latest"), "server", "/data")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", id).Run() })
	m := &minio{endpoint: "127.0.0.1:" + publishedPort(t, id, "9000/tcp"), bucket: "backups"}
	m.client = s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String("http://" + m.endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", ""),
	})
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := m.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(m.bucket)})
		cancel()
		if err == nil {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("create bucket: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (m *minio) exists(key string) bool {
	_, err := m.client.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String(m.bucket), Key: aws.String(key)})
	return err == nil
}

// TestIntegrationMiabiContract runs Miabi's exact argv and environment
// against an authenticated sqld and MinIO.
func TestIntegrationMiabiContract(t *testing.T) {
	src, dst := startSQLD(t, true), startSQLD(t, true)
	m := startMinIO(t)
	src.exec(t, `CREATE TABLE items(id INTEGER PRIMARY KEY, name TEXT)`, `INSERT INTO items VALUES (1,'a'),(2,'b'),(3,'c')`)
	store := useServer(t, src)
	t.Setenv("DB_URL", "")
	miabi := func(s *sqld) {
		t.Setenv("DB_HOST", s.host)
		t.Setenv("DB_PORT", s.port)
		t.Setenv("DB_NAME", "appdb")
		t.Setenv("DB_URL", "http://"+s.host+":"+s.port)
		t.Setenv("LIBSQL_AUTH_TOKEN", s.token)
	}
	miabi(src)
	for k, v := range map[string]string{
		"AWS_S3_ENDPOINT": "http://" + m.endpoint, "AWS_S3_BUCKET_NAME": m.bucket, "AWS_REGION": "us-east-1",
		"AWS_ACCESS_KEY": "minioadmin", "AWS_SECRET_KEY": "minioadmin",
		"AWS_DISABLE_SSL": "true", "AWS_FORCE_PATH_STYLE": "true", "GPG_PASSPHRASE": "miabi",
	} {
		t.Setenv(k, v)
	}

	// Without the token the server refuses: connection exit code.
	t.Setenv("LIBSQL_AUTH_TOKEN", "")
	if out, code := run(t, "backup", "-d", "appdb"); code != cli.ExitConnection {
		t.Fatalf("no token: exit %d\n%s", code, out)
	}
	t.Setenv("LIBSQL_AUTH_TOKEN", src.token)

	// Local: backup -d <db>.
	out, code := run(t, "backup", "-d", "appdb")
	if code != 0 {
		t.Fatalf("local backup exit %d\n%s", code, out)
	}
	local := lastArtifact(t, out)
	if !strings.HasPrefix(local, "appdb_") || !strings.HasSuffix(local, ".sql.gz.gpg") {
		t.Fatalf("artifact %q", local)
	}
	if _, err := os.Stat(filepath.Join(store, local)); err != nil {
		t.Fatalf("last match %q is not the stored file: %v", local, err)
	}

	// S3: backup --storage s3 -d <db> --path <p>, three times with retention
	// so pruned names are printed before the new one.
	t.Setenv("BACKUP_RETENTION_COUNT", "1")
	var remote string
	for i := 0; i < 3; i++ {
		out, code = run(t, "backup", "--storage", "s3", "-d", "appdb", "--path", "miabi/appdb")
		if code != 0 {
			t.Fatalf("s3 backup exit %d\n%s", code, out)
		}
		remote = lastArtifact(t, out)
		time.Sleep(1100 * time.Millisecond)
	}
	if !m.exists("miabi/appdb/" + remote) {
		t.Fatalf("last match %q is not in the bucket\n%s", remote, out)
	}
	t.Setenv("BACKUP_RETENTION_COUNT", "")

	// Restore with Miabi's argv into the other server.
	miabi(dst)
	if out, code := run(t, "restore", "-d", "appdb", "-f", local); code != 0 {
		t.Fatalf("local restore exit %d\n%s", code, out)
	}
	if got := dst.scalar(t, "SELECT group_concat(name) FROM items"); got != "a,b,c" {
		t.Fatalf("restored %q", got)
	}
	dst.exec(t, "DELETE FROM items")
	if out, code := run(t, "restore", "--storage", "s3", "-d", "appdb", "--path", "miabi/appdb", "-f", remote); code != 0 {
		t.Fatalf("s3 restore exit %d\n%s", code, out)
	}
	if got := dst.scalar(t, "SELECT count(*) FROM items"); got != "3" {
		t.Fatalf("restored %s rows", got)
	}

	// 1.x environment names still work: AWS_S3_PATH, DB_AUTH_TOKEN, BACKUP_TMP_DIR.
	t.Setenv("LIBSQL_AUTH_TOKEN", "")
	t.Setenv("DB_AUTH_TOKEN", dst.token)
	t.Setenv("AWS_S3_PATH", "legacy")
	t.Setenv("STORAGE", "s3")
	out, code = run(t, "backup", "-d", "appdb")
	if code != 0 {
		t.Fatalf("legacy env backup exit %d\n%s", code, out)
	}
	if f := lastArtifact(t, out); !m.exists("legacy/"+f) || !strings.Contains(out, "DB_AUTH_TOKEN is deprecated") {
		t.Fatalf("legacy env: %q not in legacy/\n%s", f, out)
	}
}

// TestIntegrationUnreachable: a down server gives the connection exit code.
func TestIntegrationUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	useServer(t, &sqld{host: "127.0.0.1"})
	t.Setenv("DB_URL", "http://"+addr)
	if out, code := run(t, "backup", "-d", "x"); code != cli.ExitConnection {
		t.Fatalf("exit %d\n%s", code, out)
	}
	_ = http.ErrServerClosed
}
