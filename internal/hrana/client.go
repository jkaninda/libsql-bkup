package hrana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var ErrStreamClosed = errors.New("hrana: stream closed by the server")

// Client talks to one libSQL database URL (http:// or https://).
type Client struct {
	URL         string // base URL, e.g. http://localhost:8080
	Token       string // JWT sent as "Authorization: Bearer"
	HTTP        *http.Client
	MaxResponse int64
}

// Stream is one server-side connection. A transaction opened on a stream
// stays open across requests until committed, rolled back or the stream
// is closed. A Stream is not safe for concurrent use.
type Stream struct {
	c       *Client
	baton   string
	baseURL string
	closed  bool
	started bool
}

// OpenStream returns a new stream. Nothing is sent until the first request.
func (c *Client) OpenStream() *Stream { return &Stream{c: c} }

// Value is a Hrana value. Integers and text keep their exact wire form.
type Value struct {
	Type   string          `json:"type"`
	Value  json.RawMessage `json:"value,omitempty"`
	Base64 string          `json:"base64,omitempty"`
}

// Value types.
const (
	TypeNull    = "null"
	TypeInteger = "integer"
	TypeFloat   = "float"
	TypeText    = "text"
	TypeBlob    = "blob"
)

// Text returns a text value.
func Text(s string) Value {
	b, _ := json.Marshal(s)
	return Value{Type: TypeText, Value: b}
}

// Integer returns an integer value from its decimal form.
func Integer(dec string) Value {
	b, _ := json.Marshal(dec)
	return Value{Type: TypeInteger, Value: b}
}

// Str returns the string content of a text or integer value.
func (v Value) Str() string {
	var s string
	if len(v.Value) > 0 && v.Value[0] == '"' {
		_ = json.Unmarshal(v.Value, &s)
		return s
	}
	return string(v.Value)
}

// IsNullFloat reports a float whose value the server could not encode in
// JSON (sqld sends +Inf and -Inf as null).
func (v Value) IsNullFloat() bool {
	return v.Type == TypeFloat && (len(v.Value) == 0 || string(v.Value) == "null")
}

// Stmt is a statement with positional arguments.
type Stmt struct {
	SQL      string  `json:"sql"`
	Args     []Value `json:"args,omitempty"`
	WantRows bool    `json:"want_rows"`
}

// Col is a result column.
type Col struct {
	Name     *string `json:"name"`
	Decltype *string `json:"decltype"`
}

// StmtResult is the result of one statement.
type StmtResult struct {
	Cols             []Col     `json:"cols"`
	Rows             [][]Value `json:"rows"`
	AffectedRowCount int64     `json:"affected_row_count"`
}

// Condition decides whether a batch step runs.
type Condition struct {
	Type string     `json:"type"` // "ok", "error", "not", "and", "or", "is_autocommit"
	Step *int       `json:"step,omitempty"`
	Cond *Condition `json:"cond,omitempty"`
}

// OK is a condition that holds when step i succeeded.
func OK(i int) *Condition { return &Condition{Type: "ok", Step: &i} }

// Not negates a condition.
func Not(c *Condition) *Condition { return &Condition{Type: "not", Cond: c} }

// Step is a batch step.
type Step struct {
	Condition *Condition `json:"condition,omitempty"`
	Stmt      Stmt       `json:"stmt"`
}

// Error is an error reported by the server for a statement or request.
type Error struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

func (e *Error) Error() string {
	if e.Code != "" {
		return e.Message + " (" + e.Code + ")"
	}
	return e.Message
}

// BatchResult holds one entry per step: a result, an error, or neither
// (the step was skipped because its condition did not hold).
type BatchResult struct {
	StepResults []*StmtResult `json:"step_results"`
	StepErrors  []*Error      `json:"step_errors"`
}

// FirstError returns the index and error of the first failed step, or -1.
func (b *BatchResult) FirstError() (int, *Error) {
	for i, e := range b.StepErrors {
		if e != nil {
			return i, e
		}
	}
	return -1, nil
}

type request struct {
	Type  string `json:"type"`
	Stmt  *Stmt  `json:"stmt,omitempty"`
	Batch *struct {
		Steps []Step `json:"steps"`
	} `json:"batch,omitempty"`
}

type pipelineRequest struct {
	Baton    *string   `json:"baton"`
	Requests []request `json:"requests"`
}

type streamResult struct {
	Type     string `json:"type"` // "ok" | "error"
	Error    *Error `json:"error"`
	Response *struct {
		Type   string          `json:"type"`
		Result json.RawMessage `json:"result"`
	} `json:"response"`
}

type pipelineResponse struct {
	Baton   *string        `json:"baton"`
	BaseURL *string        `json:"base_url"`
	Results []streamResult `json:"results"`
}

// HTTPError is a non-200 answer to a pipeline request.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// IsAuth reports an authentication or authorization failure.
func (e *HTTPError) IsAuth() bool { return e.Status == 401 || e.Status == 403 }

func (s *Stream) pipeline(ctx context.Context, reqs []request, closeAfter bool) ([]streamResult, error) {
	if s.closed {
		return nil, ErrStreamClosed
	}
	if closeAfter {
		reqs = append(reqs, request{Type: "close"})
	}
	pr := pipelineRequest{Requests: reqs}
	if s.started {
		b := s.baton
		pr.Baton = &b
	}
	body, err := json.Marshal(pr)
	if err != nil {
		return nil, err
	}
	base := s.c.URL
	if s.baseURL != "" {
		base = s.baseURL
	}
	endpoint, err := url.JoinPath(base, "v2/pipeline")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.c.Token)
	}
	hc := s.c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		s.closed = true
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	limit := s.c.MaxResponse
	if limit <= 0 {
		limit = 256 << 20
	}
	lr := &io.LimitedReader{R: resp.Body, N: limit + 1}
	if resp.StatusCode != http.StatusOK {
		s.closed = true
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &HTTPError{Status: resp.StatusCode, Message: errorMessage(b)}
	}
	var out pipelineResponse
	if err := json.NewDecoder(lr).Decode(&out); err != nil {
		s.closed = true
		if lr.N <= 0 {
			return nil, fmt.Errorf("hrana: response larger than %d bytes", limit)
		}
		return nil, fmt.Errorf("hrana: decode response: %w", err)
	}
	s.started = true
	if out.BaseURL != nil && *out.BaseURL != "" {
		s.baseURL = *out.BaseURL
	}
	if out.Baton == nil || *out.Baton == "" {
		s.closed = true
		s.baton = ""
	} else {
		s.baton = *out.Baton
	}
	if len(out.Results) != len(reqs) {
		return nil, fmt.Errorf("hrana: %d results for %d requests", len(out.Results), len(reqs))
	}
	return out.Results, nil
}

func errorMessage(b []byte) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(b, &e) == nil {
		switch {
		case e.Error != "":
			return e.Error
		case e.Message != "" && e.Code != "":
			return e.Message + " (" + e.Code + ")"
		case e.Message != "":
			return e.Message
		}
	}
	return strings.TrimSpace(string(b))
}

func decodeResult(r streamResult, into any) error {
	if r.Type == "error" || r.Error != nil {
		if r.Error == nil {
			return &Error{Message: "unknown error"}
		}
		return r.Error
	}
	if r.Response == nil {
		return errors.New("hrana: empty response")
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(r.Response.Result, into)
}

// Execute runs one statement on the stream.
func (s *Stream) Execute(ctx context.Context, st Stmt) (*StmtResult, error) {
	res, err := s.pipeline(ctx, []request{{Type: "execute", Stmt: &st}}, false)
	if err != nil {
		return nil, err
	}
	var out StmtResult
	if err := decodeResult(res[0], &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Exec runs one statement without arguments, discarding rows.
func (s *Stream) Exec(ctx context.Context, sql string) error {
	_, err := s.Execute(ctx, Stmt{SQL: sql})
	return err
}

// Query runs one statement and returns its rows.
func (s *Stream) Query(ctx context.Context, sql string, args ...Value) (*StmtResult, error) {
	return s.Execute(ctx, Stmt{SQL: sql, Args: args, WantRows: true})
}

// Batch runs steps in one request. A step error is reported in the result,
// not as the returned error.
func (s *Stream) Batch(ctx context.Context, steps []Step) (*BatchResult, error) {
	r := request{Type: "batch", Batch: &struct {
		Steps []Step `json:"steps"`
	}{Steps: steps}}
	res, err := s.pipeline(ctx, []request{r}, false)
	if err != nil {
		return nil, err
	}
	var out BatchResult
	if err := decodeResult(res[0], &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Close closes the stream on the server, rolling back any open transaction.
// It is safe to call more than once.
func (s *Stream) Close(ctx context.Context) error {
	if s.closed {
		return nil
	}
	if !s.started {
		s.closed = true
		return nil
	}
	_, err := s.pipeline(ctx, nil, true)
	s.closed = true
	return err
}

// Closed reports whether the stream can no longer be used.
func (s *Stream) Closed() bool { return s.closed }
