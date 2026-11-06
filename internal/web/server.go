// Package web serves the UI, the video stream and the JSON API.
package web

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mhbvr/camera_rpi/internal/camera"
	"github.com/mhbvr/camera_rpi/internal/proto"
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
}

// New builds the server.
func New(mgr *camera.Manager, hub *stream.Hub, frames *stream.FrameHub, logger *log.Logger) *Server {
	s := &Server{
		mgr:    mgr,
		hub:    hub,
		frames: frames,
		log:    logger,
		tmpl:   template.Must(template.ParseFS(templatesFS, "templates/index.html")),
		mux:    http.NewServeMux(),
	}
	static, _ := fs.Sub(staticFS, "static")
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	s.mux.HandleFunc("GET /stream", s.handleStream)
	s.mux.HandleFunc("GET /stream.mjpeg", s.handleMJPEG)
	s.mux.HandleFunc("GET /api/config", s.handleGetConfig)
	s.mux.HandleFunc("POST /api/config", s.handlePostConfig)
	s.mux.HandleFunc("POST /api/config/reset", s.handleReset)
	s.mux.HandleFunc("POST /api/capture", s.handleCapture)
	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

type connKey struct{}

// ConnContext stores the connection in the request context so /stream can
// shrink its send buffer. Use it as http.Server.ConnContext.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

// ui holds the ranges rendered into the settings form. Validation uses
// camera.Limits; some sliders use a narrower practical range.
type ui struct {
	L             any
	StreamSizes   []proto.Size
	Metering      []string
	ExposureModes []string
	AWBModes      []string
	DenoiseModes  []string
	ContrastMax   float64
	SaturationMax float64
	SharpnessMax  float64
	BitrateMinM   float64
	BitrateMaxM   float64
	// MJPEG fallback bitrate range in Mbit/s
	MJPEGBitrateMinM float64
	MJPEGBitrateMaxM float64
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	d := ui{
		L:             camera.Limits,
		StreamSizes:   camera.StreamSizes,
		Metering:      camera.MeteringModes,
		ExposureModes: camera.ExposureModes,
		AWBModes:      camera.AWBModes,
		DenoiseModes:  camera.DenoiseModes,
		ContrastMax:   4,
		SaturationMax: 4,
		SharpnessMax:  8,
		BitrateMinM:   float64(camera.Limits.BitrateMin) / 1e6,
		BitrateMaxM:   float64(camera.Limits.BitrateMax) / 1e6,

		MJPEGBitrateMinM: float64(camera.Limits.MJPEGBitrateMin) / 1e6,
		MJPEGBitrateMaxM: float64(camera.Limits.MJPEGBitrateMax) / 1e6,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, d); err != nil {
		s.log.Printf("web: template: %v", err)
	}
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	// ?video=0: events only (state, metadata, stats) for MJPEG fallback pages.
	sub := s.hub.Subscribe()
	if r.URL.Query().Get("video") == "0" {
		sub = s.hub.SubscribeEvents()
	}
	defer s.hub.Unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if c, ok := r.Context().Value(connKey{}).(net.Conn); ok {
		if tc, ok := c.(*tls.Conn); ok {
			c = tc.NetConn()
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetWriteBuffer(256 << 10) // keep backlog in the hub, where it can be dropped
		}
	}
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	last := time.Now()
	write := func(m *stream.Message) error {
		_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := w.Write(m.Wire); err != nil {
			return err
		}
		last = time.Now()
		return rc.Flush()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m, ok := <-sub.C:
			if !ok {
				return
			}
			if err := write(m); err != nil {
				return
			}
		case <-ticker.C:
			if time.Since(last) >= 2*time.Second {
				if err := write(stream.Keepalive()); err != nil {
					return
				}
			}
		}
	}
}

// mjpegBoundary separates the parts of the multipart MJPEG stream.
const mjpegBoundary = "camweb-frame"

// handleMJPEG serves the MJPEG fallback (DESIGN.md §5.5) as
// multipart/x-mixed-replace for an <img> element. While at least one
// client is connected, the helper runs a hardware MJPEG encoder; each
// client gets the newest frame (slow clients skip frames).
func (s *Server) handleMJPEG(w http.ResponseWriter, r *http.Request) {
	if s.frames == nil {
		http.NotFound(w, r)
		return
	}
	sub := s.frames.Subscribe()
	defer s.frames.Unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "multipart/x-mixed-replace; boundary="+mjpegBoundary)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-sub.C:
			if !ok {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", mjpegBoundary, len(frame)); err != nil {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if _, err := io.WriteString(w, "\r\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Config())
}

type applyResponse struct {
	Applied camera.ApplyKind `json:"applied"`
	Config  camera.Config    `json:"config"`
}

func (s *Server) handlePostConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		s.writeError(w, err)
		return
	}
	kind, cfg, err := s.mgr.Apply(r.Context(), body)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, applyResponse{kind, cfg})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	kind, cfg, err := s.mgr.Reset(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, applyResponse{kind, cfg})
}

func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Quality int `json:"quality"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			s.writeError(w, &camera.FieldError{Field: "", Msg: "invalid JSON: " + err.Error()})
			return
		}
	}
	res, err := s.mgr.Snapshot(r.Context(), req.Quality)
	if err != nil {
		s.writeError(w, err)
		return
	}
	f, err := os.Open(res.File.Path)
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.writeError(w, err)
		return
	}
	name := filepath.Base(res.File.Path)
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	h.Set("X-Capture-Width", fmt.Sprint(res.File.Width))
	h.Set("X-Capture-Height", fmt.Sprint(res.File.Height))
	h.Set("Cache-Control", "no-store")
	s.log.Printf("web: snapshot %s (%dx%d, %d bytes, timing %v)", name, res.File.Width, res.File.Height, st.Size(), res.TimingMS)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	resp := map[string]string{"error": err.Error()}
	var fe *camera.FieldError
	var re *proto.RPCError
	switch {
	case errors.As(err, &fe):
		code = http.StatusBadRequest
		resp["error"], resp["field"] = fe.Msg, fe.Field
	case errors.Is(err, camera.ErrNoCamera):
		code = http.StatusServiceUnavailable
	case errors.As(err, &re):
		switch re.Code {
		case proto.CodeInvalidParams:
			code = http.StatusBadRequest
			resp["field"] = re.Field()
		case proto.CodeBusy:
			code = http.StatusConflict
		case proto.CodeNotConfigured:
			code = http.StatusServiceUnavailable
		}
	case errors.Is(err, context.DeadlineExceeded):
		code = http.StatusGatewayTimeout
	}
	if code >= 500 {
		s.log.Printf("web: %v", err)
	}
	writeJSON(w, code, resp)
}
