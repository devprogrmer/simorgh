package node

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
)

// Server exposes a Runner over HTTP for the master to drive.
//
// It is transport only: every route unmarshals, calls the injected Runner, and
// marshals the result. The Runner it wraps on a real node is the same
// LocalRunner the master uses on its own host, which is what makes the two
// paths the conformance suite compares genuinely the same code underneath.
//
// TLS is NOT set up here. The caller supplies the listener, so tests can drive a
// real handshake through httptest and the node binary can use NodeTLSConfig.
// Splitting it that way keeps the security boundary in one place (ca.go) rather
// than half here and half there.
type Server struct {
	runner Runner

	mu sync.Mutex
	// lastGeneration is the highest generation this node has accepted. A state
	// that does not advance it is refused: without that check, anyone able to
	// record one Apply could replay it later to restore a client set the operator
	// has since revoked, and the node would apply it believing it came from the
	// master -- which, cryptographically, it did.
	lastGeneration int64
}

func NewServer(r Runner) *Server { return &Server{runner: r} }

// ErrStaleGeneration is returned for a desired state that does not advance the
// node's generation counter.
var ErrStaleGeneration = errors.New("node: refusing a desired state whose generation does not advance")

// Handler returns the routes. A plain ServeMux rather than gin: this surface is
// six endpoints with no middleware, no sessions and no templating, and pulling
// the panel's web framework into the node binary would buy nothing.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/apply", s.handleApply)
	mux.HandleFunc("/collect", s.handleCollect)
	mux.HandleFunc("/enforce", s.handleEnforce)
	mux.HandleFunc("/provision", s.handleProvision)
	mux.HandleFunc("/logs", s.handleLogs)
	return mux
}

// errorBody is how a failure crosses the wire.
//
// A non-2xx status alone is not enough: the master has to be able to tell a
// node that refused the work from a node that did it, and turning a refusal
// into a zero-valued success is the specific bug this shape prevents.
type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, errorBody{Error: err.Error()})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.runner.Status(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var state DesiredState
	if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Checked before the work, not after: a replayed state must not be applied
	// and then regretted.
	s.mu.Lock()
	if state.Generation <= s.lastGeneration {
		last := s.lastGeneration
		s.mu.Unlock()
		writeErr(w, http.StatusConflict,
			errors.New(ErrStaleGeneration.Error()+" (got "+itoa(state.Generation)+", already at "+itoa(last)+")"))
		return
	}
	s.mu.Unlock()

	res, err := s.runner.Apply(r.Context(), state)
	if err != nil {
		// The generation is NOT advanced on failure. Advancing it would leave the
		// master unable to retry the same state, since its own retry would then
		// look like a replay.
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	s.mu.Lock()
	if state.Generation > s.lastGeneration {
		s.lastGeneration = state.Generation
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCollect(w http.ResponseWriter, r *http.Request) {
	res, err := s.runner.Collect(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleEnforce(w http.ResponseWriter, r *http.Request) {
	var set DisabledSet
	if err := json.NewDecoder(r.Body).Decode(&set); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.runner.Enforce(r.Context(), set); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// handleProvision streams steps as newline-delimited JSON.
//
// Streamed rather than collected and returned at the end because provisioning
// can install a kernel and repoint the bootloader, which takes minutes; an
// operator watching a blank screen cannot tell that from a hang. Each line is
// flushed as it is produced.
func (s *Server) handleProvision(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cores []string `json:"cores"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ch, err := s.runner.Provision(r.Context(), req.Cores)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	for step := range ch {
		if err := enc.Encode(step); err != nil {
			return // client gone; draining the rest would serve nobody
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	out, err := s.runner.Logs(r.Context(), r.URL.Query().Get("core"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Logs string `json:"logs"`
	}{Logs: out})
}

// itoa keeps strconv out of this file's imports for one call site.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
