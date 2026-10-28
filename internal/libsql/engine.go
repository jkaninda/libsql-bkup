// Package libsql is the libsql-bkup engine: a logical SQL dump of a libSQL
// server (sqld, libsql-server, Turso) read over Hrana-over-HTTP, and a
// streaming, batched replay of such a dump.
package libsql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jkaninda/bkupkit/config"
	"github.com/jkaninda/bkupkit/engine"
	"github.com/jkaninda/libsql-bkup/internal/hrana"
)

// Settings specific to libsql-bkup.
const (
	AuthToken        = "LIBSQL_AUTH_TOKEN"
	RestoreBatchSize = "RESTORE_BATCH_SIZE"
	RestoreAtomic    = "RESTORE_ATOMIC"
)

// Engine implements engine.Engine for libSQL.
type Engine struct {
	// HTTP is the client used for the server (default: a dedicated client
	// with the default transport and no overall timeout).
	HTTP *http.Client
}

var _ engine.Engine = (*Engine)(nil)

// New returns the engine.
func New() *Engine { return &Engine{} }

func (e *Engine) Tool() string { return "libsql-bkup" }
func (e *Engine) Kind() string { return "libsql" }

// Settings registers the connection settings. Settings of the shared
// database set that mean nothing for libSQL (users, passwords, TLS files,
// multi-database modes, migrate targets) are removed.
func (e *Engine) Settings(r *config.Registry) {
	config.RegisterDatabase(r, "8080")
	for _, k := range []string{
		config.DBUsername, config.DBPassword, config.DBSSLMode, config.DBSSLCA, config.DBSSLCert, config.DBSSLKey,
		config.AllDatabases, config.AllInOne, config.ExcludeDBs,
		config.TargetDBURL, config.TargetDBHost, config.TargetDBPort, config.TargetDBName, config.TargetDBUser,
		config.TargetDBPass, config.TargetDBSSL, config.TargetDBSSLCA,
	} {
		r.Remove(k)
	}
	r.Override(config.DBURL, func(s *config.Setting) {
		s.Usage = "libSQL server URL: http(s)://, libsql:// or ws(s):// (default http://DB_HOST:DB_PORT). A namespace of a multi-tenant sqld is addressed by its subdomain"
	})
	r.Override(config.DBHost, func(s *config.Setting) { s.Usage = "libSQL server host (when DB_URL is not set)" })
	r.Override(config.DBPort, func(s *config.Setting) { s.Usage = "libSQL server HTTP port (when DB_URL is not set)" })
	r.Override(config.DBName, func(s *config.Setting) {
		s.Usage = "Database name, used to name artifacts (default: first label of the server host, or \"libsql\")"
	})
	r.Add(
		config.Setting{Key: AuthToken, Aliases: []string{"DB_AUTH_TOKEN"}, Secret: true, Group: config.GroupDatabase,
			Usage: "JWT for the libSQL server (Authorization: Bearer)"},
		config.Setting{Key: RestoreBatchSize, Kind: config.Int, Default: "500", Flag: "batch-size", Commands: []string{"restore"}, Group: config.GroupDatabase,
			Usage: "Statements per transaction and per request during restore"},
		config.Setting{Key: RestoreAtomic, Kind: config.Bool, Flag: "atomic", Commands: []string{"restore"}, Group: config.GroupDatabase,
			Usage: "Restore in a single transaction (all or nothing; long transactions may time out on large databases)"},
	)
}

// Configure validates the connection settings.
func (e *Engine) Configure(_ context.Context, v *config.Values) error {
	if _, err := resolveEndpoint(v); err != nil {
		return err
	}
	if v.Has(RestoreBatchSize) && v.Int(RestoreBatchSize) < 1 {
		return fmt.Errorf("%s must be at least 1", RestoreBatchSize)
	}
	return nil
}

// Targets returns the single database of a run.
func (e *Engine) Targets(_ context.Context, v *config.Values) ([]engine.Target, error) {
	ep, err := resolveEndpoint(v)
	if err != nil {
		return nil, err
	}
	return []engine.Target{{Name: targetName(v, ep), Values: v}}, nil
}

func (e *Engine) Ext(engine.Target) string             { return ".sql" }
func (e *Engine) NativeCompression(engine.Target) bool { return false }

func (e *Engine) client(t engine.Target) (*hrana.Client, error) {
	ep, err := resolveEndpoint(t.Values)
	if err != nil {
		return nil, err
	}
	hc := e.HTTP
	if hc == nil {
		hc = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	}
	return &hrana.Client{URL: ep.URL, Token: ep.Token, HTTP: hc}, nil
}

// connError adds a hint to authentication failures.
func connError(err error) error {
	var he *hrana.HTTPError
	if errors.As(err, &he) && he.IsAuth() {
		return fmt.Errorf("%w (check LIBSQL_AUTH_TOKEN)", err)
	}
	return err
}

// Ping runs SELECT 1.
func (e *Engine) Ping(ctx context.Context, t engine.Target) error {
	c, err := e.client(t)
	if err != nil {
		return err
	}
	s := c.OpenStream()
	defer closeStream(s)
	if _, err := s.Query(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("connect to libSQL at %s: %w", c.URL, connError(err))
	}
	return nil
}

// ServerVersion returns the SQLite version of the server.
func (e *Engine) ServerVersion(ctx context.Context, t engine.Target) string {
	c, err := e.client(t)
	if err != nil {
		return ""
	}
	s := c.OpenStream()
	defer closeStream(s)
	res, err := s.Query(ctx, "SELECT sqlite_version()")
	if err != nil || len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		return ""
	}
	return "sqlite " + res.Rows[0][0].Str()
}

func closeStream(s *hrana.Stream) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Close(ctx)
}

// Dump writes an SQL dump of the database to w.
func (e *Engine) Dump(ctx context.Context, t engine.Target, w io.Writer) error {
	c, err := e.client(t)
	if err != nil {
		return err
	}
	s := c.OpenStream()
	defer closeStream(s)
	if err := dump(ctx, s, w, slog.Default()); err != nil {
		return connError(err)
	}
	return nil
}

// Restore replays the dump read from r.
func (e *Engine) Restore(ctx context.Context, t engine.Target, r io.Reader, info engine.ArtifactInfo) error {
	c, err := e.client(t)
	if err != nil {
		return err
	}
	o := restoreOptions{BatchSize: t.Values.Int(RestoreBatchSize), Atomic: t.Values.Bool(RestoreAtomic), Log: slog.Default()}
	s := c.OpenStream()
	defer closeStream(s)
	slog.Default().Info("Replaying dump", "file", info.File, "batch_size", o.BatchSize, "atomic", o.Atomic)
	st, err := replay(ctx, s, r, o)
	if err != nil {
		if !o.Atomic && st.Batches > 0 {
			return fmt.Errorf("%w (%d statements in %d batches were already committed; use --atomic for all-or-nothing restores)", connError(err), st.Statements, st.Batches)
		}
		return connError(err)
	}
	slog.Default().Info("Dump replayed", "statements", st.Statements, "batches", st.Batches, "skipped", st.Skipped)
	return nil
}
