// net/http round trip against a local httptest server: an interpreted
// http.Handler serving JSON, and a client decoding it. No external network.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
)

type api struct{ calls int }

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.calls++
	if r.URL.Path != "/v1/items" {
		http.Error(w, "nope", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"items": []string{"a", "b"}, "page": r.URL.Query().Get("page")})
}

func main() {
	h := &api{}
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, p := range []string{"/v1/items?page=2", "/missing"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			fmt.Println("err", err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%d %s %q\n", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	fmt.Println(rec.Code, rec.Body.String(), h.calls)
}
