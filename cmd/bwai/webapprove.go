package main

import (
	"crypto/subtle"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// The web approval page is reachable from the sandbox: bwai launches
// bwrap without --unshare-net, so the agent shares the host network
// namespace and can connect to the loopback port the broker binds. The
// per-request token is therefore the *only* thing that authorizes a web
// decision, and it never crosses into the sandbox — it travels solely in
// the URL embedded in the host-side desktop notification. Mutations are
// POST-only so a link prefetch or GET can't approve anything.

var (
	errNoSuchRequest = errors.New("no such request")
	errBadToken      = errors.New("bad token")
	errBadDecision   = errors.New("invalid decision")
)

// approvalHandler builds the loopback approval server's routes. It's a
// method returning an http.Handler so tests can exercise it with
// httptest without binding a port.
func (b *Broker) approvalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/r/", b.handleRequestPage)
	return mux
}

// handleRequestPage serves one pending request: GET renders the page,
// POST records a decision. Anything else is 405. (Built for Go 1.21, so
// the id is parsed from the path rather than via 1.22 mux wildcards.)
func (b *Broker) handleRequestPage(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/r/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		b.renderRequestPage(w, r, id)
	case http.MethodPost:
		b.handleRequestDecision(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// renderRequestPage shows the single-request approval view. Unknown id →
// 404; known id with a missing/wrong token → 403. Knowing an id exists
// is useless without the 128-bit token, which gates every mutation.
func (b *Broker) renderRequestPage(w http.ResponseWriter, r *http.Request, id string) {
	token := r.URL.Query().Get("k")

	b.mu.Lock()
	p := b.pending[id]
	var data approvalPageData
	if p != nil {
		data = approvalPageData{
			ID:      p.id,
			Token:   p.token,
			Cmd:     strings.Join(p.req.Argv, " "),
			Project: b.projectDir,
			Cwd:     p.req.Cwd,
			AgeS:    int(time.Since(p.enqueued).Seconds()),
		}
	}
	tokenOK := p != nil && token != "" &&
		subtle.ConstantTimeCompare([]byte(p.token), []byte(token)) == 1
	b.mu.Unlock()

	if p == nil {
		http.NotFound(w, r)
		return
	}
	if !tokenOK {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := approvalPageTmpl.Execute(w, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// handleRequestDecision applies a POSTed decision and maps the result to
// a status code: 404 unknown id, 403 bad token, 400 bad decision.
func (b *Broker) handleRequestDecision(w http.ResponseWriter, r *http.Request, id string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	token := r.PostFormValue("k")
	decision := r.PostFormValue("decision")

	switch err := b.resolveByToken(id, token, decision); {
	case err == nil:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = resultPageTmpl.Execute(w, decisionVerb(decision))
	case errors.Is(err, errNoSuchRequest):
		http.NotFound(w, r)
	case errors.Is(err, errBadToken):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, errBadDecision):
		http.Error(w, "invalid decision", http.StatusBadRequest)
	default:
		http.Error(w, "error", http.StatusInternalServerError)
	}
}

// resolveByToken validates the token against the pending request and, on
// match, delivers the decision. Lookup order is id (404) → token (403) →
// decision (400) so an unauthorized caller learns nothing extra. The
// constant-time compare avoids leaking the token byte-by-byte. Single
// use is inherent: resolve() is sync.Once-guarded and awaitApproval
// deletes the entry once it returns, so a replay gets errNoSuchRequest.
func (b *Broker) resolveByToken(id, token, decision string) error {
	b.mu.Lock()
	p := b.pending[id]
	b.mu.Unlock()
	if p == nil {
		return errNoSuchRequest
	}
	if token == "" || subtle.ConstantTimeCompare([]byte(p.token), []byte(token)) != 1 {
		return errBadToken
	}
	if decision != "approve" && decision != "deny" && decision != "always" {
		return errBadDecision
	}
	p.resolve(decision)
	return nil
}

// decisionVerb renders a decision as a past-tense word for the result
// page.
func decisionVerb(decision string) string {
	switch decision {
	case "approve":
		return "approved"
	case "deny":
		return "denied"
	case "always":
		return "approved for this session"
	default:
		return decision
	}
}

type approvalPageData struct {
	ID      string
	Token   string
	Cmd     string
	Project string
	Cwd     string
	AgeS    int
}

var approvalPageTmpl = template.Must(template.New("approval").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>bwai · approval needed</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 15px/1.5 system-ui, sans-serif; margin: 0; padding: 2rem 1rem;
         display: flex; justify-content: center; background: #f4f4f5; }
  @media (prefers-color-scheme: dark) { body { background: #18181b; } }
  .card { background: Canvas; color: CanvasText; max-width: 36rem; width: 100%;
          border: 1px solid #8884; border-radius: 12px; padding: 1.5rem 1.75rem;
          box-shadow: 0 1px 3px #0002; }
  h1 { font-size: 1.1rem; margin: 0 0 1rem; }
  dl { display: grid; grid-template-columns: max-content 1fr; gap: .35rem .9rem; margin: 0 0 1.5rem; }
  dt { color: #8889; font-weight: 600; }
  dd { margin: 0; word-break: break-word; }
  code { font: 13px/1.5 ui-monospace, monospace; background: #8881; padding: .1rem .35rem; border-radius: 5px; }
  .cmd { display: block; padding: .6rem .75rem; }
  .actions { display: flex; gap: .6rem; flex-wrap: wrap; }
  button { font: inherit; font-weight: 600; padding: .55rem 1.1rem; border-radius: 8px;
           border: 1px solid #8884; cursor: pointer; }
  .approve { background: #16a34a; color: #fff; border-color: #15803d; }
  .deny    { background: #dc2626; color: #fff; border-color: #b91c1c; }
  .always  { background: transparent; color: CanvasText; }
</style>
</head>
<body>
<form class="card" method="post" action="/r/{{.ID}}">
  <h1>The sandbox wants to run a command on the host</h1>
  <dl>
    <dt>command</dt><dd><code class="cmd">{{.Cmd}}</code></dd>
    {{if .Project}}<dt>project</dt><dd><code>{{.Project}}</code></dd>{{end}}
    <dt>cwd</dt><dd><code>{{.Cwd}}</code></dd>
    <dt>age</dt><dd>{{.AgeS}}s</dd>
  </dl>
  <input type="hidden" name="k" value="{{.Token}}">
  <div class="actions">
    <button class="approve" name="decision" value="approve">Approve</button>
    <button class="deny" name="decision" value="deny">Deny</button>
    <button class="always" name="decision" value="always">Always this session</button>
  </div>
</form>
</body>
</html>
`))

var resultPageTmpl = template.Must(template.New("result").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>bwai · {{.}}</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 15px/1.5 system-ui, sans-serif; margin: 0; padding: 3rem 1rem;
         text-align: center; }
  p { font-size: 1.1rem; }
</style>
</head>
<body>
<p><strong>{{.}}.</strong></p>
<p>You can close this tab.</p>
</body>
</html>
`))
