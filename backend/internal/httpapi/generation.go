package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/generation"
)

func (a *AdminAPI) generationsReady(w http.ResponseWriter, r *http.Request) bool {
	if a.generations == nil {
		a.fail(w, r, 503, "DEPENDENCY_UNAVAILABLE")
		return false
	}
	return true
}

// createGeneration admits one paid formal version. The client must echo the
// confirmed preview manifest and the cost it agreed to; a mismatch costs
// nothing and returns a conflict the UI must resolve.
func (a *AdminAPI) createGeneration(w http.ResponseWriter, r *http.Request) {
	if !a.generationsReady(w, r) {
		return
	}
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	key, e := account.ParseID(r.Header.Get("Idempotency-Key"))
	if e != nil || len(r.Header.Values("Idempotency-Key")) != 1 {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	var c struct {
		ExpectedRevision        int32  `json:"expected_revision"`
		AcceptedPreviewManifest string `json:"accepted_preview_manifest_id"`
		AcceptedCostPoints      string `json:"accepted_cost_points"`
	}
	raw, ok := a.signedBody(w, r, &c)
	if !ok {
		return
	}
	manifestID, e := account.ParseID(c.AcceptedPreviewManifest)
	if e != nil || c.ExpectedRevision < 1 {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	cost, e := strconv.ParseInt(c.AcceptedCostPoints, 10, 64)
	if e != nil || cost < 0 || cost > 1 || strconv.FormatInt(cost, 10) != c.AcceptedCostPoints {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	i, ok := a.signedPrincipal(w, r, raw)
	if !ok {
		return
	}
	result, e := a.generations.Create(r.Context(), i.Principal, generation.Command{
		DocumentID: id, ExpectedRevision: c.ExpectedRevision, ManifestID: manifestID,
		AcceptedCost: cost, IdempotencyKey: key})
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 202, result)
}

func (a *AdminAPI) getGeneration(w http.ResponseWriter, r *http.Request) {
	if !a.generationsReady(w, r) {
		return
	}
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	result, e := a.generations.Status(r.Context(), i.Principal.ActorUserID, id)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}

func (a *AdminAPI) listVersions(w http.ResponseWriter, r *http.Request) {
	if !a.generationsReady(w, r) {
		return
	}
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	items, e := a.generations.Versions(r.Context(), i.Principal.ActorUserID, id)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, struct {
		Items []generation.VersionSummary `json:"items"`
	}{items})
}

// downloadVersion streams one formal format of a delivered version. Only the
// settled entitlement matters: the balance is never consulted and repeats are
// free.
func (a *AdminAPI) downloadVersion(w http.ResponseWriter, r *http.Request) {
	if !a.generationsReady(w, r) {
		return
	}
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	values, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || len(values) != 1 || len(values["format"]) != 1 || (values.Get("format") != "pdf" && values.Get("format") != "png") {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	file, e := a.generations.Download(r.Context(), i.Principal.ActorUserID, id, values.Get("format"))
	if e != nil {
		if errors.Is(e, generation.ErrPending) {
			w.Header().Set("Retry-After", "10")
			a.fail(w, r, 503, "RESULT_PENDING")
			return
		}
		a.error(w, r, e)
		return
	}
	h := w.Header()
	h.Set("Content-Type", file.ContentType)
	h.Set("Content-Length", strconv.Itoa(len(file.Data)))
	h.Set("Content-Disposition", `attachment; filename="`+file.Filename+`"`)
	h.Set("Cache-Control", "private, no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)
	_, _ = w.Write(file.Data)
}
