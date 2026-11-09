package internal

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// _maxCurationBody bounds POST /admin/curation; bulk imports can be large.
const _maxCurationBody = 16 << 20

// NewAdminMux serves the curation API. Every request needs
// "Authorization: Bearer <token>".
func NewAdminMux(store *Store, token string) http.Handler {
	a := &admin{store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/authors", a.searchAuthors)
	mux.HandleFunc("GET /admin/authors/{id}", a.author)
	mux.HandleFunc("GET /admin/authors/{id}/preview", a.preview)
	mux.HandleFunc("GET /admin/works", a.searchWorks)
	mux.HandleFunc("GET /admin/works/{id}", a.work)
	mux.HandleFunc("GET /admin/editions", a.editions)
	mux.HandleFunc("GET /admin/log", a.log)
	mux.HandleFunc("POST /admin/curation", a.curate)
	mux.HandleFunc("POST /admin/rules/run", a.runRules)
	return requireToken(token, mux)
}

type admin struct {
	store *Store
}

func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		// Constant time so response timing doesn't leak the token.
		if token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			adminError(w, statusErr(http.StatusUnauthorized))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *admin) searchAuthors(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", 20, 200)
	if err != nil {
		adminError(w, err)
		return
	}
	res, err := a.store.SearchAuthors(r.Context(), strings.TrimSpace(r.FormValue("q")), int(limit))
	adminJSON(w, res, err)
}

func (a *admin) author(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		adminError(w, err)
		return
	}
	f := AuthorWorksFilter{Query: strings.TrimSpace(r.FormValue("q"))}
	switch st := r.FormValue("status"); st {
	case "", "canon", "hidden", "none", "served", "excluded":
		f.Status = st
	default:
		adminError(w, badRequest("status must be canon, hidden, none, served or excluded"))
		return
	}
	limit, err := intParam(r, "limit", 100, 5000)
	if err != nil {
		adminError(w, err)
		return
	}
	offset, err := intParam(r, "offset", 0, 1<<31)
	if err != nil {
		adminError(w, err)
		return
	}
	f.Limit, f.Offset = int(limit), int(offset)
	res, err := a.store.AuthorWorks(r.Context(), id, f)
	adminJSON(w, res, err)
}

func (a *admin) preview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		adminError(w, err)
		return
	}
	mode := r.FormValue("mode")
	if mode != "" && mode != "rules" && mode != "allowlist" {
		adminError(w, badRequest("mode must be rules or allowlist"))
		return
	}
	res, err := a.store.PreviewAuthor(r.Context(), id, mode, r.FormValue("rules") == "1")
	adminJSON(w, res, err)
}

func (a *admin) searchWorks(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", 50, 500)
	if err != nil {
		adminError(w, err)
		return
	}
	author, err := intParam(r, "author", 0, 1<<62)
	if err != nil {
		adminError(w, err)
		return
	}
	q := strings.TrimSpace(r.FormValue("q"))
	if q == "" && author == 0 {
		adminError(w, badRequest(`"q" or "author" is required`))
		return
	}
	res, err := a.store.SearchWorks(r.Context(), q, author, int(limit))
	adminJSON(w, res, err)
}

func (a *admin) work(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		adminError(w, err)
		return
	}
	res, err := a.store.Work(r.Context(), id)
	adminJSON(w, res, err)
}

func (a *admin) editions(w http.ResponseWriter, r *http.Request) {
	isbn, asin := strings.TrimSpace(r.FormValue("isbn")), strings.TrimSpace(r.FormValue("asin"))
	if (isbn == "") == (asin == "") {
		adminError(w, badRequest(`exactly one of "isbn" or "asin" is required`))
		return
	}
	res, err := a.store.EditionsByCode(r.Context(), isbn, asin)
	adminJSON(w, res, err)
}

func (a *admin) log(w http.ResponseWriter, r *http.Request) {
	kind := r.FormValue("kind")
	switch kind {
	case "", "author", "work", "edition":
	default:
		adminError(w, badRequest("kind must be author, work or edition"))
		return
	}
	id, err := intParam(r, "id", 0, 1<<62)
	if err != nil {
		adminError(w, err)
		return
	}
	limit, err := intParam(r, "limit", 50, 1000)
	if err != nil {
		adminError(w, err)
		return
	}
	res, err := a.store.CurationLog(r.Context(), kind, id, int(limit))
	adminJSON(w, res, err)
}

func (a *admin) runRules(w http.ResponseWriter, r *http.Request) {
	res, err := a.store.RunRules(r.Context())
	if err == nil {
		Log(r.Context()).Info("ran rules", "works", res.Works, "junk", res.Junk,
			"foreign", res.Foreign, "sibling", res.Sibling, "comic", res.Comic, "editions", res.Editions, "took", res.Took)
	}
	adminJSON(w, res, err)
}

func (a *admin) curate(w http.ResponseWriter, r *http.Request) {
	var req CurationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, _maxCurationBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		adminError(w, errors.Join(errBadRequest, err))
		return
	}
	if strings.TrimSpace(req.Source) == "" {
		adminError(w, badRequest(`"source" is required`))
		return
	}
	res, err := a.store.ApplyCuration(r.Context(), req)
	if err == nil && !res.DryRun {
		Log(r.Context()).Info("applied curation", "source", req.Source,
			"authors", res.Authors, "works", res.Works, "editions", res.Editions)
	}
	adminJSON(w, res, err)
}

// intParam parses an optional non-negative query parameter, capped at max.
func intParam(r *http.Request, name string, def, maxVal int64) (int64, error) {
	v := r.FormValue(name)
	if v == "" {
		return def, nil
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil || i < 0 {
		return 0, badRequest("%s must be a non-negative integer", name)
	}
	return min(i, maxVal), nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, badRequest("id must be a positive integer")
	}
	return id, nil
}

func adminJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		adminError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// adminError writes {"error": "..."} with the wrapped statusErr's code, or
// 500. Internal errors still include their text: the API is LAN-only.
func adminError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var s statusErr
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		status = http.StatusRequestEntityTooLarge
	case errors.As(err, &s):
		status = s.Status()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
