package main

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
)

//go:embed web
var webFiles embed.FS

// site is the small landing page at "/" (logo, tagline, download button, version, credits).
type site struct {
	cfg  Config
	page *template.Template
}

func newSite(cfg Config) *site {
	return &site{cfg: cfg, page: template.Must(template.ParseFS(webFiles, "web/index.html"))}
}

func (s *site) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	err := s.page.Execute(w, map[string]string{
		"Version": version, "Download": s.cfg.DownloadURL, "Source": s.cfg.SourceURL,
		"Author": s.cfg.AuthorName, "AuthorURL": s.cfg.AuthorURL,
	})
	if err != nil {
		log.Printf("site: %v", err)
	}
}

func (s *site) static() http.Handler {
	sub, _ := fs.Sub(webFiles, "web/static")
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}
