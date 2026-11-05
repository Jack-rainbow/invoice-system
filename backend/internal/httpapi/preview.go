package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/preview"
)

func (a *AdminAPI) previewsReady(w http.ResponseWriter, r *http.Request) bool {
	if a.previews == nil {
		a.fail(w, r, 503, "DEPENDENCY_UNAVAILABLE")
		return false
	}
	return true
}

// createPreview queues the watermarked preview for the revision the client
// just saved. Repeated identical content merges server-side.
func (a *AdminAPI) createPreview(w http.ResponseWriter, r *http.Request) {
	if !a.previewsReady(w, r) {
		return
	}
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	var c struct {
		ExpectedRevision int32 `json:"expected_revision"`
	}
	raw, ok := a.signedBody(w, r, &c)
	if !ok {
		return
	}
	i, ok := a.signedPrincipal(w, r, raw)
	if !ok {
		return
	}
	result, e := a.previews.Create(r.Context(), i.Principal.ActorUserID, id, c.ExpectedRevision)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 202, result)
}

func (a *AdminAPI) getPreview(w http.ResponseWriter, r *http.Request) {
	if !a.previewsReady(w, r) {
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
	result, e := a.previews.Status(r.Context(), i.Principal.ActorUserID, id)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}

// getPreviewImage streams only the server-composited watermarked JPEG. The
// clean base page has no route and never leaves the renderer process memory.
func (a *AdminAPI) getPreviewImage(w http.ResponseWriter, r *http.Request) {
	if !a.previewsReady(w, r) {
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
	image, e := a.previews.Image(r.Context(), i.Principal.ActorUserID, id)
	if e != nil {
		if errors.Is(e, preview.ErrNotFound) {
			a.fail(w, r, 404, "NOT_FOUND")
			return
		}
		a.error(w, r, e)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", "private, no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(image)))
	w.WriteHeader(200)
	_, _ = w.Write(image)
}
