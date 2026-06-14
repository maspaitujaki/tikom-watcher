// Package mocksite serves a tiket.com-like event page on localhost whose
// availability can be flipped at runtime, for end-to-end testing of the
// render -> detect -> notify pipeline without waiting for a real ticket drop.
//
// The rendered DOM mirrors the bits detection cares about: a
// [data-testid="product-card"] section and a primary <button> that is either
// disabled "Terjual habis" (sold out) or enabled "Beli tiket sekarang"
// (available). A <span>"Terjual habis"</span> category badge is present in BOTH
// states, so a passing AVAILABLE result also proves the page-wide "Habis" trap
// is correctly ignored.
package mocksite

import (
	"html/template"
	"net/http"
	"sort"
	"strings"
	"sync"
)

const (
	StateSoldOut   = "sold_out"
	StateAvailable = "available"
)

// Server holds per-slug availability state in memory.
type Server struct {
	mu     sync.Mutex
	states map[string]string
}

// New returns a Server, seeding the given slugs as SOLD_OUT.
func New(seedSlugs ...string) *Server {
	s := &Server{states: map[string]string{}}
	for _, sl := range seedSlugs {
		s.states[sl] = StateSoldOut
	}
	return s
}

// State returns the slug's state (SOLD_OUT if unknown).
func (s *Server) State(slug string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.states[slug]; ok {
		return st
	}
	return StateSoldOut
}

// Set forces a slug's state (anything other than "available" is sold_out).
func (s *Server) Set(slug, state string) {
	if state != StateAvailable {
		state = StateSoldOut
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[slug] = state
}

func (s *Server) ensure(slug string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.states[slug]; !ok {
		s.states[slug] = StateSoldOut
	}
}

func (s *Server) snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.states))
	for k, v := range s.states {
		out[k] = v
	}
	return out
}

// Handler returns the HTTP routes: event pages, a flip endpoint, and an admin panel.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/to-do/", s.handleEvent)
	mux.HandleFunc("/set", s.handleSet)
	mux.HandleFunc("/admin", s.handleAdmin)
	mux.HandleFunc("/", s.handleAdmin)
	return mux
}

type eventData struct {
	Name    string
	State   string
	SoldOut bool
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/to-do/"), "/")
	if slug == "" {
		http.NotFound(w, r)
		return
	}
	s.ensure(slug)
	st := s.State(slug)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = eventTmpl.Execute(w, eventData{
		Name:    titleize(slug),
		State:   st,
		SoldOut: st != StateAvailable,
	})
}

func (s *Server) handleSet(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("slug")
	state := r.URL.Query().Get("state")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}
	s.Set(slug, state)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

type adminRow struct {
	Slug, State, EventURL string
	Available             bool
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	slugs := make([]string, 0, len(snap))
	for k := range snap {
		slugs = append(slugs, k)
	}
	sort.Strings(slugs)

	scheme := "http://"
	rows := make([]adminRow, 0, len(slugs))
	for _, sl := range slugs {
		rows = append(rows, adminRow{
			Slug:      sl,
			State:     snap[sl],
			Available: snap[sl] == StateAvailable,
			EventURL:  scheme + r.Host + "/to-do/" + sl,
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = adminTmpl.Execute(w, rows)
}

func titleize(slug string) string {
	words := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return "MOCK: " + strings.Join(words, " ")
}

var eventTmpl = template.Must(template.New("event").Parse(`<!doctype html>
<html lang="id"><head><meta charset="utf-8"><title>{{.Name}}</title></head>
<body>
  <h1>{{.Name}}</h1>
  <p>Mock tiket.com event — current state: <strong>{{.State}}</strong></p>
  <section data-testid="product-card">
    <div class="ticket-category">
      <span class="cat-name">VIP</span>
      <span class="cat-status">Terjual habis</span>
    </div>
    <div class="ticket-category">
      <span class="cat-name">CAT 1</span>
      <span class="price">Rp 1.500.000</span>
    </div>
    {{if .SoldOut}}
    <button class="Button_variant_primary__mock" disabled><span>Terjual habis</span></button>
    {{else}}
    <button class="Button_variant_primary__mock"><span>Beli tiket sekarang</span></button>
    {{end}}
  </section>
</body></html>
`))

var adminTmpl = template.Must(template.New("admin").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>mocksite admin</title>
<style>body{font-family:sans-serif;margin:2rem}td,th{padding:.4rem .8rem;text-align:left}
.so{color:#b00}.av{color:#080}a.btn{display:inline-block;padding:.3rem .6rem;border:1px solid #888;border-radius:4px;text-decoration:none;margin-right:.4rem}</style>
</head><body>
<h1>mocksite</h1>
<table border="1" cellspacing="0">
<tr><th>event</th><th>state</th><th>page</th><th>flip</th></tr>
{{range .}}
<tr>
  <td>{{.Slug}}</td>
  <td class="{{if .Available}}av{{else}}so{{end}}">{{.State}}</td>
  <td><a href="{{.EventURL}}">{{.EventURL}}</a></td>
  <td>
    <a class="btn" href="/set?slug={{.Slug}}&state=available">→ AVAILABLE</a>
    <a class="btn" href="/set?slug={{.Slug}}&state=sold_out">→ SOLD_OUT</a>
  </td>
</tr>
{{end}}
</table>
<p>Hit a <code>/to-do/&lt;slug&gt;</code> URL once to register a new event, then flip it here.</p>
</body></html>
`))
