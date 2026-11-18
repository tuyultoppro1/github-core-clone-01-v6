// Forge — a GitHub-compatible git forge, static Go core.
// Entry point: binds an ephemeral port on all interfaces, prints
// "FORGE_PORT <port>" on stdout, serves until killed.
package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

var store = NewStore()
var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")
var reqSeq atomic.Uint64

func main() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Printf("FORGE_PORT %d\n", ln.Addr().(*net.TCPAddr).Port)
	srv := &http.Server{
		Handler: http.HandlerFunc(route),
		// No read timeout: clients hold connections for the whole bench.
	}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func route(w http.ResponseWriter, r *http.Request) {
	// /healthz is always open.
	if r.URL.Path == "/healthz" {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	// Auth: every other route needs a Bearer token. When the shop token
	// is unset (local runs) any non-empty token is accepted.
	tok := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = h[len("Bearer "):]
	}
	if tok == "" {
		writeErr(w, 401, "Requires authentication")
		return
	}
	if shopToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(shopToken)) != 1 {
		writeErr(w, 401, "Bad credentials")
		return
	}

	p := r.URL.Path
	switch {
	case p == "/api/graphql":
		if r.Method != "POST" {
			writeErr(w, 404, "Not Found")
			return
		}
		handleGraphQL(w, r)
	case p == "/mcp":
		handleMCP(w, r)
	case strings.HasPrefix(p, "/api/v3"):
		handleREST(w, r)
	case strings.HasPrefix(p, "/api/"):
		writeErr(w, 404, "Not Found")
	case strings.Contains(p, ".git/"):
		handleGit(w, r)
	default:
		writeErr(w, 404, "Not Found")
	}
}

// --- JSON helpers -------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	var b []byte
	if s, ok := v.(string); ok {
		b = []byte(s)
	} else {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			w.Write([]byte(`{"message":"internal error"}`))
			return
		}
		b = bytes.TrimRight(buf.Bytes(), "\n")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", fmt.Sprintf("%016x", reqSeq.Add(1)))
	w.WriteHeader(status)
	w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func readJSONBody(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	return dec.Decode(v)
}

// decodeJSON is like readJSONBody but reports malformed JSON distinctly.
func decodeJSONBody(r *http.Request, v any) error {
	return readJSONBody(r, v)
}

func jsonBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// escHTML-safe marshal via Encoder with SetEscapeHTML(false) is avoided
// on the hot path; plain json.Marshal is byte-stable for our payloads.
