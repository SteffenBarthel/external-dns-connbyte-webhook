package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// DefaultMaxBodySize matches the external-dns default request/response cap.
const DefaultMaxBodySize = 32 << 20

// Handler returns the provider API (/, /records, /adjustendpoints).
// It should only be reachable by external-dns, ideally on localhost.
func Handler(p Provider, log *slog.Logger, timeout time.Duration) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	s := &server{p: p, log: log, timeout: timeout}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.negotiate)
	mux.HandleFunc("/records", s.records)
	mux.HandleFunc("/adjustendpoints", s.adjustEndpoints)
	return mux
}

// HealthHandler returns the exposed endpoints (/healthz).
func HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

type server struct {
	p       Provider
	log     *slog.Logger
	timeout time.Duration
}

func (s *server) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	if s.timeout > 0 {
		return context.WithTimeout(r.Context(), s.timeout)
	}
	return context.WithCancel(r.Context())
}

func (s *server) negotiate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.p.DomainFilter())
}

func (s *server) records(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.ctx(r)
	defer cancel()

	switch r.Method {
	case http.MethodGet:
		eps, err := s.p.Records(ctx)
		if err != nil {
			s.log.Error("failed to list records", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to list records")
			return
		}
		if eps == nil {
			eps = []*Endpoint{}
		}
		writeJSON(w, http.StatusOK, eps)
	case http.MethodPost:
		var changes Changes
		if status, err := decode(w, r, &changes); err != nil {
			s.log.Error("failed to decode changes", "error", err)
			writeError(w, status, "invalid request body")
			return
		}
		if err := s.p.ApplyChanges(ctx, &changes); err != nil {
			s.log.Error("failed to apply changes", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to apply changes")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *server) adjustEndpoints(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var eps []*Endpoint
	if status, err := decode(w, r, &eps); err != nil {
		s.log.Error("failed to decode endpoints", "error", err)
		writeError(w, status, "invalid request body")
		return
	}
	out, err := s.p.AdjustEndpoints(eps)
	if err != nil {
		s.log.Error("failed to adjust endpoints", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to adjust endpoints")
		return
	}
	if out == nil {
		out = []*Endpoint{}
	}
	writeJSON(w, http.StatusOK, out)
}

func decode(w http.ResponseWriter, r *http.Request, v any) (int, error) {
	defer r.Body.Close()
	body := http.MaxBytesReader(w, r.Body, DefaultMaxBodySize)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return http.StatusRequestEntityTooLarge, err
		}
		return http.StatusBadRequest, err
	}
	return 0, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", MediaType)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}
