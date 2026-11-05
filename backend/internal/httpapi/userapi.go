package httpapi

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5/pgtype"
	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/credit"
	"invoice.local/system/backend/internal/document"
)

// parseListQuery validates the closed query parameter set shared by cursor
// lists: only the named keys, each exactly once.
func parseListQuery(r *http.Request, keys ...string) (url.Values, bool) {
	values, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return nil, false
	}
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	for key, vs := range values {
		if len(vs) != 1 || !allowed[key] {
			return nil, false
		}
	}
	return values, true
}

// ---- product presets ----

func (a *AdminAPI) createPreset(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
		return
	}
	var c struct {
		Name   string          `json:"name"`
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
	result, e := a.documents.SavePreset(r.Context(), i.Principal.ActorUserID, pgtype.UUID{}, c.Name, c.Fields)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 201, result)
}

func (a *AdminAPI) updatePreset(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
		return
	}
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	var c struct {
		Name   string          `json:"name"`
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
	result, e := a.documents.SavePreset(r.Context(), i.Principal.ActorUserID, id, c.Name, c.Fields)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}

func (a *AdminAPI) deletePreset(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
		return
	}
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	raw, ok := a.signedBody(w, r, &struct{}{})
	if !ok {
		return
	}
	i, ok := a.signedPrincipal(w, r, raw)
	if !ok {
		return
	}
	if e := a.documents.DeletePreset(r.Context(), i.Principal.ActorUserID, id); e != nil {
		a.error(w, r, e)
		return
	}
	w.WriteHeader(204)
}

func (a *AdminAPI) listPresets(w http.ResponseWriter, r *http.Request) {
	if !a.documentsReady(w, r) {
		return
	}
	values, ok := parseListQuery(r, "cursor", "limit")
	if !ok {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	limit, e := document.ParseLimit(values.Get("limit"))
	if e != nil || len(values.Get("cursor")) > 4096 {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	page, e := a.documents.ListPresets(r.Context(), i.Principal.ActorUserID, values.Get("cursor"), limit)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, page)
}

// ---- own wallet entries ----

func (a *AdminAPI) creditEntries(w http.ResponseWriter, r *http.Request) {
	values, ok := parseListQuery(r, "cursor", "limit")
	if !ok {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	limit, e := document.ParseLimit(values.Get("limit"))
	if e != nil || len(values.Get("cursor")) > 4096 {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	page, e := a.credits.Entries(r.Context(), i.Principal.ActorUserID, values.Get("cursor"), limit)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, page)
}

// ---- idempotent operation lookup ----

// lookupOperation recovers the outcome of one of the caller's own ambiguous
// operations. It never creates work: the same idempotency key is echoed back.
func (a *AdminAPI) lookupOperation(w http.ResponseWriter, r *http.Request) {
	var c struct {
		Operation      string `json:"operation"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	raw, ok := a.signedBody(w, r, &c)
	if !ok {
		return
	}
	key, e := account.ParseID(c.IdempotencyKey)
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	i, ok := a.signedPrincipal(w, r, raw)
	if !ok {
		return
	}
	outcome, e := a.credits.LookupOperation(r.Context(), i.Principal.ActorUserID, c.Operation, key)
	if e != nil {
		if errors.Is(e, credit.ErrNotFound) {
			a.fail(w, r, 404, "NOT_FOUND")
			return
		}
		a.error(w, r, e)
		return
	}
	resource := any(nil)
	if outcome.ResourceID != nil {
		resource = *outcome.ResourceID
	}
	response := struct {
		OperationID string `json:"operation_id"`
		Operation   string `json:"operation"`
		Status      string `json:"status"`
		ResourceID  any    `json:"resource_id"`
		Generation  any    `json:"generation,omitempty"`
	}{OperationID: outcome.OperationID, Operation: outcome.Operation, Status: outcome.Status, ResourceID: resource}
	// A completed GENERATE resolves to the live order state so an ambiguous
	// timeout can resume polling without minting a new key.
	if outcome.Operation == "GENERATE" && outcome.ResourceID != nil {
		orderID, e := account.ParseID(*outcome.ResourceID)
		if e == nil && a.generations != nil {
			if state, e := a.generations.Status(r.Context(), i.Principal.ActorUserID, orderID); e == nil {
				response.Generation = state
			}
		}
	}
	write(w, 200, response)
}
