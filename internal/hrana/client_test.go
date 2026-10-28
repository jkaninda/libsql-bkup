package hrana

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeServer answers pipeline requests, checking batons.
type fakeServer struct {
	t       *testing.T
	batons  []string // baton received per request ("" = none)
	auth    []string
	handler func(req map[string]any) []any
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/prefix/v2/pipeline" {
		http.Error(w, `{"error":"not found"}`, 404)
		return
	}
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	var req struct {
		Baton    *string          `json:"baton"`
		Requests []map[string]any `json:"requests"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Fatal(err)
	}
	b := ""
	if req.Baton != nil {
		b = *req.Baton
	}
	f.batons = append(f.batons, b)
	var results []any
	closing := false
	for _, rq := range req.Requests {
		if rq["type"] == "close" {
			closing = true
			results = append(results, map[string]any{"type": "ok", "response": map[string]any{"type": "close"}})
			continue
		}
		results = append(results, f.handler(rq)...)
	}
	next := any("baton-" + string(rune('a'+len(f.batons))))
	if closing {
		next = nil
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"baton": next, "base_url": nil, "results": results})
}

func ok(result any) map[string]any {
	return map[string]any{"type": "ok", "response": map[string]any{"type": "execute", "result": result}}
}

func TestStreamKeepsBatonAndAuth(t *testing.T) {
	f := &fakeServer{t: t, handler: func(rq map[string]any) []any {
		stmt := rq["stmt"].(map[string]any)
		if stmt["sql"] == "bad" {
			return []any{map[string]any{"type": "error", "error": map[string]any{"message": "no such table: x", "code": "SQLITE_ERROR"}}}
		}
		return []any{ok(map[string]any{
			"cols": []any{map[string]any{"name": "a", "decltype": nil}},
			"rows": []any{[]any{
				map[string]any{"type": "integer", "value": "9223372036854775807"},
			}, []any{
				map[string]any{"type": "text", "value": "x\"y"},
			}},
		})}
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &Client{URL: srv.URL + "/prefix", Token: "tok"}
	s := c.OpenStream()
	ctx := context.Background()
	res, err := s.Query(ctx, "SELECT ?", Text("a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0].Str(); got != "9223372036854775807" {
		t.Fatalf("integer %q", got)
	}
	if got := res.Rows[1][0].Str(); got != `x"y` {
		t.Fatalf("text %q", got)
	}
	err = s.Exec(ctx, "bad")
	var he *Error
	if !errors.As(err, &he) || he.Code != "SQLITE_ERROR" {
		t.Fatalf("statement error: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if !s.Closed() {
		t.Fatal("not closed")
	}
	if err := s.Exec(ctx, "SELECT 1"); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("closed stream: %v", err)
	}
	want := []string{"", "baton-b", "baton-c"}
	if strings.Join(f.batons, ",") != strings.Join(want, ",") {
		t.Fatalf("batons %q, want %q", f.batons, want)
	}
	for _, a := range f.auth {
		if a != "Bearer tok" {
			t.Fatalf("auth header %q", a)
		}
	}
}

func TestBatchStepErrors(t *testing.T) {
	f := &fakeServer{t: t, handler: func(rq map[string]any) []any {
		steps := rq["batch"].(map[string]any)["steps"].([]any)
		res := make([]any, len(steps))
		errs := make([]any, len(steps))
		res[0] = map[string]any{"cols": []any{}, "rows": []any{}}
		errs[1] = map[string]any{"message": "UNIQUE constraint failed"}
		return []any{map[string]any{"type": "ok", "response": map[string]any{"type": "batch", "result": map[string]any{"step_results": res, "step_errors": errs}}}}
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := (&Client{URL: srv.URL + "/prefix/"}).OpenStream()
	br, err := s.Batch(context.Background(), []Step{{Stmt: Stmt{SQL: "a"}}, {Stmt: Stmt{SQL: "b"}, Condition: OK(0)}, {Stmt: Stmt{SQL: "c"}, Condition: OK(1)}})
	if err != nil {
		t.Fatal(err)
	}
	i, e := br.FirstError()
	if i != 1 || e.Message != "UNIQUE constraint failed" {
		t.Fatalf("first error %d %v", i, e)
	}
	if br.StepResults[2] != nil {
		t.Fatal("skipped step has a result")
	}
}

func TestHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"Unauthorized: invalid token"}`))
	}))
	defer srv.Close()
	s := (&Client{URL: srv.URL}).OpenStream()
	err := s.Exec(context.Background(), "SELECT 1")
	var he *HTTPError
	if !errors.As(err, &he) || !he.IsAuth() || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("got %v", err)
	}
	if !s.Closed() {
		t.Fatal("stream must be closed after an HTTP error")
	}
}

func TestValues(t *testing.T) {
	if v := Integer("-5"); v.Str() != "-5" || v.Type != TypeInteger {
		t.Fatal(v)
	}
	var v Value
	if err := json.Unmarshal([]byte(`{"type":"float","value":null}`), &v); err != nil || !v.IsNullFloat() {
		t.Fatalf("%v %v", v, err)
	}
	if err := json.Unmarshal([]byte(`{"type":"float","value":1.5}`), &v); err != nil || v.IsNullFloat() || v.Str() != "1.5" {
		t.Fatalf("%v %v", v, err)
	}
}
