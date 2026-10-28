// Package sqlscript splits an SQLite script into statements while streaming
// it, with the same notion of "complete statement" as sqlite3_complete():
//
//   - ';' ends a statement unless it is inside a '…' string, a "…", `…` or
//     […] identifier, a -- or /* */ comment, or a trigger body;
//   - a statement whose leading keywords are CREATE [TEMP|TEMPORARY] TRIGGER
//     (optionally after EXPLAIN …) is a trigger: inside it ';' only separates
//     the body's statements, and the trigger ends at the ';' that follows an
//     END keyword which itself directly follows a ';'. That is exactly the
//     grammar's "cmd ; … cmd ; END", so CASE … END expressions and columns
//     named "end" or "begin" inside the body never end it early.
//
// Only one statement is held in memory at a time.
package sqlscript

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// DefaultMaxStatement is the largest statement accepted (SQLite's own
// default SQLITE_MAX_SQL_LENGTH).
const DefaultMaxStatement = 1_000_000_000

// Statement is one complete statement.
type Statement struct {
	// Text is the statement without its terminating ';', with leading
	// whitespace and comments removed and trailing whitespace trimmed.
	Text string
	// Keywords are the first (up to three) unquoted words, upper-cased:
	// ["BEGIN","TRANSACTION"], ["PRAGMA","FOREIGN_KEYS"], ["CREATE","TABLE","T"].
	Keywords []string
	// Terminated is false for a final statement without ';' at end of input.
	Terminated bool
	// Line is the 1-based line where the statement starts.
	Line int
	// End is the input offset just after the statement (after its ';').
	End int64
}

// Keyword returns the i-th leading keyword, or "".
func (s Statement) Keyword(i int) string {
	if i < len(s.Keywords) {
		return s.Keywords[i]
	}
	return ""
}

// ErrIncomplete is returned when the input ends inside a string, identifier,
// block comment or trigger body (typically a truncated file).
var ErrIncomplete = errors.New("incomplete SQL statement at end of input")

// Splitter reads statements from a reader.
type Splitter struct {
	r    *bufio.Reader
	Max  int // maximum statement size (default DefaultMaxStatement)
	buf  []byte
	off  int64
	line int
	err  error
}

// NewSplitter returns a splitter reading from r.
func NewSplitter(r io.Reader) *Splitter {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 256<<10)
	}
	return &Splitter{r: br, line: 1}
}

// header states while reading a statement's leading tokens (sqlite3_complete).
type phase int

const (
	phStart   phase = iota // nothing significant yet
	phNormal               // an ordinary statement
	phExplain              // after EXPLAIN …
	phCreate               // after CREATE [TEMP]
	phTrigger              // inside a trigger
	phSemi                 // inside a trigger, right after ';'
	phEnd                  // inside a trigger, right after "; END"
)

type tokKind int

const (
	tkSemi tokKind = iota
	tkOther
	tkWord
)

// Next returns the next non-empty statement, or io.EOF.
func (s *Splitter) Next() (Statement, error) {
	for {
		st, empty, err := s.next()
		if err != nil {
			return st, err
		}
		if !empty {
			return st, nil
		}
	}
}

func (s *Splitter) readByte() (byte, error) {
	c, err := s.r.ReadByte()
	if err == nil {
		s.off++
		if c == '\n' {
			s.line++
		}
	}
	return c, err
}

func (s *Splitter) peek() (byte, bool) {
	b, err := s.r.Peek(1)
	if err != nil || len(b) == 0 {
		return 0, false
	}
	return b[0], true
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// isIDChar matches SQLite's IdChar: letters, digits, '_', '$' and any byte >= 0x80.
func isIDChar(c byte) bool {
	return c >= 0x80 || c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// next reads up to and including the next ';' that completes a statement.
// empty reports a statement with no significant token (";;" or comments).
func (s *Splitter) next() (st Statement, empty bool, err error) {
	if s.err != nil {
		return Statement{}, false, s.err
	}
	max := s.Max
	if max <= 0 {
		max = DefaultMaxStatement
	}
	s.buf = s.buf[:0]
	ph := phStart
	var words []string
	startLine := 0
	add := func(b ...byte) error {
		if ph == phStart {
			return nil
		}
		s.buf = append(s.buf, b...)
		if len(s.buf) > max {
			return fmt.Errorf("statement starting at line %d is larger than %d bytes", startLine, max)
		}
		return nil
	}
	// token applies sqlite3_complete's transition for one significant token.
	token := func(k tokKind, word string) {
		kw := strings.ToUpper(word)
		if ph == phStart {
			startLine = s.line
		}
		if k == tkWord && len(words) < 3 {
			words = append(words, kw)
		}
		switch ph {
		case phStart:
			switch {
			case k == tkWord && kw == "EXPLAIN":
				ph = phExplain
			case k == tkWord && kw == "CREATE":
				ph = phCreate
			default:
				ph = phNormal
			}
		case phExplain:
			switch {
			case k == tkWord && kw == "CREATE":
				ph = phCreate
			case k == tkWord && kw == "EXPLAIN":
				ph = phNormal
			case k == tkWord && (kw == "TEMP" || kw == "TEMPORARY" || kw == "TRIGGER" || kw == "END"):
				ph = phNormal
			}
		case phCreate:
			switch {
			case k == tkWord && (kw == "TEMP" || kw == "TEMPORARY"):
			case k == tkWord && kw == "TRIGGER":
				ph = phTrigger
			default:
				ph = phNormal
			}
		case phTrigger:
		case phSemi:
			if k == tkWord && kw == "END" {
				ph = phEnd
			} else {
				ph = phTrigger
			}
		case phEnd:
			ph = phTrigger
		}
	}
	finish := func(terminated bool) Statement {
		return Statement{
			Text:       strings.TrimRight(string(s.buf), " \t\r\n\f\v"),
			Keywords:   words,
			Terminated: terminated,
			Line:       startLine,
			End:        s.off,
		}
	}
	for {
		c, rerr := s.readByte()
		if rerr == io.EOF {
			switch ph {
			case phStart:
				s.err = io.EOF
				return Statement{}, false, io.EOF
			case phTrigger, phSemi, phEnd:
				s.err = fmt.Errorf("%w: CREATE TRIGGER starting at line %d has no END", ErrIncomplete, startLine)
				return Statement{}, false, s.err
			}
			s.err = io.EOF
			return finish(false), false, nil
		}
		if rerr != nil {
			s.err = rerr
			return Statement{}, false, rerr
		}
		switch {
		case isSpace(c):
			if err := add(c); err != nil {
				return Statement{}, false, s.fail(err)
			}
		case c == '-' && s.peekIs('-'):
			if err := s.lineComment(add); err != nil {
				return Statement{}, false, s.fail(err)
			}
		case c == '/' && s.peekIs('*'):
			if err := s.blockComment(add, startLine); err != nil {
				return Statement{}, false, s.fail(err)
			}
		case c == ';':
			switch ph {
			case phStart:
				return Statement{End: s.off}, true, nil
			case phTrigger, phSemi:
				ph = phSemi
				if err := add(c); err != nil {
					return Statement{}, false, s.fail(err)
				}
				continue
			}
			// phNormal, phExplain, phCreate, phEnd: the statement is complete.
			return finish(true), false, nil
		case c == '\'' || c == '"' || c == '`' || c == '[':
			token(tkOther, "")
			if err := add(c); err != nil {
				return Statement{}, false, s.fail(err)
			}
			if err := s.quoted(c, add, startLine); err != nil {
				return Statement{}, false, s.fail(err)
			}
		case isIDChar(c):
			word := []byte{c}
			for {
				p, ok := s.peek()
				if !ok || !isIDChar(p) {
					break
				}
				_, _ = s.readByte()
				word = append(word, p)
			}
			token(tkWord, string(word))
			if err := add(word...); err != nil {
				return Statement{}, false, s.fail(err)
			}
		default:
			token(tkOther, "")
			if err := add(c); err != nil {
				return Statement{}, false, s.fail(err)
			}
		}
	}
}

func (s *Splitter) fail(err error) error {
	s.err = err
	return err
}

func (s *Splitter) peekIs(b byte) bool {
	p, ok := s.peek()
	return ok && p == b
}

// lineComment consumes "-…" up to and including the newline.
func (s *Splitter) lineComment(add func(...byte) error) error {
	if err := add('-'); err != nil {
		return err
	}
	for {
		c, err := s.readByte()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := add(c); err != nil {
			return err
		}
		if c == '\n' {
			return nil
		}
	}
}

// blockComment consumes "*…*/" after the opening '/'.
func (s *Splitter) blockComment(add func(...byte) error, line int) error {
	if err := add('/'); err != nil {
		return err
	}
	c, err := s.readByte() // '*'
	if err != nil {
		return err
	}
	if err := add(c); err != nil {
		return err
	}
	prevStar := false
	for {
		c, err := s.readByte()
		if err == io.EOF {
			return fmt.Errorf("%w: unterminated /* comment (statement at line %d)", ErrIncomplete, line)
		}
		if err != nil {
			return err
		}
		if err := add(c); err != nil {
			return err
		}
		if prevStar && c == '/' {
			return nil
		}
		prevStar = c == '*'
	}
}

// quoted consumes a quoted string or identifier after its opening quote.
// A doubled closing quote (” "" “) simply starts a new quoted run, which
// gives the same result as treating it as an escape.
func (s *Splitter) quoted(open byte, add func(...byte) error, line int) error {
	closeCh := open
	if open == '[' {
		closeCh = ']'
	}
	for {
		c, err := s.readByte()
		if err == io.EOF {
			return fmt.Errorf("%w: unterminated %c…%c starting at line %d", ErrIncomplete, open, closeCh, line)
		}
		if err != nil {
			return err
		}
		if err := add(c); err != nil {
			return err
		}
		if c == closeCh {
			return nil
		}
	}
}
