// Panel serving with runtime Base URL injection (G5 ruling).
//
// Contract (contract-json-tags-v0.5.md §6, frozen attachment):
//   - index.html carries an inline <script>window.__ALL2API_BASE__=%q</script>
//     placed before the bundle;
//   - value priority: config external_base_url → request X-Forwarded-Prefix → "";
//   - when empty the frontend falls back to location.origin itself, so the
//     backend must not invent a origin here.
package web

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"all2api/internal/config"
)

// deriveBase implements the G5 priority chain for one request.
// external_base_url is passed through verbatim (owner-configured full URL,
// including any /v1 suffix — the backend never rewrites it).
// X-Forwarded-Prefix is resolved against the forwarded scheme/host so the
// panel's ConnectSnippet can display a complete copyable address.
func deriveBase(r *http.Request) string {
	if b := strings.TrimSpace(config.Shared.Get().ExternalBaseURL); b != "" {
		return b
	}
	prefix := strings.TrimSpace(r.Header.Get("X-Forwarded-Prefix"))
	if prefix == "" {
		return ""
	}
	prefix = "/" + strings.Trim(prefix, "/")
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host + prefix + "/v1"
}

// NewPanelHandler serves the embedded dist/ with SPA fallback to index.html
// and injects the __ALL2API_BASE__ declaration before the bundle.
func NewPanelHandler() http.Handler {
	sub, err := fs.Sub(Dist, "dist")
	if err != nil {
		sub = Dist // unreachable: "dist" is the embedded root
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if name != "index.html" {
			// fs.Stat rejects ".." traversal; anything invalid falls through to the
			// index fallback, matching SPA-router behavior.
			if st, statErr := fs.Stat(sub, name); statErr == nil && !st.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
		}
		serveIndexInjected(w, r, sub)
	})
}

// serveIndexInjected renders index.html (or a placeholder while the frontend
// build has not landed) with the injection tag immediately after <head>.
func serveIndexInjected(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	tag := fmt.Sprintf("<script>window.__ALL2API_BASE__=%q</script>", deriveBase(r))

	var html []byte
	doc, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		html = []byte(placeholderDoc(tag))
	} else {
		html = injectTag(doc, tag)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The injected base depends on request headers and hot-reloadable config;
	// caching index.html would freeze a stale derivation.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(html)
}

// injectTag places the tag right after the opening <head> tag, guaranteeing
// it precedes any bundle script regardless of where the bundle is referenced.
func injectTag(doc []byte, tag string) []byte {
	if i := bytes.Index(bytes.ToLower(doc), []byte("<head")); i >= 0 {
		if j := bytes.Index(doc[i:], []byte(">")); j >= 0 {
			pos := i + j + 1
			out := make([]byte, 0, len(doc)+len(tag)+1)
			out = append(out, doc[:pos]...)
			out = append(out, '\n')
			out = append(out, tag...)
			out = append(out, doc[pos:]...)
			return out
		}
	}
	out := make([]byte, 0, len(doc)+len(tag)+1)
	out = append(out, tag...)
	out = append(out, '\n')
	out = append(out, doc...)
	return out
}

func placeholderDoc(tag string) string {
	return "<!doctype html>\n<html lang=\"zh-CN\">\n<head>\n" + tag +
		"\n<meta charset=\"utf-8\">\n<title>all2api</title>\n</head>\n<body>\n" +
		"<p>all2api 面板资源尚未构建（web-panel dist/ 为空占位）。</p>\n</body>\n</html>\n"
}
