// Package web serves the UI, the video streams and the REST API
// (docs/REST_API.md). Everything the bundled UI does goes through these
// endpoints (its form ranges are the same data as GET /api/schema), so an
// alternative interface can do the same.
package web

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/mhbvr/camera_rpi/internal/camera"
	"github.com/mhbvr/camera_rpi/internal/stream"
)

//go:embed templates/index.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Server is the HTTP front end.
type Server struct {
	mgr    *camera.Manager
	hub    *stream.Hub
	frames *stream.FrameHub // MJPEG fallback
	log    *log.Logger
	tmpl   *template.Template
	mux    *http.ServeMux

	// CORSOrigin, if set, is sent as Access-Control-Allow-Origin so a UI
	// served from another origin can use the API ("*" allows any).
	CORSOrigin string
}

// New builds the server.
func New(mgr *camera.Manager, hub *stream.Hub, frames *stream.FrameHub, logger *log.Logger) *Server {
	s := &Server{
		mgr:    mgr,
		hub:    hub,
		frames: frames,
		log:    logger,
		tmpl:   template.Must(template.New("index.html").Funcs(schemaFuncs).ParseFS(templatesFS, "templates/index.html")),
		mux:    http.NewServeMux(),
	}
	static, _ := fs.Sub(staticFS, "static")
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	s.mux.HandleFunc("GET /stream", s.handleStream)
	s.mux.HandleFunc("GET /stream.mjpeg", s.handleMJPEG)
	s.mux.HandleFunc("GET /stream.h264", s.handleH264)
	s.mux.HandleFunc("GET /api/schema", s.handleSchema)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/config", s.handleGetConfig)
	s.mux.HandleFunc("POST /api/config", s.handlePostConfig)
	s.mux.HandleFunc("POST /api/config/reset", s.handleReset)
	s.mux.HandleFunc("POST /api/capture", s.handleCapture)
	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	if s.CORSOrigin == "" {
		return s.mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", s.CORSOrigin)
		h.Set("Access-Control-Expose-Headers", "Content-Disposition, X-Capture-Width, X-Capture-Height")
		if s.CORSOrigin != "*" {
			h.Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions { // preflight
			h.Set("Access-Control-Allow-Methods", "GET, POST")
			h.Set("Access-Control-Allow-Headers", "Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

type connKey struct{}

// ConnContext stores the connection in the request context so /stream can
// shrink its send buffer. Use it as http.Server.ConnContext.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

// schemaFuncs give the page template the form ranges and choices from the
// settings schema (camera.Schema), so they always match validation. The
// template uses narrower slider ranges in a few places (contrast, saturation,
// sharpness, white-balance gains); those are UI choices written in the
// template.
var schemaFuncs = template.FuncMap{
	"min":    func(path string) string { return fmtNum(*camera.FieldByPath(path).Min) },
	"max":    func(path string) string { return fmtNum(*camera.FieldByPath(path).Max) },
	"minM":   func(path string) string { return fmtNum(*camera.FieldByPath(path).Min / 1e6) }, // bit/s → Mbit/s
	"maxM":   func(path string) string { return fmtNum(*camera.FieldByPath(path).Max / 1e6) },
	"values": func(path string) []string { return camera.FieldByPath(path).Values },
	"sizes":  func(path string) [][2]int { return camera.FieldByPath(path).Sizes },
}

func fmtNum(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, nil); err != nil {
		s.log.Printf("web: template: %v", err)
	}
}
