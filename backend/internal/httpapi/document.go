package httpapi

import (
	"errors"
	"net/http"
	"net/url"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/document"
)

// signedPrincipal resolves the caller and, for a USER session, verifies the
// RFC9421 proof over the exact bytes that were read from the request body.
// A body-sensitive signature must never be checked against a re-encoded copy.
func (a *AdminAPI) signedPrincipal(w http.ResponseWriter, r *http.Request, raw []byte) (account.Identity, bool) {
	i, e := a.service.Resolve(r.Context(), cookie(r, sessionCookie))
	if errors.Is(e, account.ErrDeviceSession) {
		i, e = a.service.ResolveDevice(r.Context(), cookie(r, sessionCookie), r, raw, false)
	}
	if e != nil {
		a.error(w, r, e)
		return i, false
	}
	return i, true
}

func (a *AdminAPI) documentsReady(w http.ResponseWriter, r *http.Request) bool {
	if a.documents == nil {
		a.fail(w, r, 503, "DEPENDENCY_UNAVAILABLE")
		return false
	}
	return true
}

func (a *AdminAPI) createDocument(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
		return
	}
	var c struct {
		Fields document.Fields `json:"fields"`
	}
	raw, ok := a.signedBody(w, r, &c)
	if !ok {
		return
	}
	i, ok := a.signedPrincipal(w, r, raw)
	if !ok {
		return
	}
	result, e := a.documents.Create(r.Context(), i.Principal.ActorUserID, c.Fields)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 201, result)
}

func (a *AdminAPI) listDocuments(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
		return
	}
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	values, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	for key, vs := range values {
		if len(vs) != 1 || (key != "cursor" && key != "limit" && key != "status") {
			a.fail(w, r, 400, "INVALID_REQUEST")
			return
		}
	}
	limit, e := document.ParseLimit(values.Get("limit"))
	if e != nil || len(values.Get("cursor")) > 4096 {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	result, e := a.documents.List(r.Context(), i.Principal.ActorUserID, document.Filter{Cursor: values.Get("cursor"), Limit: limit, Status: values.Get("status")})
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}

func (a *AdminAPI) getDocument(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
		return
	}
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	result, e := a.documents.Get(r.Context(), i.Principal.ActorUserID, id)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}

func (a *AdminAPI) saveDocument(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
		return
	}
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	var c struct {
		ExpectedRevision int32           `json:"expected_revision"`
		Fields           document.Fields `json:"fields"`
	}
	raw, ok := a.signedBody(w, r, &c)
	if !ok {
		return
	}
	i, ok := a.signedPrincipal(w, r, raw)
	if !ok {
		return
	}
	result, e := a.documents.Save(r.Context(), i.Principal.ActorUserID, id, c.ExpectedRevision, c.Fields)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}

func (a *AdminAPI) deleteDocument(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
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
	if e := a.documents.Delete(r.Context(), i.Principal.ActorUserID, id, c.ExpectedRevision); e != nil {
		a.error(w, r, e)
		return
	}
	w.WriteHeader(204)
}

// listRenderProfiles exposes only selectable metadata: id, name and status.
// Template bytes and storage keys never leave the server.
func (a *AdminAPI) listRenderProfiles(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
		return
	}
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	items, e := a.documents.Profiles(r.Context(), i.Principal.ActorUserID)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, struct {
		Items []document.ProfileSummary `json:"items"`
	}{items})
}

// changeDocumentProfile adopts a new ACTIVE template as a new revision. This is
// the only way an existing draft may leave a revoked profile.
func (a *AdminAPI) changeDocumentProfile(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
		return
	}
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	var c struct {
		ExpectedRevision int32  `json:"expected_revision"`
		TargetProfileID  string `json:"target_profile_id"`
	}
	raw, ok := a.signedBody(w, r, &c)
	if !ok {
		return
	}
	i, ok := a.signedPrincipal(w, r, raw)
	if !ok {
		return
	}
	result, e := a.documents.ChangeProfile(r.Context(), i.Principal.ActorUserID, id, c.ExpectedRevision, c.TargetProfileID)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}
