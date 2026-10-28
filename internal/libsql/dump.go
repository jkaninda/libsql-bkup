package libsql

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/jkaninda/libsql-bkup/internal/hrana"
	"github.com/jkaninda/libsql-bkup/internal/sqlscript"
)

// Page sizing for table reads. Hrana over HTTP returns a whole result set
// in one response, so tables are read in pages; the page row count adapts
// to keep each response near pageBytes.
const (
	pageBytes    = 4 << 20
	pageMinRows  = 1
	pageMaxRows  = 5000
	pageInitRows = 256
)

type schemaObject struct {
	typ, name, tblName, sql string
}

type tableInfo struct {
	obj     schemaObject
	virtual bool
	cols    []string // dumped columns (not hidden, not generated)
	keyCols []int    // WITHOUT ROWID: positions of the primary key in cols
	rowid   string   // rowid alias to page on ("" = none usable)
	ipk     bool     // has an INTEGER PRIMARY KEY (the rowid is already a column)
}

// dumper writes an SQL dump equivalent to `sqlite3 .dump`, read inside one
// transaction so the dump is a consistent snapshot.
type dumper struct {
	s   *hrana.Stream
	w   *bufio.Writer
	err error
	log *slog.Logger
	// stats
	tables, rows int64
}

func (d *dumper) printf(format string, a ...any) {
	if d.err == nil {
		_, d.err = fmt.Fprintf(d.w, format, a...)
	}
}

func (d *dumper) write(s string) {
	if d.err == nil {
		_, d.err = d.w.WriteString(s)
	}
}

// dump streams the database behind s to w.
//
// Output, in the order of the sqlite3 shell's .dump:
//
//	PRAGMA foreign_keys=OFF;
//	BEGIN TRANSACTION;
//	DROP TABLE IF EXISTS "t"; CREATE TABLE t(…); INSERT INTO "t"(…) VALUES(…); …  (every table)
//	DELETE FROM sqlite_sequence WHERE name='t'; INSERT INTO sqlite_sequence …      (AUTOINCREMENT state)
//	views, then triggers, then indexes (each preceded by DROP … IF EXISTS)
//	COMMIT;
//
// The DROP … IF EXISTS statements make a restore replace existing objects,
// as libsql-bkup 1.x dumps did.
func dump(ctx context.Context, s *hrana.Stream, w io.Writer, log *slog.Logger) (err error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d := &dumper{s: s, w: bufio.NewWriterSize(w, 256<<10), log: log}
	// A deferred transaction: the first read takes a snapshot that every
	// later read on this stream shares (sqld runs SQLite in WAL mode, so
	// writers are not blocked). Read-only transactions cannot be requested
	// over Hrana; nothing below writes.
	if err := s.Exec(ctx, "BEGIN"); err != nil {
		return fmt.Errorf("begin read transaction: %w", err)
	}
	defer func() {
		if rerr := s.Exec(context.WithoutCancel(ctx), "ROLLBACK"); rerr != nil && err == nil && !errors.Is(rerr, hrana.ErrStreamClosed) {
			err = fmt.Errorf("end read transaction: %w", rerr)
		}
	}()

	objs, err := d.schema(ctx)
	if err != nil {
		return err
	}
	kinds, err := d.tableKinds(ctx)
	if err != nil {
		return err
	}

	skip := map[string]bool{}
	var tables []schemaObject
	hasSequence := false
	for _, o := range objs {
		if o.typ != "table" {
			continue
		}
		switch {
		case o.name == "sqlite_sequence":
			hasSequence = true
			continue
		case isInternal(o.name), kinds[o.name].typ == "shadow":
			skip[o.name] = true
			continue
		}
		tables = append(tables, o)
	}

	d.write("PRAGMA foreign_keys=OFF;\nBEGIN TRANSACTION;\n")
	dumped := map[string]bool{}
	for _, o := range tables {
		k, known := kinds[o.name]
		if !known {
			k = tableKind{typ: "table", wr: withoutRowid(o.sql)}
			if isVirtualSQL(o.sql) {
				k.typ = "virtual"
			}
		}
		if err := d.table(ctx, o, k); err != nil {
			return err
		}
		dumped[o.name] = true
	}
	if hasSequence {
		if err := d.sequences(ctx, dumped); err != nil {
			return err
		}
	}
	for _, typ := range []string{"view", "trigger", "index"} {
		for _, o := range objs {
			if o.typ != typ || skip[o.tblName] || isInternal(o.name) {
				continue
			}
			d.printf("DROP %s IF EXISTS %s;\n", strings.ToUpper(typ), quoteIdent(o.name))
			d.write(terminate(o.sql))
		}
	}
	d.write("COMMIT;\n")
	if d.err != nil {
		return d.err
	}
	if err := d.w.Flush(); err != nil {
		return err
	}
	log.Info("Dump written", "tables", d.tables, "rows", d.rows)
	return nil
}

// isInternal reports tables a libSQL server or tool keeps for itself.
func isInternal(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "sqlite_") || strings.HasPrefix(n, "libsql_") || strings.HasPrefix(n, "_litestream_")
}

// virtualModule returns the lower-case module name of CREATE VIRTUAL TABLE … USING module(…).
func virtualModule(sql string) string {
	f := strings.Fields(strings.NewReplacer("(", " ( ").Replace(sql))
	for i := 0; i+1 < len(f); i++ {
		if strings.EqualFold(f[i], "USING") {
			return strings.ToLower(f[i+1])
		}
	}
	return ""
}

func isVirtualSQL(sql string) bool {
	f := strings.Fields(strings.ToUpper(sql))
	return len(f) >= 2 && f[0] == "CREATE" && f[1] == "VIRTUAL"
}

// terminate returns sql followed by ";\n". When the text ends inside a
// "--" comment, the ';' goes on its own line.
func terminate(sql string) string {
	sql = strings.TrimRight(sql, " \t\r\n;")
	sp := sqlscript.NewSplitter(strings.NewReader(sql + ";"))
	st, err := sp.Next()
	if err == nil && st.Terminated && st.End == int64(len(sql)+1) {
		if _, err := sp.Next(); err == io.EOF {
			return sql + ";\n"
		}
	}
	return sql + "\n;\n"
}

func (d *dumper) schema(ctx context.Context) ([]schemaObject, error) {
	// '_' is a LIKE wildcard: escape it, or "sqliteX" tables would be skipped too.
	res, err := d.s.Query(ctx, `SELECT type, name, tbl_name, sql FROM sqlite_schema
		WHERE sql IS NOT NULL AND (name NOT LIKE 'sqlite\_%' ESCAPE '\' OR name = 'sqlite_sequence')
		ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	out := make([]schemaObject, 0, len(res.Rows))
	for _, r := range res.Rows {
		if len(r) != 4 {
			return nil, errors.New("read schema: unexpected column count")
		}
		out = append(out, schemaObject{typ: r[0].Str(), name: r[1].Str(), tblName: r[2].Str(), sql: r[3].Str()})
	}
	return out, nil
}

// tableKind is a row of PRAGMA table_list.
type tableKind struct {
	typ string // "table", "view", "virtual" or "shadow"
	wr  bool   // WITHOUT ROWID
}

// tableKinds reads PRAGMA table_list (SQLite 3.37+). Older servers give an
// empty map, and the table definitions are inspected instead.
func (d *dumper) tableKinds(ctx context.Context) (map[string]tableKind, error) {
	out := map[string]tableKind{}
	res, err := d.s.Query(ctx, "SELECT name, type, wr FROM pragma_table_list WHERE schema = 'main'")
	if err != nil {
		var he *hrana.Error
		if errors.As(err, &he) {
			d.log.Debug("PRAGMA table_list is not available; shadow tables cannot be detected", "error", err)
			return out, nil
		}
		return nil, fmt.Errorf("list tables: %w", err)
	}
	for _, r := range res.Rows {
		out[r[0].Str()] = tableKind{typ: r[1].Str(), wr: r[2].Str() == "1"}
	}
	return out, nil
}

func (d *dumper) columns(ctx context.Context, o schemaObject, k tableKind) (*tableInfo, error) {
	virtual := k.typ == "virtual"
	res, err := d.s.Query(ctx, "SELECT name, type, pk, hidden FROM pragma_table_xinfo(?)", hrana.Text(o.name))
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", o.name, err)
	}
	ti := &tableInfo{obj: o, virtual: virtual}
	all := map[string]bool{}
	type pkCol struct{ pos, idx int }
	var pks []pkCol
	var pkTypes []string
	for _, r := range res.Rows {
		name, typ, pk, hidden := r[0].Str(), r[1].Str(), r[2].Str(), r[3].Str()
		all[strings.ToLower(name)] = true
		if hidden != "0" {
			// 1: hidden column of a virtual table; 2, 3: generated column.
			continue
		}
		if pk != "0" {
			var n int
			_, _ = fmt.Sscan(pk, &n)
			pks = append(pks, pkCol{pos: n, idx: len(ti.cols)})
			pkTypes = append(pkTypes, typ)
		}
		ti.cols = append(ti.cols, name)
	}
	if len(ti.cols) == 0 {
		return nil, fmt.Errorf("table %s has no columns to dump", o.name)
	}
	if virtual {
		// Full-text tables keep their rowids (external-content tables and
		// applications refer to them); other modules are paged by offset.
		switch virtualModule(o.sql) {
		case "fts3", "fts4", "fts5":
			ti.rowid = "rowid"
		}
		return ti, nil
	}
	if k.wr {
		for i := 1; i <= len(pks); i++ {
			for _, p := range pks {
				if p.pos == i {
					ti.keyCols = append(ti.keyCols, p.idx)
				}
			}
		}
		return ti, nil
	}
	ti.ipk = len(pks) == 1 && strings.EqualFold(strings.TrimSpace(pkTypes[0]), "INTEGER")
	for _, alias := range []string{"rowid", "_rowid_", "oid"} {
		if !all[alias] {
			ti.rowid = alias
			break
		}
	}
	return ti, nil
}

// withoutRowid reports a "… ) WITHOUT ROWID" table definition (fallback
// for servers without PRAGMA table_list).
func withoutRowid(sql string) bool {
	f := strings.Fields(strings.ToUpper(strings.NewReplacer(",", " , ", ")", " ) ").Replace(sql)))
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "WITHOUT" && f[i+1] == "ROWID" {
			return true
		}
	}
	return false
}

func (d *dumper) table(ctx context.Context, o schemaObject, k tableKind) error {
	ti, err := d.columns(ctx, o, k)
	if err != nil {
		return err
	}
	d.tables++
	d.printf("DROP TABLE IF EXISTS %s;\n", quoteIdent(o.name))
	d.write(terminate(o.sql))

	quoted := make([]string, len(ti.cols))
	for i, c := range ti.cols {
		quoted[i] = quoteIdent(c)
	}
	colList := strings.Join(quoted, ",")
	insertCols := colList
	keepRowid := ti.rowid != "" && !ti.ipk
	if keepRowid {
		// Keep rowids: they are not renumbered by a restore, so anything
		// that refers to them (FTS external content, application ids)
		// stays valid.
		insertCols = ti.rowid + "," + colList
	}
	prefix := "INSERT INTO " + quoteIdent(o.name) + "(" + insertCols + ") VALUES("

	page := pageInitRows
	var (
		lastKey []hrana.Value
		offset  int64
	)
	for {
		var (
			q    string
			args []hrana.Value
		)
		switch {
		case ti.rowid != "":
			q = "SELECT " + ti.rowid + "," + colList + " FROM " + quoteIdent(o.name)
			if lastKey != nil {
				q += " WHERE " + ti.rowid + " > ?"
				args = lastKey
			}
			q += fmt.Sprintf(" ORDER BY %s LIMIT %d", ti.rowid, page)
		case len(ti.keyCols) > 0:
			keys := make([]string, len(ti.keyCols))
			for i, k := range ti.keyCols {
				keys[i] = quoted[k]
			}
			q = "SELECT " + colList + " FROM " + quoteIdent(o.name)
			if lastKey != nil {
				q += " WHERE (" + strings.Join(keys, ",") + ") > (" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ")"
				args = lastKey
			}
			q += fmt.Sprintf(" ORDER BY %s LIMIT %d", strings.Join(keys, ","), page)
		default:
			q = fmt.Sprintf("SELECT %s FROM %s LIMIT %d OFFSET %d", colList, quoteIdent(o.name), page, offset)
		}
		res, err := d.s.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("read %s: %w", o.name, err)
		}
		size := 0
		for ri, r := range res.Rows {
			vals := r
			if ti.rowid != "" && !keepRowid {
				vals = r[1:]
			}
			d.write(prefix)
			for i, v := range vals {
				if i > 0 {
					d.write(",")
				}
				lit, ok, err := literal(v)
				if err != nil {
					return fmt.Errorf("%s: %w", o.name, err)
				}
				if !ok {
					if lit, err = d.infinity(ctx, ti, r, offset+int64(ri), i, keepRowid); err != nil {
						return err
					}
				}
				d.write(lit)
				size += valueSize(v)
			}
			d.write(");\n")
		}
		if d.err != nil {
			return d.err
		}
		d.rows += int64(len(res.Rows))
		if len(res.Rows) < page {
			return nil
		}
		last := res.Rows[len(res.Rows)-1]
		switch {
		case ti.rowid != "":
			lastKey = []hrana.Value{last[0]}
		case len(ti.keyCols) > 0:
			lastKey = lastKey[:0]
			for _, k := range ti.keyCols {
				if last[k].IsNullFloat() {
					return fmt.Errorf("%s: cannot page on an infinite primary key value", o.name)
				}
				lastKey = append(lastKey, last[k])
			}
		default:
			offset += int64(len(res.Rows))
		}
		page = nextPage(page, len(res.Rows), size)
	}
}

// nextPage scales the row count so the next response is near pageBytes.
func nextPage(page, rows, size int) int {
	if rows == 0 || size == 0 {
		return page
	}
	n := int(int64(rows) * pageBytes / int64(size))
	if n > page*4 {
		n = page * 4
	}
	return min(max(n, pageMinRows), pageMaxRows)
}

// infinity asks the server for the sign of a REAL the JSON encoding could
// not carry (sqld sends ±Inf as null) and returns 9e999 or -9e999.
func (d *dumper) infinity(ctx context.Context, ti *tableInfo, row []hrana.Value, rowIndex int64, col int, keepRowid bool) (string, error) {
	c := col
	if keepRowid {
		c-- // vals include the rowid first
	}
	if c < 0 {
		return "", fmt.Errorf("%s: unexpected non-integer rowid", ti.obj.name)
	}
	expr := quoteIdent(ti.cols[c])
	var (
		q    string
		args []hrana.Value
	)
	switch {
	case ti.rowid != "":
		q = fmt.Sprintf("SELECT %s > 0 FROM %s WHERE %s = ?", expr, quoteIdent(ti.obj.name), ti.rowid)
		args = []hrana.Value{row[0]}
	case len(ti.keyCols) > 0:
		keys := make([]string, len(ti.keyCols))
		for i, k := range ti.keyCols {
			keys[i] = quoteIdent(ti.cols[k])
			args = append(args, row[k])
		}
		q = fmt.Sprintf("SELECT %s > 0 FROM %s WHERE (%s) = (%s)", expr, quoteIdent(ti.obj.name),
			strings.Join(keys, ","), strings.TrimSuffix(strings.Repeat("?,", len(keys)), ","))
	default:
		q = fmt.Sprintf("SELECT %s > 0 FROM %s LIMIT 1 OFFSET %d", expr, quoteIdent(ti.obj.name), rowIndex)
	}
	res, err := d.s.Query(ctx, q, args...)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ti.obj.name, err)
	}
	if len(res.Rows) != 1 {
		return "", fmt.Errorf("%s: row changed while reading an infinite value", ti.obj.name)
	}
	switch res.Rows[0][0].Str() {
	case "1":
		return "9e999", nil
	case "0":
		return "-9e999", nil
	}
	return "NULL", nil
}

// sequences writes the AUTOINCREMENT counters of the dumped tables.
func (d *dumper) sequences(ctx context.Context, dumped map[string]bool) error {
	res, err := d.s.Query(ctx, "SELECT name, seq FROM sqlite_sequence")
	if err != nil {
		return fmt.Errorf("read sqlite_sequence: %w", err)
	}
	for _, r := range res.Rows {
		name := r[0].Str()
		if !dumped[name] {
			continue
		}
		seq, ok, err := literal(r[1])
		if err != nil || !ok {
			return fmt.Errorf("sqlite_sequence %s: invalid value", name)
		}
		d.printf("DELETE FROM sqlite_sequence WHERE name = %s;\n", quoteText(name))
		d.printf("INSERT INTO sqlite_sequence(name,seq) VALUES(%s,%s);\n", quoteText(name), seq)
	}
	return nil
}
