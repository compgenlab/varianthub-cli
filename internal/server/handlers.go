package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/vcf"
)

// maxVCFUpload caps an uploaded VCF at 64 MiB (sites-only, so this is generous).
const maxVCFUpload = 64 << 20

// --- annotation discovery + selection -------------------------------------

type annotationInfo struct {
	Name        string `json:"name"`
	Field       string `json:"field,omitempty"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Default     bool   `json:"default"`
}

type sourceInfo struct {
	Name          string           `json:"name"`
	Version       string           `json:"version,omitempty"`
	Title         string           `json:"title,omitempty"`
	Type          string           `json:"type"` // "data" | "builtin" | "tool" | "genelist"
	NonCommercial bool             `json:"non_commercial,omitempty"`
	Annotations   []annotationInfo `json:"annotations"`
}

type annotationsResponse struct {
	Snapshot string       `json:"snapshot"` // slug
	Title    string       `json:"title"`    // human-readable snapshot name (falls back to slug)
	Assembly string       `json:"assembly"`
	Sources  []sourceInfo `json:"sources"`
}

// sourceKind names a source's kind for the discovery payload.
func sourceKind(s config.Source) string {
	switch {
	case s.IsBuiltinSource():
		return "builtin"
	case s.IsTool():
		return "tool"
	case s.IsGeneList():
		return "genelist"
	default:
		return "data"
	}
}

// describeAnnotations builds the discovery payload from the loaded snapshot: each
// source and the annotation fields it exposes, marking the default-set members.
func (s *Server) describeAnnotations() annotationsResponse {
	resp := annotationsResponse{Snapshot: s.snap.Name, Title: s.snap.DisplayTitle(), Assembly: s.snap.Assembly}
	// Index the flat, derived annotation list (carries Source + Default) by source.
	for _, src := range s.snap.Sources {
		info := sourceInfo{
			Name: src.Name, Version: src.Version, Title: src.Title, Type: sourceKind(src),
			NonCommercial: src.NonCommercial,
			Annotations:   []annotationInfo{},
		}
		for _, a := range s.snap.Annotations {
			if !annotationBelongs(a, src) {
				continue
			}
			info.Annotations = append(info.Annotations, annotationInfo{
				Name: a.Name, Field: a.Field, Type: a.Type, Description: a.Description, Default: a.Default,
			})
		}
		resp.Sources = append(resp.Sources, info)
	}
	return resp
}

// annotationBelongs reports whether the flat annotation a was declared on source
// src. For a builtin container the derived Source is the builtin name, so match on
// the builtin's presence in the source's nested annotations.
func annotationBelongs(a config.Annotation, src config.Source) bool {
	if src.IsBuiltinSource() {
		for _, sa := range src.Annotations {
			if sa.Builtin == a.Source && sa.Name == a.Name {
				return true
			}
		}
		return false
	}
	return a.Source == src.Name
}

func (s *Server) handleAnnotations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.describeAnnotations())
}

// resolveSelection turns a stored selection string into the concrete annotation
// set: "" → the snapshot defaults, "all" → every annotation, else a comma list of
// names (unknown names error).
func resolveSelection(snap *config.Snapshot, selection string) ([]config.Annotation, error) {
	selection = strings.TrimSpace(selection)
	if selection == "all" {
		return snap.SelectAnnotations(nil, true)
	}
	var keys []string
	for _, k := range strings.Split(selection, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return snap.SelectAnnotations(keys, false)
}

func annotationNames(anns []config.Annotation) []string {
	names := make([]string, 0, len(anns))
	for _, a := range anns {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return names
}

// selectionField accepts the request's "annotations" value in either shape — the
// string "all", or an array of annotation names — and normalizes it to the stored
// selection string ("" = defaults, "all", or a comma-joined name list).
type selectionField struct {
	all   bool
	names []string
	set   bool
}

func (sf *selectionField) UnmarshalJSON(b []byte) error {
	sf.set = true
	var asString string
	if err := json.Unmarshal(b, &asString); err == nil {
		if strings.EqualFold(asString, "all") {
			sf.all = true
		} else if s := strings.TrimSpace(asString); s != "" {
			sf.names = splitCSV(s)
		}
		return nil
	}
	return json.Unmarshal(b, &sf.names)
}

func (sf selectionField) selection() string {
	if sf.all {
		return "all"
	}
	return strings.Join(sf.names, ",")
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// --- submit handlers -------------------------------------------------------

type annotateLocusRequest struct {
	Locus       string         `json:"locus"`
	Snapshot    string         `json:"snapshot,omitempty"`
	Annotations selectionField `json:"annotations,omitempty"`
}

// handleAnnotateLocus serves POST /v1/annotate and POST /ui/submit: it validates
// the locus + annotation selection, enqueues a locus job, and returns its id.
func (s *Server) handleAnnotateLocus(w http.ResponseWriter, r *http.Request) {
	var req annotateLocusRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Locus) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing \"locus\" (chrom:pos:ref:alt)")
		return
	}
	if !s.snapshotOK(w, req.Snapshot) {
		return
	}
	selection := req.Annotations.selection()
	if !s.validate(w, req.Locus, selection) {
		return
	}
	if !s.toolGateOK(w, r, selection) {
		return
	}
	s.enqueue(w, r, KindLocus, selection, req.Locus, []byte(req.Locus))
}

// handleAnnotateVCF serves POST /v1/annotate/vcf: a multipart VCF upload (field
// "vcf"), with optional "snapshot" and "annotations" form fields. It enqueues a
// vcf job whose input is the uploaded bytes.
func (s *Server) handleAnnotateVCF(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxVCFUpload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}
	file, header, err := r.FormFile("vcf")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing \"vcf\" file field")
		return
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxVCFUpload))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "uploaded VCF is empty")
		return
	}
	if !s.snapshotOK(w, r.FormValue("snapshot")) {
		return
	}
	// annotations form field is a comma-separated list, or "all".
	var selection string
	if raw := strings.TrimSpace(r.FormValue("annotations")); raw != "" {
		if strings.EqualFold(raw, "all") {
			selection = "all"
		} else {
			selection = strings.Join(splitCSV(raw), ",")
		}
	}
	if !s.validateSelection(w, selection) {
		return
	}
	if !s.toolGateOK(w, r, selection) {
		return
	}
	label := "VCF upload"
	if header != nil && header.Filename != "" {
		label = header.Filename
	}
	s.enqueue(w, r, KindVCF, selection, label, body)
}

const sessionCookie = "varhub_session"

// sessionID returns the requester's session id from its cookie (browser) or the
// X-Varhub-Session header (API clients), or "" when there is none.
func (s *Server) sessionID(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return c.Value
	}
	return strings.TrimSpace(r.Header.Get("X-Varhub-Session"))
}

// ensureSession returns the requester's session id, minting one (Set-Cookie) if
// absent so a browser gets a stable id to scope its request history by. Must be
// called before the response body is written.
func (s *Server) ensureSession(w http.ResponseWriter, r *http.Request) string {
	if id := s.sessionID(r); id != "" {
		return id
	}
	id, err := newID()
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 90 * 24 * 3600,
	})
	return id
}

// authed reports whether the request carries a valid bearer token (used to relax
// the unauthenticated tool-source gate for /ui callers that do present a token).
func (s *Server) authed(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && VerifyToken(s.cfg.Server.MasterKey, strings.TrimSpace(tok))
}

// toolGateOK blocks unauthenticated requests from triggering expensive type="tool"
// sources (VEP/ANNOVAR — the main compute-amplification vector) unless the server
// opts in via allow_tools_unauth. Authenticated requests are always allowed. On a
// block it writes a 403 and returns false.
func (s *Server) toolGateOK(w http.ResponseWriter, r *http.Request, selection string) bool {
	if s.cfg.Server.ToolsAllowedUnauth() || s.authed(r) {
		return true
	}
	anns, err := resolveSelection(s.snap, selection)
	if err != nil {
		return true // selection already validated upstream; don't double-report
	}
	for _, a := range anns {
		if src := s.snap.SourceByName(a.Source); src != nil && src.IsTool() {
			writeJSONError(w, http.StatusForbidden,
				"tool-based annotations require an API token on this server")
			return false
		}
	}
	return true
}

// snapshotOK enforces that a request's optional snapshot matches the one the
// server is pinned to (arbitrary-snapshot requests are not supported yet).
func (s *Server) snapshotOK(w http.ResponseWriter, name string) bool {
	if name != "" && name != s.snap.Name {
		writeJSONError(w, http.StatusBadRequest,
			"server is pinned to snapshot "+s.snap.Name+"; remove \"snapshot\" or match it")
		return false
	}
	return true
}

// validate checks both the locus syntax and the annotation selection up front, so
// the client gets immediate 400s rather than a failed job.
func (s *Server) validate(w http.ResponseWriter, locus, selection string) bool {
	if _, err := vcf.ParseLocus(locus); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return s.validateSelection(w, selection)
}

func (s *Server) validateSelection(w http.ResponseWriter, selection string) bool {
	if _, err := resolveSelection(s.snap, selection); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// enqueue records a job (tagged with the trusted-proxy-resolved client IP for fair
// scheduling). With a ?wait= (capped by submit_wait), it blocks up to that long for
// the job to finish and returns the results inline — so fast jobs come back done in
// one round trip. Otherwise it writes the async 202 { job_id } response as before.
func (s *Server) enqueue(w http.ResponseWriter, r *http.Request, kind, selection, label string, body []byte) {
	ip := clientIP(r, s.trusted)
	session := s.ensureSession(w, r) // tags the job so the submitter can browse it later
	id, err := s.queue.Enqueue(r.Context(), NewJob{
		Kind: kind, Snapshot: s.snap.Name, Selection: selection,
		ClientIP: ip, Session: session, Label: label, Body: body,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}
	wait := s.parseWait(r)
	if wait <= 0 {
		writeJSON(w, http.StatusAccepted, map[string]string{"job_id": id})
		return
	}
	job, _, err := s.queue.WaitFor(r.Context(), id, wait)
	if err != nil {
		// ctx cancelled (client gone) or a DB error — still hand back the id to poll.
		writeJSON(w, http.StatusAccepted, submitResponse{JobID: id, Status: StatusQueued})
		return
	}
	s.writeSubmitResult(w, r.Context(), id, job)
}

// submitResponse is the wait-aware submit/poll payload: it always carries the job
// id and status, plus the results array (and n_variants) once the job is done, or
// the error message if it failed.
type submitResponse struct {
	JobID     string          `json:"job_id"`
	Status    string          `json:"status"`
	NVariants int64           `json:"n_variants,omitempty"`
	Error     string          `json:"error,omitempty"`
	Results   json.RawMessage `json:"results,omitempty"`
}

// writeSubmitResult renders a job's current state: 200 with results when done, 200
// with the error when failed, else 202 with the job id to keep polling.
func (s *Server) writeSubmitResult(w http.ResponseWriter, ctx context.Context, id string, job Job) {
	switch job.Status {
	case StatusDone:
		result, ok, err := s.queue.Result(ctx, id)
		if err != nil || !ok {
			writeJSON(w, http.StatusAccepted, submitResponse{JobID: id, Status: StatusRunning})
			return
		}
		writeJSON(w, http.StatusOK, submitResponse{
			JobID: id, Status: StatusDone, NVariants: job.NVariants, Results: json.RawMessage(result)})
	case StatusError:
		writeJSON(w, http.StatusOK, submitResponse{JobID: id, Status: StatusError, Error: job.Error})
	default:
		writeJSON(w, http.StatusAccepted, submitResponse{JobID: id, Status: job.Status})
	}
}

// parseWait reads a bounded wait from ?wait= (plain seconds like "10" or a Go
// duration like "10s"), capped by the server's submit_wait. 0/absent = don't wait.
func (s *Server) parseWait(r *http.Request) time.Duration {
	raw := strings.TrimSpace(r.URL.Query().Get("wait"))
	if raw == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.Atoi(raw); err == nil {
		d = time.Duration(n) * time.Second
	} else if pd, err := time.ParseDuration(raw); err == nil {
		d = pd
	} else {
		return 0
	}
	if d < 0 {
		return 0
	}
	if capD := s.cfg.Server.SubmitWaitCap(); d > capD {
		d = capD
	}
	return d
}

// --- ops endpoints ---------------------------------------------------------

// handleHealthz is an unauthenticated liveness/readiness probe (for the reverse
// proxy). It reports the snapshot the server is serving.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "ok",
		"snapshot": s.snap.Name,
		"title":    s.snap.DisplayTitle(),
		"assembly": s.snap.Assembly,
	})
}

// handleVersion reports the varhub build version.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.version})
}

// handleListJobs lists jobs (newest first) with optional ?status= and ?limit/?offset
// paging. It serves both /v1/jobs and /ui/jobs. Listing is scoped to the requester's
// own history (by session cookie, else client IP) UNLESS the request is
// authenticated with a token — an admin then sees every job. This keeps one user
// from browsing another's requests on an open public server.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	switch status {
	case "", StatusQueued, StatusRunning, StatusDone, StatusError:
	default:
		writeJSONError(w, http.StatusBadRequest, "invalid status filter")
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	if limit > 500 {
		limit = 500
	}
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)

	f := JobFilter{Status: status}
	scoped := !s.authed(r)
	if scoped {
		if sess := s.sessionID(r); sess != "" {
			f.Session = sess
		} else {
			f.ClientIP = clientIP(r, s.trusted)
		}
	}
	jobs, err := s.queue.List(r.Context(), f, limit, offset)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []Job{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "limit": limit, "offset": offset, "scoped": scoped})
}

// atoiDefault parses s as an int, falling back to def on empty/invalid input.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// --- job status + results --------------------------------------------------

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	job, ok, err := s.queue.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown job id")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleJobResults(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// ?wait= long-polls up to the (capped) duration for the job to finish, so a
	// poller can block rather than spin.
	job, ok, err := s.queue.WaitFor(r.Context(), id, s.parseWait(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown job id")
		return
	}
	switch job.Status {
	case StatusDone:
		result, ok, err := s.queue.Result(r.Context(), id)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			writeJSONError(w, http.StatusInternalServerError, "job done but result missing")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result)
	case StatusError:
		writeJSONError(w, http.StatusUnprocessableEntity, "job failed: "+job.Error)
	default:
		writeJSONError(w, http.StatusConflict, "job not finished (status: "+job.Status+")")
	}
}
