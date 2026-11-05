package httpapi

import (
	"net/http"
	"strconv"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/credit"
	db "invoice.local/system/backend/internal/platform/postgres/db"
)

// ---- device pairing (candidate browser, pre-session) ----

// confirmPairing completes the PAIR proof: the candidate key is recorded as a
// proven pairing an administrator may later designate.
func (a *AdminAPI) confirmPairing(w http.ResponseWriter, r *http.Request) {
	var c struct {
		ChallengeID string `json:"challenge_id"`
		PublicJWK   string `json:"public_jwk"`
	}
	raw, ok := a.signedBody(w, r, &c)
	if !ok {
		return
	}
	result, e := a.service.ConfirmPairing(r.Context(), cookie(r, preCookie), c.ChallengeID, c.PublicJWK, r, raw)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 201, result)
}

// getTransfer returns the caller's own pending transfer state only.
func (a *AdminAPI) getTransfer(w http.ResponseWriter, r *http.Request) {
	id, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	view, e := a.service.DeviceBindingView(r.Context(), i.Principal, i.Principal.ActorUserID)
	if e != nil {
		a.error(w, r, e)
		return
	}
	if view.PendingTransfer == nil || view.PendingTransfer.ID != account.IDText(id) {
		a.fail(w, r, 404, "NOT_FOUND")
		return
	}
	write(w, 200, view.PendingTransfer)
}

// ---- administrator operations (recent MFA inside the service transaction) ----

func (a *AdminAPI) userStatus(w http.ResponseWriter, r *http.Request) {
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	target, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	var c struct {
		IsActive bool   `json:"is_active"`
		Reason   string `json:"reason"`
	}
	raw, ok := a.signedBody(w, r, &c)
	if !ok {
		return
	}
	_ = raw
	if e := a.service.SetUserStatus(r.Context(), i.Principal, target, c.IsActive, c.Reason); e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, struct {
		IsActive bool `json:"is_active"`
	}{c.IsActive})
}

func (a *AdminAPI) passwordReset(w http.ResponseWriter, r *http.Request) {
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	target, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	var c struct {
		Reason string `json:"reason"`
	}
	if !a.body(w, r, &c) {
		return
	}
	result, e := a.service.IssuePasswordReset(r.Context(), i.Principal, target, c.Reason)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 201, result)
}

func (a *AdminAPI) deviceBindingView(w http.ResponseWriter, r *http.Request) {
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	target, e := account.ParseID(r.PathValue("id"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	view, e := a.service.DeviceBindingView(r.Context(), i.Principal, target)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, view)
}

func (a *AdminAPI) deviceReset(w http.ResponseWriter, r *http.Request) {
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	target, e := account.ParseID(r.PathValue("id"))
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
		ExpectedEpoch   int32  `json:"expected_binding_epoch"`
		PairingID       string `json:"pairing_id"`
		TargetKeyHash   string `json:"target_key_hash"`
		VerificationRef string `json:"verification_ref"`
		Reason          string `json:"reason"`
	}
	if !a.body(w, r, &c) {
		return
	}
	pairing, e := account.ParseID(c.PairingID)
	if e != nil || c.ExpectedEpoch < 1 || c.TargetKeyHash == "" {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	result, e := a.service.ApproveDeviceReset(r.Context(), i.Principal, target, account.TransferApprovalCommand{
		ExpectedEpoch: c.ExpectedEpoch, PairingID: pairing, TargetKeyHash: c.TargetKeyHash,
		VerificationRef: c.VerificationRef, Reason: c.Reason}, key)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 201, result)
}

func (a *AdminAPI) cancelTransfer(w http.ResponseWriter, r *http.Request) {
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	target, e := account.ParseID(r.PathValue("id"))
	transfer, e2 := account.ParseID(r.PathValue("transfer_id"))
	if e != nil || e2 != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	var c struct {
		ExpectedEpoch int32  `json:"expected_binding_epoch"`
		Reason        string `json:"reason"`
	}
	if !a.body(w, r, &c) {
		return
	}
	if e := a.service.CancelTransfer(r.Context(), i.Principal, target, transfer, c.ExpectedEpoch, c.Reason); e != nil {
		a.error(w, r, e)
		return
	}
	w.WriteHeader(204)
}

// ---- global ledger and audit reads (complete MFA session) ----

func (a *AdminAPI) adminCreditEntries(w http.ResponseWriter, r *http.Request) {
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	values, ok := parseListQuery(r, "cursor", "limit", "user_id")
	if !ok {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	limit, e := parseLimit(values.Get("limit"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	// Authorization: only complete MFA administrators read the global ledger.
	pool := a.service.Pool()
	if e = credit.RequireAdministrator(r.Context(), db.New(pool), i.Principal); e != nil {
		a.error(w, r, e)
		return
	}
	params := db.AdminCreditEntriesPageParams{PageLimit: limit}
	if values.Get("user_id") != "" {
		user, e := account.ParseID(values.Get("user_id"))
		if e != nil {
			a.fail(w, r, 400, "INVALID_REQUEST")
			return
		}
		params.UserFilter = user
	}
	rows, e := db.New(pool).AdminCreditEntriesPage(r.Context(), params)
	if e != nil {
		a.error(w, r, e)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"id": account.IDText(row.ID), "username": row.Username, "kind": row.Kind,
			"delta_available": strconv.FormatInt(row.DeltaAvailable, 10),
			"delta_frozen":    strconv.FormatInt(row.DeltaFrozen, 10),
			"after_available": strconv.FormatInt(row.AfterAvailable, 10),
			"after_frozen":    strconv.FormatInt(row.AfterFrozen, 10),
			"reason":          row.Reason, "created_at": row.CreatedAt.Time.UTC(),
		})
	}
	write(w, 200, struct {
		Items []map[string]any `json:"items"`
	}{items})
}

func (a *AdminAPI) adminAuditLogs(w http.ResponseWriter, r *http.Request) {
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	values, ok := parseListQuery(r, "cursor", "limit", "action")
	if !ok {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	limit, e := parseLimit(values.Get("limit"))
	if e != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	pool := a.service.Pool()
	if e = credit.RequireAdministrator(r.Context(), db.New(pool), i.Principal); e != nil {
		a.error(w, r, e)
		return
	}
	rows, e := db.New(pool).AdminAuditPage(r.Context(), db.AdminAuditPageParams{
		ActionFilter: values.Get("action"), PageLimit: limit})
	if e != nil {
		a.error(w, r, e)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"id": account.IDText(row.ID), "actor": row.Actor, "action": row.Action,
			"target_type": row.TargetType, "target_id": account.IDText(row.TargetID),
			"created_at": row.CreatedAt.Time.UTC(),
		})
	}
	write(w, 200, struct {
		Items []map[string]any `json:"items"`
	}{items})
}

func parseLimit(raw string) (int32, error) {
	if raw == "" {
		return 20, nil
	}
	n, e := strconv.ParseInt(raw, 10, 32)
	if e != nil || n < 1 || n > 100 || strconv.FormatInt(n, 10) != raw {
		return 0, account.ErrInvalid
	}
	return int32(n), nil
}
