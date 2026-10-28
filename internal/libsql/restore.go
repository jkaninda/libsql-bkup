package libsql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/jkaninda/libsql-bkup/internal/hrana"
	"github.com/jkaninda/libsql-bkup/internal/sqlscript"
)

// maxBatchBytes bounds the SQL text sent in one request, whatever the
// statement count.
const maxBatchBytes = 8 << 20

type restoreOptions struct {
	BatchSize int  // statements per transaction (non-atomic mode) and per request
	Atomic    bool // one transaction for the whole restore
	Log       *slog.Logger
}

type restoreStats struct {
	Statements, Skipped, Batches int64
}

// classify decides what to do with a statement of the dump.
//
// The dump's own transaction control (BEGIN, COMMIT, END) is dropped: the
// replayer manages transactions itself. PRAGMAs are dropped too, as in
// libsql-bkup 1.x (foreign_keys is set by the replayer, and most PRAGMAs
// cannot run inside a transaction). A bare ROLLBACK is what the sqlite3
// shell writes when a dump failed half-way, so it is an error.
func classify(st sqlscript.Statement) (skip bool, err error) {
	switch st.Keyword(0) {
	case "BEGIN", "COMMIT", "END", "PRAGMA":
		return true, nil
	case "ROLLBACK":
		if st.Keyword(1) == "TO" || st.Keyword(2) == "TO" {
			return false, nil
		}
		return false, fmt.Errorf("line %d: the dump contains ROLLBACK; it was written with errors and is incomplete", st.Line)
	}
	return false, nil
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// replay streams statements from r into the stream s.
//
// Non-atomic (default): every batch of BatchSize statements is one request
// that runs BEGIN; statements…; COMMIT, with each step conditional on the
// previous one and a ROLLBACK step if anything failed. A batch is only sent
// once it has been read completely, so a read error (decryption, checksum,
// truncated file) never commits a partial batch; batches already committed
// stay.
//
// Atomic: one BEGIN, the batches, one COMMIT. Any error rolls everything back.
func replay(ctx context.Context, s *hrana.Stream, r io.Reader, o restoreOptions) (stats restoreStats, err error) {
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 500
	}
	// Rows are loaded table by table, so a child row may come before its
	// parent. foreign_keys only changes outside a transaction.
	if err := s.Exec(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return stats, fmt.Errorf("disable foreign keys: %w", err)
	}
	inTx := false
	if o.Atomic {
		if err := s.Exec(ctx, "BEGIN"); err != nil {
			return stats, fmt.Errorf("begin transaction: %w", err)
		}
		inTx = true
	}
	defer func() {
		if err != nil && inTx {
			if rerr := s.Exec(context.WithoutCancel(ctx), "ROLLBACK"); rerr != nil && !errors.Is(rerr, hrana.ErrStreamClosed) {
				log.Warn("Rollback failed; the server discards the transaction when the stream closes", "error", rerr)
			}
		}
	}()

	sp := sqlscript.NewSplitter(r)
	batch := make([]sqlscript.Statement, 0, o.BatchSize)
	for done := false; !done; {
		batch = batch[:0]
		size := 0
		for len(batch) < o.BatchSize && size < maxBatchBytes {
			st, err := sp.Next()
			if err == io.EOF {
				done = true
				break
			}
			if err != nil {
				return stats, fmt.Errorf("read dump: %w", err)
			}
			skip, err := classify(st)
			if err != nil {
				return stats, err
			}
			if skip {
				stats.Skipped++
				log.Debug("Skipping statement", "line", st.Line, "statement", truncate(st.Text, 80))
				continue
			}
			batch = append(batch, st)
			size += len(st.Text)
		}
		if len(batch) == 0 {
			continue
		}
		if err := sendBatch(ctx, s, batch, !o.Atomic); err != nil {
			return stats, err
		}
		stats.Statements += int64(len(batch))
		stats.Batches++
		if stats.Batches%100 == 0 {
			log.Info("Restore progress", "statements", stats.Statements)
		}
	}
	if o.Atomic {
		if err := s.Exec(ctx, "COMMIT"); err != nil {
			return stats, fmt.Errorf("commit: %w", err)
		}
		inTx = false
	}
	return stats, nil
}

// sendBatch runs the statements in one request. With ownTx, the batch is
// wrapped in BEGIN … COMMIT (ROLLBACK on failure).
func sendBatch(ctx context.Context, s *hrana.Stream, batch []sqlscript.Statement, ownTx bool) error {
	steps := make([]hrana.Step, 0, len(batch)+3)
	add := func(sql string) {
		st := hrana.Step{Stmt: hrana.Stmt{SQL: sql}}
		if n := len(steps); n > 0 {
			st.Condition = hrana.OK(n - 1)
		}
		steps = append(steps, st)
	}
	first := 0
	if ownTx {
		add("BEGIN")
		first = 1
	}
	for _, st := range batch {
		add(st.Text)
	}
	commit := -1
	if ownTx {
		add("COMMIT")
		commit = len(steps) - 1
		steps = append(steps, hrana.Step{Stmt: hrana.Stmt{SQL: "ROLLBACK"}, Condition: hrana.Not(hrana.OK(commit))})
	}
	res, err := s.Batch(ctx, steps)
	if err != nil {
		return fmt.Errorf("restore batch starting at line %d: %w", batch[0].Line, err)
	}
	if i, e := res.FirstError(); i >= 0 {
		switch {
		case i < first:
			return fmt.Errorf("begin transaction: %w", e)
		case i-first < len(batch):
			st := batch[i-first]
			return fmt.Errorf("statement at line %d failed: %w: %s", st.Line, e, truncate(st.Text, 200))
		default:
			return fmt.Errorf("commit: %w", e)
		}
	}
	if len(res.StepResults) != len(steps) {
		return fmt.Errorf("restore batch starting at line %d: %d results for %d steps", batch[0].Line, len(res.StepResults), len(steps))
	}
	if commit >= 0 && res.StepResults[commit] == nil {
		return fmt.Errorf("restore batch starting at line %d was not committed", batch[0].Line)
	}
	last := len(batch) - 1 + first
	if res.StepResults[last] == nil {
		return fmt.Errorf("statement at line %d was not executed", batch[len(batch)-1].Line)
	}
	return nil
}
