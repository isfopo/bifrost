// Package server wires config and compiled flows to HTTP routes.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"bifrost/internal/auth"
	"bifrost/internal/config"
	"bifrost/internal/deliver"
	"bifrost/internal/flow"
)

// Server is the Bifrost HTTP service.
type Server struct {
	cfg   *config.Config
	flows map[string]*flow.Compiled
	disp  *deliver.Dispatcher
}

// NewServer compiles the config's flows and returns a ready server.
func NewServer(cfg *config.Config) (*Server, error) {
	flows, err := flow.CompileFlows(cfg)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, flows: flows, disp: deliver.NewDispatcher()}, nil
}

// Routes returns the root handler (logging and panic recovery included).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("POST /hooks/{flow}/test", s.handleTest)
	mux.HandleFunc("POST /hooks/{flow}", s.handleHook)
	return logRequest(s.recoverPanics(mux))
}

// recoverPanics turns panics in a flow evaluation into 500s instead of
// killing the connection with nothing logged.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(s.flows))
	for name := range s.flows {
		names = append(names, name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "bifrost",
		"endpoints": []string{
			"POST /hooks/{flow}",
			"POST /hooks/{flow}/test  (?execute=true sends the real request)",
			"GET  /healthz",
		},
		"flows": names,
	})
}

// handleHook is the generic inbound webhook handler. Every flow uses it; all
// specificity lives in the flow config and adapters.
func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	flow := s.lookupFlow(w, r)
	if flow == nil {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, deliver.MaxBodySize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if len(body) > deliver.MaxBodySize {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	if err := auth.Verify(s.cfg.Services[flow.Config().From.Service].From.Auth, r, body); err != nil {
		if errors.Is(err, config.ErrAuth) {
			writeError(w, http.StatusUnauthorized, "authentication failed")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Live traffic: not a dry run.
	s.process(flow, body, w, false)
}

// handleTest is the authoring loop: POST a sample payload, see exactly what
// `when` decides and what `map` produces without sending anything. Add
// ?execute=true to actually perform the delivery.
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	flow := s.lookupFlow(w, r)
	if flow == nil {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, deliver.MaxBodySize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	// The test endpoint bypasses authentication by design: it is for trying
	// mappings locally with a payload copied from a past real delivery. By
	// default it is a dry run; ?execute=true performs the real delivery.
	s.process(flow, body, w, r.URL.Query().Get("execute") != "true")
}

// process runs the shared pipeline: parse -> when -> map -> deliver; it writes
// the pipeline trace to w when dryRun is true.
func (s *Server) process(fl *flow.Compiled, body []byte, w http.ResponseWriter, dryRun bool) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: "+err.Error())
		return
	}

	matched, err := fl.Matches(doc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !matched {
		if dryRun {
			writeJSON(w, http.StatusOK, map[string]any{"matched": false})
		} else {
			// Tell the source 202 Accepted: the event was seen but is not a
			// match for this flow; nothing was delivered.
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted (no matching flow)"})
		}
		return
	}

	mapped, err := fl.MapBody(doc)
	if err != nil {
		if dryRun {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// A mapping error at real-traffic time is a config bug, not the
		// source's fault: do not make the source retry itself forever.
		writeError(w, http.StatusAccepted, "mapped payload failed: "+err.Error())
		log.Printf("flow %q: mapping error: %v", fl.Name, err)
		return
	}

	if dryRun {
		writeJSON(w, http.StatusOK, map[string]any{
			"matched": true,
			"payload": mapped,
		})
		return
	}

	oref := fl.Config().To
	target := s.cfg.Services[oref.Service].To
	if err := s.disp.Deliver(oref, target, mapped); err != nil {
		// The webhook was accepted; delivery problems are Bifrost's to fix.
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted (delivery delayed)"})
		log.Printf("flow %q: delivery to %q failed: %v", fl.Name, oref.Service, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// lookupFlow finds the compiled flow or, if it is missing, writes an error
// response and returns nil. Callers check the nil flow after the call.
func (s *Server) lookupFlow(w http.ResponseWriter, r *http.Request) *flow.Compiled {
	name := r.PathValue("flow")
	fl, ok := s.flows[name]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown flow "+quoted(name))
		return nil
	}
	return fl
}

func quoted(s string) string {
	var out strings.Builder
	out.Grow(len(s) + 2)
	out.WriteByte('"')
	out.WriteString(s)
	out.WriteByte('"')
	return out.String()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// logRequest middleware logs each request with its duration and status.
func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rw.status, time.Since(start))
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
