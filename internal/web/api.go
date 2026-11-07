package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mhbvr/camera_rpi/internal/camera"
	"github.com/mhbvr/camera_rpi/internal/proto"
)

// JSON API: /api/config, /api/config/reset, /api/schema, /api/status,
// /api/capture (docs/REST_API.md).

// statusResponse is the GET /api/status response.
type statusResponse struct {
	// Latest event of each kind (see docs/REST_API.md "Events"); absent
	// until the first one arrives.
	State    json.RawMessage `json:"state,omitempty"`
	Camera   json.RawMessage `json:"camera,omitempty"`
	Stream   json.RawMessage `json:"stream,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Stats    json.RawMessage `json:"stats,omitempty"`
	Clients  struct {
		Stream int `json:"stream"` // /stream, /stream.h264 and /api/events clients
		MJPEG  int `json:"mjpeg"`  // /stream.mjpeg clients
	} `json:"clients"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ev := s.hub.StickyEvents()
	resp := statusResponse{State: ev["state"], Camera: ev["camera"], Stream: ev["stream"], Metadata: ev["metadata"], Stats: ev["stats"]}
	resp.Clients.Stream = s.hub.Viewers()
	if s.frames != nil {
		resp.Clients.MJPEG = s.frames.Viewers()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, camera.Schema())
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
