package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/credit"
	"invoice.local/system/backend/internal/document"
	"invoice.local/system/backend/internal/generation"
	"invoice.local/system/backend/internal/preview"
)

const sessionCookie = "__Host-session"
const preCookie = "__Host-pre-session"
const bootstrapCookie = "__Host-bootstrap"
const anonymousCookie = "__Host-csrf-context"

type AdminAPI struct {
	service     *account.Service
	credits     *credit.Service
	documents   *document.Service
	previews    *preview.Service
	generations *generation.Service
	keys        *account.Keyring
	origin      string
	mux         *http.ServeMux
	slots       chan struct{}
}

func NewAdminAPI(service *account.Service, credits *credit.Service, documents *document.Service, previews *preview.Service, generations *generation.Service, keys *account.Keyring, origin string) *AdminAPI {
	a := &AdminAPI{service: service, credits: credits, documents: documents, previews: previews, generations: generations, keys: keys, origin: origin, mux: http.NewServeMux(), slots: make(chan struct{}, 32)}
	a.mux.HandleFunc("GET /api/v1/auth/csrf", a.csrf)
	a.mux.HandleFunc("POST /api/v1/auth/admin/bootstrap/start", a.bootstrapStart)
	a.mux.HandleFunc("POST /api/v1/auth/admin/bootstrap/confirm", a.bootstrapConfirm)
	a.mux.HandleFunc("POST /api/v1/auth/login", a.login)
	a.mux.HandleFunc("POST /api/v1/auth/mfa/challenges", a.challenge)
	a.mux.HandleFunc("POST /api/v1/auth/mfa/verify", a.verify)
	a.mux.HandleFunc("POST /api/v1/auth/mfa/step-up", a.stepUp)
	a.mux.HandleFunc("POST /api/v1/auth/logout", a.logout)
	a.mux.HandleFunc("POST /api/v1/auth/device/challenges", a.deviceChallenge)
	a.mux.HandleFunc("POST /api/v1/auth/password/set", a.initialPassword)
	a.mux.HandleFunc("POST /api/v1/auth/device/bind", a.bindDevice)
	a.mux.HandleFunc("POST /api/v1/auth/device/verify", a.loginDevice)
	a.mux.HandleFunc("GET /api/v1/auth/device/context", a.deviceContext)
	a.mux.HandleFunc("GET /api/v1/me", a.me)
	a.mux.HandleFunc("POST /api/v1/admin/users", a.createUser)
	a.mux.HandleFunc("GET /api/v1/admin/users", a.users)
	a.mux.HandleFunc("POST /api/v1/admin/users/{id}/credit-grants", a.grant)
	a.mux.HandleFunc("POST /api/v1/documents", a.createDocument)
	a.mux.HandleFunc("GET /api/v1/documents", a.listDocuments)
	a.mux.HandleFunc("GET /api/v1/documents/{id}", a.getDocument)
	a.mux.HandleFunc("PATCH /api/v1/documents/{id}", a.saveDocument)
	a.mux.HandleFunc("DELETE /api/v1/documents/{id}", a.deleteDocument)
	a.mux.HandleFunc("GET /api/v1/render-profiles", a.listRenderProfiles)
	a.mux.HandleFunc("POST /api/v1/documents/{id}/render-profile", a.changeDocumentProfile)
	a.mux.HandleFunc("POST /api/v1/documents/{id}/previews", a.createPreview)
	a.mux.HandleFunc("GET /api/v1/previews/{id}", a.getPreview)
	a.mux.HandleFunc("GET /api/v1/previews/{id}/image", a.getPreviewImage)
	a.mux.HandleFunc("POST /api/v1/documents/{id}/generations", a.createGeneration)
	a.mux.HandleFunc("GET /api/v1/generations/{id}", a.getGeneration)
	a.mux.HandleFunc("GET /api/v1/documents/{id}/versions", a.listVersions)
	a.mux.HandleFunc("GET /api/v1/versions/{id}/download", a.downloadVersion)
	a.mux.HandleFunc("GET /api/v1/presets", a.listPresets)
	a.mux.HandleFunc("POST /api/v1/presets", a.createPreset)
	a.mux.HandleFunc("PATCH /api/v1/presets/{id}", a.updatePreset)
	a.mux.HandleFunc("DELETE /api/v1/presets/{id}", a.deletePreset)
	a.mux.HandleFunc("GET /api/v1/credits/entries", a.creditEntries)
	a.mux.HandleFunc("POST /api/v1/operations/lookup", a.lookupOperation)
	a.mux.HandleFunc("POST /api/v1/auth/device/pairings", a.confirmPairing)
	a.mux.HandleFunc("GET /api/v1/auth/device/transfers/{id}", a.getTransfer)
	a.mux.HandleFunc("PATCH /api/v1/admin/users/{id}/status", a.userStatus)
	a.mux.HandleFunc("POST /api/v1/admin/users/{id}/password-reset", a.passwordReset)
	a.mux.HandleFunc("GET /api/v1/admin/users/{id}/device-binding", a.deviceBindingView)
	a.mux.HandleFunc("POST /api/v1/admin/users/{id}/device-reset", a.deviceReset)
	a.mux.HandleFunc("POST /api/v1/admin/users/{id}/device-transfers/{transfer_id}/cancel", a.cancelTransfer)
	a.mux.HandleFunc("GET /api/v1/admin/credit-entries", a.adminCreditEntries)
	a.mux.HandleFunc("GET /api/v1/admin/audit-logs", a.adminAuditLogs)
	return a
}
func (a *AdminAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.service == nil || a.credits == nil || a.keys == nil {
		a.fail(w, r, 503, "DEPENDENCY_UNAVAILABLE")
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		a.fail(w, r, 429, "RATE_LIMITED")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if path.Clean(r.URL.Path) != r.URL.Path || strings.Contains(r.URL.Path, "//") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	peer, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		peer = r.RemoteAddr
	}
	if e = a.service.Allow(ctx, "http-global", "all", 1200, 60); e != nil {
		a.error(w, r, e)
		return
	}
	// One browser tab performs a proof challenge plus the request for each
	// business action, so a single peer legitimately exceeds 120 requests per
	// minute during sustained editing. The global ceiling still bounds total work.
	if e = a.service.Allow(ctx, "http-peer", peer, 600, 60); e != nil {
		a.error(w, r, e)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		if r.Header.Get("Origin") != a.origin || len(r.Header.Values("Origin")) != 1 {
			a.fail(w, r, 403, "ORIGIN_REJECTED")
			return
		}
		kind, value := contextCookie(r)
		if value == "" || len(r.Header.Values("X-CSRF-Token")) != 1 || !a.keys.VerifyCSRF(r.Header.Get("X-CSRF-Token"), kind, value) {
			a.fail(w, r, 403, "CSRF_INVALID")
			return
		}
	}
	_, pattern := a.mux.Handler(r)
	if pattern == "" {
		a.fail(w, r, 503, "DEPENDENCY_UNAVAILABLE")
		return
	}
	a.mux.ServeHTTP(w, r)
}
func cookie(r *http.Request, name string) string {
	cookies := r.CookiesNamed(name)
	if len(cookies) != 1 {
		return ""
	}
	if _, e := account.ParseToken(cookies[0].Value); e != nil {
		return ""
	}
	return cookies[0].Value
}
func contextCookie(r *http.Request) (string, string) {
	for _, name := range []string{bootstrapCookie, preCookie, sessionCookie, anonymousCookie} {
		if value := cookie(r, name); value != "" {
			return name, value
		}
	}
	return "", ""
}
func setCookie(w http.ResponseWriter, name, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Path: "/", Expires: expires, MaxAge: max(1, int(time.Until(expires).Seconds()))})
}
func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Path: "/", MaxAge: -1})
}
func (a *AdminAPI) csrf(w http.ResponseWriter, r *http.Request) {
	kind, value := contextCookie(r)
	if value == "" {
		t, e := account.NewToken()
		if e != nil {
			a.error(w, r, e)
			return
		}
		kind, value = anonymousCookie, t.Reveal()
		setCookie(w, kind, value, time.Now().Add(30*time.Minute))
	}
	write(w, 200, map[string]string{"csrf_token": a.keys.CSRF(kind, value)})
}
func (a *AdminAPI) body(w http.ResponseWriter, r *http.Request, v any) bool {
	if DecodeJSON(r, v, 64<<10) != nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return false
	}
	return true
}
func (a *AdminAPI) bootstrapStart(w http.ResponseWriter, r *http.Request) {
	var c struct {
		Token    string `json:"bootstrap_token"`
		Password string `json:"new_password"`
	}
	if !a.body(w, r, &c) {
		return
	}
	result, t, e := a.service.BeginBootstrap(r.Context(), c.Token, c.Password)
	if e != nil {
		a.error(w, r, e)
		return
	}
	setCookie(w, bootstrapCookie, t.Reveal(), result.ExpiresAt)
	write(w, 200, result)
}
func (a *AdminAPI) bootstrapConfirm(w http.ResponseWriter, r *http.Request) {
	var c struct {
		Context string `json:"bootstrap_context"`
		Code    string `json:"totp"`
	}
	if !a.body(w, r, &c) {
		return
	}
	result, t, e := a.service.FinishBootstrap(r.Context(), cookie(r, bootstrapCookie), c.Context, c.Code)
	if e != nil {
		a.error(w, r, e)
		return
	}
	clearCookie(w, bootstrapCookie)
	clearCookie(w, preCookie)
	setCookie(w, sessionCookie, t.Reveal(), result.ExpiresAt)
	write(w, 200, result)
}
func (a *AdminAPI) login(w http.ResponseWriter, r *http.Request) {
	var c struct {
		Name     string `json:"username"`
		Password string `json:"password"`
	}
	if !a.body(w, r, &c) {
		return
	}
	result, t, e := a.service.Login(r.Context(), c.Name, c.Password)
	if e != nil {
		a.error(w, r, e)
		return
	}
	setCookie(w, preCookie, t.Reveal(), result.ExpiresAt)
	write(w, 200, result)
}
func (a *AdminAPI) challenge(w http.ResponseWriter, r *http.Request) {
	var c struct {
		Purpose string `json:"purpose"`
	}
	if !a.body(w, r, &c) {
		return
	}
	name := preCookie
	if c.Purpose == "STEP_UP" {
		name = sessionCookie
	}
	result, e := a.service.MFAChallenge(r.Context(), cookie(r, name), c.Purpose)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}
func (a *AdminAPI) verify(w http.ResponseWriter, r *http.Request) {
	var c struct {
		ID       string `json:"challenge_id"`
		Code     string `json:"totp"`
		Recovery string `json:"recovery_code"`
	}
	if !a.body(w, r, &c) {
		return
	}
	result, t, e := a.service.VerifyMFA(r.Context(), cookie(r, preCookie), c.ID, c.Code, c.Recovery, false)
	if e != nil {
		a.error(w, r, e)
		return
	}
	clearCookie(w, preCookie)
	setCookie(w, sessionCookie, t.Reveal(), result.ExpiresAt)
	write(w, 200, result)
}
func (a *AdminAPI) stepUp(w http.ResponseWriter, r *http.Request) {
	var c struct {
		ID   string `json:"challenge_id"`
		Code string `json:"totp"`
	}
	if !a.body(w, r, &c) {
		return
	}
	result, _, e := a.service.VerifyMFA(r.Context(), cookie(r, sessionCookie), c.ID, c.Code, "", true)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, map[string]time.Time{"recent_verified_at": result.VerifiedAt.UTC()})
}
func (a *AdminAPI) principal(w http.ResponseWriter, r *http.Request) (account.Identity, bool) {
	i, e := a.service.Resolve(r.Context(), cookie(r, sessionCookie))
	if errors.Is(e, account.ErrDeviceSession) {
		i, e = a.service.ResolveDevice(r.Context(), cookie(r, sessionCookie), r, nil, false)
	}
	if e != nil {
		a.error(w, r, e)
		return i, false
	}
	return i, true
}
func (a *AdminAPI) me(w http.ResponseWriter, r *http.Request) {
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	write(w, 200, struct {
		User       account.User  `json:"user"`
		Wallet     credit.Wallet `json:"wallet"`
		Generation string        `json:"generation"`
	}{i.User, i.Wallet, account.IDText(i.Principal.Generation)})
}
func (a *AdminAPI) logout(w http.ResponseWriter, r *http.Request) {
	var empty struct{}
	raw, ok := a.signedBody(w, r, &empty)
	if !ok {
		return
	}
	if e := a.service.Logout(r.Context(), cookie(r, sessionCookie)); e != nil {
		if errors.Is(e, account.ErrDeviceSession) {
			_, e = a.service.ResolveDevice(r.Context(), cookie(r, sessionCookie), r, raw, true)
		}
		if e != nil {
			a.error(w, r, e)
			return
		}
	}
	for _, name := range []string{sessionCookie, preCookie, bootstrapCookie} {
		clearCookie(w, name)
	}
	w.WriteHeader(204)
}
func (a *AdminAPI) createUser(w http.ResponseWriter, r *http.Request) {
	i, ok := a.principal(w, r)
	if !ok {
		return
	}
	key, e := account.ParseID(r.Header.Get("Idempotency-Key"))
	if e != nil || len(r.Header.Values("Idempotency-Key")) != 1 {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	var c account.CreateUserCommand
	if !a.body(w, r, &c) {
		return
	}
	result, e := a.service.CreateUser(r.Context(), i.Principal, key, c)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 201, result)
}
func (a *AdminAPI) users(w http.ResponseWriter, r *http.Request) {
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
		if len(vs) != 1 || (key != "cursor" && key != "limit" && key != "username" && key != "status") {
			a.fail(w, r, 400, "INVALID_REQUEST")
			return
		}
	}
	filter := account.UserFilter{Cursor: values.Get("cursor"), Username: values.Get("username"), Status: values.Get("status")}
	if vs, ok := values["limit"]; ok {
		n, err := strconv.ParseInt(vs[0], 10, 32)
		if err != nil || n < 1 || n > 100 || strconv.FormatInt(n, 10) != vs[0] {
			a.fail(w, r, 400, "INVALID_REQUEST")
			return
		}
		filter.Limit = int32(n)
	}
	items, e := a.service.ManagedUsers(r.Context(), i.Principal, filter)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, items)
}
func (a *AdminAPI) grant(w http.ResponseWriter, r *http.Request) {
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
		Amount string `json:"amount"`
		Reason string `json:"reason"`
	}
	if !a.body(w, r, &c) {
		return
	}
	n, e := strconv.ParseInt(c.Amount, 10, 64)
	if e != nil || n <= 0 || strconv.FormatInt(n, 10) != c.Amount {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return
	}
	result, e := a.credits.Grant(r.Context(), i.Principal, credit.Command{TargetUserID: target, Points: n, Reason: c.Reason, IdempotencyKey: key})
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}
func (a *AdminAPI) error(w http.ResponseWriter, r *http.Request, e error) {
	var validation *document.ValidationError
	var conflict *document.ConflictError
	var previewConflict *preview.ConflictError
	var generationConflict *generation.ConflictError
	switch {
	case errors.As(e, &validation):
		code := "VALIDATION_ERROR"
		fields := make([]map[string]string, 0, len(validation.Errors))
		for _, f := range validation.Errors {
			if f.Code == "TEXT_DOES_NOT_FIT" {
				code = "TEXT_DOES_NOT_FIT"
			}
			fields = append(fields, map[string]string{"field": f.Field, "code": f.Code, "message": "字段内容不符合票面要求"})
		}
		a.failDetails(w, r, 400, code, map[string]any{"field_errors": fields})
	case errors.As(e, &conflict):
		details := map[string]any{}
		if conflict.Current > 0 {
			details["current_revision"] = conflict.Current
		}
		a.failDetails(w, r, 409, conflict.Code, details)
	case errors.As(e, &previewConflict):
		details := map[string]any{}
		if previewConflict.Current > 0 {
			details["current_revision"] = previewConflict.Current
		}
		a.failDetails(w, r, 409, previewConflict.Code, details)
	case errors.As(e, &generationConflict):
		details := map[string]any{}
		if generationConflict.Current > 0 {
			details["current_revision"] = generationConflict.Current
		}
		if generationConflict.GenerationID != "" {
			details["generation_id"] = generationConflict.GenerationID
		}
		if generationConflict.VersionID != "" {
			details["version_id"] = generationConflict.VersionID
		}
		a.failDetails(w, r, 409, generationConflict.Code, details)
	case errors.Is(e, generation.ErrPending):
		w.Header().Set("Retry-After", "10")
		a.fail(w, r, 503, "RESULT_PENDING")
	case errors.Is(e, document.ErrNotFound) || errors.Is(e, preview.ErrNotFound) || errors.Is(e, generation.ErrNotFound):
		a.fail(w, r, 404, "NOT_FOUND")
	case errors.Is(e, document.ErrInvalid) || errors.Is(e, preview.ErrInvalid) || errors.Is(e, generation.ErrInvalid):
		a.fail(w, r, 400, "INVALID_REQUEST")
	case errors.Is(e, document.ErrUnavailable) || errors.Is(e, preview.ErrUnavailable) || errors.Is(e, generation.ErrIntegrity):
		a.fail(w, r, 503, "DEPENDENCY_UNAVAILABLE")
	case errors.Is(e, generation.ErrUnauthorized):
		a.fail(w, r, 401, "AUTHENTICATION_REQUIRED")
		a.fail(w, r, 404, "NOT_FOUND")
	case errors.Is(e, document.ErrInvalid) || errors.Is(e, preview.ErrInvalid):
		a.fail(w, r, 400, "INVALID_REQUEST")
	case errors.Is(e, document.ErrUnavailable) || errors.Is(e, preview.ErrUnavailable):
		a.fail(w, r, 503, "DEPENDENCY_UNAVAILABLE")
	case errors.Is(e, account.ErrDeviceProofRequired):
		a.fail(w, r, 403, "DEVICE_PROOF_REQUIRED")
	case errors.Is(e, account.ErrDeviceProof):
		a.fail(w, r, 400, "DEVICE_PROOF_INVALID")
	case errors.Is(e, account.ErrAuth) || errors.Is(e, account.ErrDeviceSession):
		a.fail(w, r, 401, "AUTHENTICATION_REQUIRED")
	case errors.Is(e, credit.ErrUnauthorized):
		a.fail(w, r, 403, "FORBIDDEN")
	case errors.Is(e, account.ErrInvalid) || errors.Is(e, credit.ErrInvalid):
		a.fail(w, r, 400, "INVALID_REQUEST")
	case errors.Is(e, account.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		a.fail(w, r, 429, "RATE_LIMITED")
	case errors.Is(e, account.ErrConflict) || errors.Is(e, credit.ErrConflict) || errors.Is(e, generation.ErrConflict):
		a.fail(w, r, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(e, account.ErrLinkIssued):
		a.fail(w, r, 409, "SETUP_LINK_ALREADY_ISSUED")
	case errors.Is(e, credit.ErrOverflow):
		a.fail(w, r, 400, "AMOUNT_LIMIT")
	case errors.Is(e, credit.ErrOutcomeUnknown) || errors.Is(e, account.ErrOutcomeUnknown):
		w.Header().Set("Retry-After", "60")
		a.fail(w, r, 503, "RESULT_PENDING")
	default:
		a.fail(w, r, 503, "DEPENDENCY_UNAVAILABLE")
	}
}
func (a *AdminAPI) fail(w http.ResponseWriter, r *http.Request, status int, code string) {
	write(w, status, map[string]string{"code": code, "message": "操作未完成，请核对登录状态或稍后重试", "request_id": w.Header().Get("X-Request-ID")})
}
func (a *AdminAPI) failDetails(w http.ResponseWriter, r *http.Request, status int, code string, details map[string]any) {
	write(w, status, map[string]any{"code": code, "message": "操作未完成，请核对登录状态或稍后重试", "request_id": w.Header().Get("X-Request-ID"), "details": details})
}
