package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to one VariantHub server as one token.
type Client struct {
	Server string
	Token  string
	HTTP   *http.Client
}

// New builds a client from credentials.
//
// No overall timeout on the http.Client, deliberately. It would apply to the
// whole exchange including the body, so it is a cap on *transfer size* dressed
// up as a cap on latency: a genome's annotated VCF takes as long as it takes,
// and a client that cut it off at thirty seconds would fail on exactly the jobs
// worth running. Each request carries its own deadline through the context
// instead, and the ones that stream carry none.
func New(c Credentials) *Client {
	return &Client{
		Server: strings.TrimRight(c.Server, "/"),
		Token:  c.Token,
		HTTP:   &http.Client{CheckRedirect: dropTokenOffHost},
	}
}

// dropTokenOffHost removes the credential when a redirect leaves the server.
//
// format=vcf answers 302 with a link straight to object storage, so following
// redirects is the point rather than an edge case — and the hop lands on a host
// this program has no relationship with. The presigned URL carries its own
// authority in its query string, so it needs nothing from us; forwarding a
// VariantHub bearer token there would hand a storage provider, a CDN, or
// whoever a misconfigured endpoint actually resolves to a credential for the
// caller's whole account.
//
// Go's own rule strips sensitive headers across a domain change and would
// usually cover this. Usually is the problem: it compares domains, so a
// redirect to the same host on another port keeps the header — which is what a
// gateway beside the API looks like, and is exactly the deployment where the
// storage endpoint is least likely to be somebody we trust with a token. A test
// against two loopback ports found the header being forwarded, which is how
// this came to be written rather than assumed.
//
// So the rule here is ours and it is narrower: the token survives only a
// redirect that stays on the same scheme, host and port.
func dropTokenOffHost(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		req.Header.Del("Authorization")
	}
	return nil
}

// Job is what a server reports about a submission.
//
// A subset: the fields a command-line caller acts on. Decoding into a partial
// struct rather than a map keeps the names in one place and makes a server that
// renames one a compile-time conversation rather than a silent empty column.
type Job struct {
	JobID     string `json:"job_id"`
	Kind      string `json:"kind"`
	Snapshot  string `json:"snapshot"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	NVariants int64  `json:"n_variants"`
	Label     string `json:"label,omitempty"`

	CreatedAt  int64 `json:"created_at"`
	StartedAt  int64 `json:"started_at,omitempty"`
	FinishedAt int64 `json:"finished_at,omitempty"`

	Total  int `json:"chunks_total"`
	Done   int `json:"chunks_done"`
	Failed int `json:"chunks_failed"`

	// PurgedAt tells "this produced nothing" apart from "the results have
	// expired", which otherwise look identical from here: both report no
	// variants and no result to fetch.
	PurgedAt int64  `json:"purged_at,omitempty"`
	Runner   string `json:"runner,omitempty"`
}

// Terminal reports whether the job has stopped changing.
func (j Job) Terminal() bool {
	return j.Status == "done" || j.Status == "error" || j.Status == "cancelled"
}

// SubmitRequest is a submission: what to annotate and against what.
type SubmitRequest struct {
	// Exactly one of these. Variants is a locus list; VCFPath is a file.
	Variants []string
	VCFPath  string

	Snapshot    string
	Sources     []string
	Build       string
	Annotations string // "" | "all" | "a,b"
}

// Submit sends a job and returns its id.
func (c *Client) Submit(ctx context.Context, req SubmitRequest) (string, error) {
	if req.VCFPath != "" {
		return c.submitVCF(ctx, req)
	}
	return c.submitLoci(ctx, req)
}

func (c *Client) submitLoci(ctx context.Context, req SubmitRequest) (string, error) {
	body := map[string]any{"variants": req.Variants}
	if req.Snapshot != "" {
		body["snapshot"] = req.Snapshot
	}
	if len(req.Sources) > 0 {
		body["sources"] = req.Sources
	}
	if req.Build != "" {
		body["build"] = req.Build
	}
	if req.Annotations != "" {
		body["annotations"] = req.Annotations
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, "POST", "/api/v1/annotate", "application/json",
		strings.NewReader(string(b)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return acceptedID(resp)
}

// submitVCF uploads a file as multipart, streaming it.
//
// Through an io.Pipe rather than a buffer: the point of a VCF submission is
// that it is large, and building the request in memory first would put a cap on
// what this can send that has nothing to do with what the server accepts.
func (c *Client) submitVCF(ctx context.Context, req SubmitRequest) (string, error) {
	f, err := os.Open(req.VCFPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		// Fields before the file. The server reads the parts in order as they
		// arrive, so a field sent after the file is one it has already had to
		// decide without.
		for name, v := range map[string]string{
			"snapshot":    req.Snapshot,
			"build":       req.Build,
			"annotations": req.Annotations,
			"sources":     strings.Join(req.Sources, ","),
		} {
			if v == "" {
				continue
			}
			if err := mw.WriteField(name, v); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		part, err := mw.CreateFormFile("vcf", filepath.Base(req.VCFPath))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, f); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(mw.Close())
	}()

	resp, err := c.do(ctx, "POST", "/api/v1/annotate/vcf", mw.FormDataContentType(), pr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return acceptedID(resp)
}

func acceptedID(resp *http.Response) (string, error) {
	var out struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("the server accepted the job but its reply could not be read: %w", err)
	}
	if out.JobID == "" {
		return "", fmt.Errorf("the server accepted the job without naming it")
	}
	return out.JobID, nil
}

// Status reports on one job.
func (c *Client) Status(ctx context.Context, id string) (Job, error) {
	resp, err := c.do(ctx, "GET", "/api/v1/jobs/"+url.PathEscape(id), "", nil)
	if err != nil {
		return Job{}, err
	}
	defer resp.Body.Close()
	var j Job
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return Job{}, err
	}
	return j, nil
}

// Fetch streams a finished job's results in one of the four formats.
//
// The redirect is the interesting part. format=vcf may answer 302 with a link
// straight to object storage, and Go's client follows it — dropping the
// Authorization header on the way, because the hop crosses to another host.
// That is exactly right and worth stating: the presigned URL carries its own
// authority in the query string, and forwarding a VariantHub bearer token to a
// storage provider would hand a third party a credential for this account.
func (c *Client) Fetch(ctx context.Context, id, format string, w io.Writer) (int64, error) {
	path := "/api/v1/jobs/" + url.PathEscape(id) + "/export"
	if format != "" {
		path += "?format=" + url.QueryEscape(format)
	}
	resp, err := c.do(ctx, "GET", path, "", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return io.Copy(w, resp.Body)
}

// Delete removes a job's stored input and results, and takes it out of the
// caller's listing. The record that it ran is kept by the server.
func (c *Client) Delete(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "DELETE", "/api/v1/jobs/"+url.PathEscape(id), "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Ping checks the server answers and the token is accepted.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := c.do(ctx, "GET", "/api/v1/ping", "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// do issues a request and turns a failure status into an error worth reading.
func (c *Client) do(ctx context.Context, method, path, contentType string,
	body io.Reader) (*http.Response, error) {

	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, c.Server+path, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, statusError(method, c.Server+path, resp)
	}
	return resp, nil
}

// statusError reads the server's own explanation, which it promises is safe to
// show. Falling back to the status line when there is not one: a bare "400" is
// unhelpful, and inventing a cause would be worse.
func statusError(method, url string, resp *http.Response) error {
	var out struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = json.Unmarshal(b, &out)

	msg := out.Error
	if msg != "" && out.Detail != "" {
		msg += ": " + out.Detail
	}
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	if msg == "" {
		msg = resp.Status
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("the server did not accept the token (%s). "+
			"Run `varhub login` with a current one", msg)
	case http.StatusNotFound:
		// A job someone else owns answers 404 too, which is deliberate on the
		// server's side — saying "not yours" confirms the id exists.
		return fmt.Errorf("%s %s: not found (%s)", method, url, msg)
	}
	return fmt.Errorf("%s %s: %s", method, url, msg)
}
