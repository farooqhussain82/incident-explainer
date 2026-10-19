package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

func (m *Manager) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", m.handleList)
	mux.HandleFunc("GET /incidents/{id}", m.handleDetail)
	mux.HandleFunc("POST /incidents/{id}/explain", m.handleReexplain)
	mux.HandleFunc("GET /api/incidents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.store.List())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	return mux
}

func (m *Manager) handleList(w http.ResponseWriter, r *http.Request) {
	var open, resolved []Incident
	for _, inc := range m.store.List() {
		if inc.Status == "open" {
			open = append(open, inc)
		} else if len(resolved) < 25 {
			resolved = append(resolved, inc)
		}
	}
	render(w, listTmpl, map[string]any{"Open": open, "Resolved": resolved, "Model": m.llm.Model})
}

func (m *Manager) handleDetail(w http.ResponseWriter, r *http.Request) {
	inc := m.store.Get(r.PathValue("id"))
	if inc == nil {
		http.NotFound(w, r)
		return
	}
	render(w, detailTmpl, map[string]any{"I": inc, "Model": m.llm.Model})
}

func (m *Manager) handleReexplain(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if m.store.Get(id) == nil {
		http.NotFound(w, r)
		return
	}
	m.store.Do(func() {
		i := m.store.all[id]
		i.ExplainState = "pending"
		i.Log("Explanation requested again from the web page")
	})
	m.enqueue(id)
	http.Redirect(w, r, "/incidents/"+id, http.StatusSeeOther)
}

func render(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "page", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

var funcs = template.FuncMap{
	"ago": func(t time.Time) string {
		d := time.Since(t).Round(time.Second)
		switch {
		case d < time.Minute:
			return fmt.Sprintf("%ds ago", int(d.Seconds()))
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		}
		return t.Format("2 Jan 15:04")
	},
	"clock":  func(t time.Time) string { return t.Local().Format("15:04:05") },
	"counts": CountsString,
	"lower":  strings.ToLower,
	"dur": func(a time.Time, b *time.Time) string {
		if b == nil {
			return ""
		}
		return b.Sub(a).Round(time.Second).String()
	},
}

const layout = `{{define "page"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Incident Explainer</title>
{{block "refresh" .}}{{end}}
<style>
:root{--bg:#f6f7f9;--card:#fff;--ink:#16181d;--muted:#5d6472;--line:#e3e6eb;--red:#c62f2f;--amber:#b56b00;--green:#1f7a4d;--blue:#2457c5;--code:#f0f2f5}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--ink:#e8eaee;--muted:#9aa3b2;--line:#2a2f3a;--red:#ff6b6b;--amber:#ffb84d;--green:#4fd18b;--blue:#7aa2ff;--code:#11141a}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif}
header{display:flex;align-items:center;gap:12px;padding:14px 20px;border-bottom:1px solid var(--line);background:var(--card)}
header a{color:inherit;text-decoration:none;font-weight:650}header .tag{margin-left:auto;color:var(--muted);font-size:13px}
main{max-width:1000px;margin:0 auto;padding:20px 16px 60px}
h2{font-size:15px;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);margin:28px 0 10px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px;margin-bottom:10px}
a.card{display:block;color:inherit;text-decoration:none}a.card:hover{border-color:var(--blue)}
.row{display:flex;gap:10px;align-items:baseline;flex-wrap:wrap}.id{font-family:ui-monospace,Menlo,monospace;color:var(--muted);font-size:13px}
.pod{font-weight:600}.when{margin-left:auto;color:var(--muted);font-size:13px}
.badge{font-size:12px;font-weight:600;padding:2px 8px;border-radius:99px;border:1px solid currentColor}
.open{color:var(--red)}.resolved{color:var(--green)}.pending,.running{color:var(--amber)}.failed{color:var(--red)}
.summary{margin:8px 0 0;color:var(--ink)}.muted{color:var(--muted)}
.empty{color:var(--muted);padding:24px;text-align:center;border:1px dashed var(--line);border-radius:10px}
code,pre{font-family:ui-monospace,Menlo,monospace;font-size:13px}pre{background:var(--code);border:1px solid var(--line);border-radius:8px;padding:12px;overflow:auto;white-space:pre-wrap;word-break:break-word}
blockquote{margin:6px 0;padding:6px 10px;border-left:3px solid var(--amber);background:var(--code);font-family:ui-monospace,Menlo,monospace;font-size:13px;white-space:pre-wrap;word-break:break-word}
ol{padding-left:22px}li{margin:4px 0}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px}.stat .k{font-size:12px;color:var(--muted)}.stat .v{font-weight:600}
button{font:inherit;padding:6px 12px;border-radius:8px;border:1px solid var(--line);background:var(--card);color:var(--ink);cursor:pointer}button:hover{border-color:var(--blue)}
details summary{cursor:pointer;color:var(--blue)}
.tl{list-style:none;padding:0}.tl li{display:flex;gap:10px}.tl .t{color:var(--muted);font-family:ui-monospace,Menlo,monospace;font-size:13px;min-width:70px}
</style></head><body>
<header><a href="/">🧯 Incident Explainer</a><span class="tag">local model: {{.Model}} · nothing leaves the cluster</span></header>
<main>{{template "body" .}}</main></body></html>{{end}}`

var listTmpl = template.Must(template.Must(template.New("l").Funcs(funcs).Parse(layout)).Parse(`
{{define "refresh"}}<meta http-equiv="refresh" content="10">{{end}}
{{define "body"}}
<h2>Open ({{len .Open}})</h2>
{{range .Open}}{{template "item" .}}{{else}}<div class="empty">No open incidents. Everything looks healthy.</div>{{end}}
<h2>Recently resolved</h2>
{{range .Resolved}}{{template "item" .}}{{else}}<div class="empty">Nothing resolved yet.</div>{{end}}
{{end}}
{{define "item"}}<a class="card" href="/incidents/{{.ID}}">
<div class="row"><span class="id">{{.ID}}</span><span class="pod">{{.Namespace}}/{{.Pod}}</span>
<span class="badge {{.Status}}">{{.Reason}}</span>
{{if ne .ExplainState "done"}}<span class="badge {{.ExplainState}}">{{if eq .ExplainState "failed"}}explain failed{{else}}explaining…{{end}}</span>{{end}}
<span class="when">{{with .Resolved}}resolved {{ago .}}{{end}}{{if .Resolved}} · lasted {{dur .Opened .Resolved}}{{else}}opened {{ago .Opened}}{{end}}</span></div>
{{with .Explanation}}<p class="summary">{{.Summary}}</p>{{else}}<p class="summary muted">{{.Message}}</p>{{end}}
</a>{{end}}`))

var detailTmpl = template.Must(template.Must(template.New("d").Funcs(funcs).Parse(layout)).Parse(`
{{define "refresh"}}{{if or (eq .I.ExplainState "pending") (eq .I.ExplainState "running")}}<meta http-equiv="refresh" content="5">{{end}}{{end}}
{{define "body"}}{{with .I}}
<div class="row" style="margin-top:6px"><span class="id">{{.ID}}</span><h1 style="margin:0;font-size:22px">{{.Namespace}}/{{.Pod}}</h1>
<span class="badge {{.Status}}">{{.Status}}</span></div>
<div class="grid" style="margin-top:14px">
<div class="card stat"><div class="k">Reason</div><div class="v">{{.Reason}}</div></div>
<div class="card stat"><div class="k">Container</div><div class="v">{{.Container}}</div></div>
<div class="card stat"><div class="k">Restarts</div><div class="v">{{.Restarts}}</div></div>
<div class="card stat"><div class="k">Opened</div><div class="v">{{ago .Opened}}</div></div>
</div>

{{if eq .ExplainState "done"}}{{with .Explanation}}
<h2>What happened <span class="badge {{lower .Confidence}}" style="text-transform:none">confidence: {{.Confidence}}</span></h2>
<div class="card"><p style="margin:0;font-size:17px">{{.Summary}}</p></div>
<h2>Likely cause</h2><div class="card">{{.LikelyCause}}</div>
{{if .Evidence}}<h2>Evidence from the logs and events</h2><div class="card">{{range .Evidence}}<blockquote>{{.}}</blockquote>{{end}}</div>{{end}}
{{if .NextSteps}}<h2>Check next</h2><div class="card"><ol>{{range .NextSteps}}<li>{{.}}</li>{{end}}</ol></div>{{end}}
<p class="muted">Explained by <code>{{.Model}}</code> in {{.Seconds}}s · {{.PromptTokens}} tokens in, {{.OutputTokens}} out · cost: $0</p>
{{end}}{{else if eq .ExplainState "failed"}}
<h2>Explanation</h2><div class="card"><span class="failed">Failed:</span> {{.ExplainError}}</div>
{{else}}<h2>Explanation</h2><div class="card muted">The local model is reading the logs… this page refreshes by itself.</div>{{end}}

<form method="post" action="/incidents/{{.ID}}/explain" style="margin:14px 0"><button>Explain again</button></form>

<h2>Timeline</h2><div class="card"><ul class="tl">{{range .Timeline}}<li><span class="t">{{clock .At}}</span><span>{{.Text}}</span></li>{{end}}</ul></div>

{{if .SentText}}<h2>Privacy</h2><div class="card">
<p style="margin-top:0">Redacted before the model saw it: <b>{{counts .Redactions}}</b>. Env var values and Secrets are never read.</p>
<details><summary>Show exactly what the model saw</summary><pre>{{.SentText}}</pre></details></div>{{end}}
{{end}}{{end}}`))
