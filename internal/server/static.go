package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"

	"tally/web"
)

// Assets are read once at start. Text is gzipped once, and every URL carries a version so a
// browser can keep a file for a year and still pick up a new release at once.
type asset struct {
	body, gz []byte
	ctype    string
	etag     string
}

type assets struct {
	version string
	files   map[string]*asset
	index   string
	login   *template.Template
}

var contentTypes = map[string]string{
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".woff2":       "font/woff2",
	".webmanifest": "application/manifest+json",
	".txt":         "text/plain; charset=utf-8",
}

func loadAssets() (*assets, error) {
	a := &assets{files: map[string]*asset{}}
	var names []string
	err := fs.WalkDir(web.Static, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := web.Static.ReadFile(p)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(p, "static/")
		sum := sha256.Sum256(body)
		f := &asset{body: body, ctype: contentTypes[path.Ext(p)], etag: `W/"` + hex.EncodeToString(sum[:8]) + `"`}
		if f.ctype == "" {
			f.ctype = "application/octet-stream"
		}
		if strings.HasPrefix(f.ctype, "text/") || strings.Contains(f.ctype, "svg") || strings.Contains(f.ctype, "manifest") {
			f.gz = gzipped(body)
		}
		a.files[name] = f
		names = append(names, name+f.etag)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	a.version = hex.EncodeToString(sum[:5])
	b, err := web.Pages.ReadFile("index.html")
	if err != nil {
		return nil, err
	}
	a.index = strings.ReplaceAll(string(b), "{{V}}", a.version)
	if a.login, err = template.ParseFS(web.Pages, "login.html"); err != nil {
		return nil, err
	}
	return a, nil
}

func gzipped(b []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(enc), "gzip") && strings.ReplaceAll(strings.TrimSpace(q), " ", "") != "q=0" {
			return true
		}
	}
	return false
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("path")
	f := s.assets.files[name]
	if f == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	h.Set("Vary", "Accept-Encoding")
	// Fonts never change under the same name. Everything else is versioned by the page.
	if r.URL.Query().Get("v") == s.assets.version || strings.HasPrefix(name, "fonts/") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := f.body
	if f.gz != nil && len(f.gz) < len(body) && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		body = f.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

// writeBody sends a page, gzipped when the browser takes it.
func writeBody(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	w.Header().Set("Vary", "Accept-Encoding")
	if acceptsGzip(r) && len(body) > 1024 {
		w.Header().Set("Content-Encoding", "gzip")
		body = gzipped(body)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

// index is the app with its first state written into the page, or sign in.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	ok, err := s.signedIn(w, r)
	if err != nil {
		s.log.Error("check session", "err", err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	if !ok {
		msg := ""
		if r.URL.Query().Has("unsaved") {
			msg = "Signed out before the last change was saved"
		}
		s.renderLogin(w, r, http.StatusOK, msg)
		return
	}
	st, err := s.state(r)
	if err != nil {
		s.log.Error("read state", "err", err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	// json.Marshal escapes <, > and &, so the state cannot close the script tag it sits in.
	data, err := json.Marshal(st)
	if err != nil {
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	page := strings.Replace(s.assets.index, "{{STATE}}", string(data), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	writeBody(w, r, http.StatusOK, []byte(page))
}
