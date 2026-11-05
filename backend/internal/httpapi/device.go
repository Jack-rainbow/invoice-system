package httpapi

import (
	"bytes"
	"invoice.local/system/backend/internal/account"
	"io"
	"net/http"
)

// Read once, cap before the RFC library and preserve the exact signed bytes.
func (a *AdminAPI) signedBody(w http.ResponseWriter, r *http.Request, v any) ([]byte, bool) {
	if r.Body == nil {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return nil, false
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	if e != nil || len(raw) > 64<<10 {
		a.fail(w, r, 400, "INVALID_REQUEST")
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if !a.body(w, r, v) {
		return nil, false
	}
	return raw, true
}
func (a *AdminAPI) deviceChallenge(w http.ResponseWriter, r *http.Request) {
	var c account.DeviceChallengeCommand
	if !a.body(w, r, &c) {
		return
	}
	result, e := a.service.DeviceChallenge(r.Context(), cookie(r, preCookie), cookie(r, sessionCookie), c)
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}
func (a *AdminAPI) initialPassword(w http.ResponseWriter, r *http.Request) {
	var c account.InitialDevicePasswordCommand
	raw, ok := a.signedBody(w, r, &c)
	if !ok {
		return
	}
	result, token, e := a.service.SetInitialDevicePassword(r.Context(), c, r, raw)
	if e != nil {
		a.error(w, r, e)
		return
	}
	setCookie(w, preCookie, token.Reveal(), result.ExpiresAt)
	write(w, 200, result)
}
func (a *AdminAPI) bindDevice(w http.ResponseWriter, r *http.Request)  { a.completeDevice(w, r, true) }
func (a *AdminAPI) loginDevice(w http.ResponseWriter, r *http.Request) { a.completeDevice(w, r, false) }
func (a *AdminAPI) completeDevice(w http.ResponseWriter, r *http.Request, bind bool) {
	if bind {
		var c struct {
			TransferID  string `json:"transfer_id"`
			ChallengeID string `json:"challenge_id"`
		}
		raw, ok := a.signedBody(w, r, &c)
		if !ok {
			return
		}
		a.finishDevice(w, r, c.TransferID, c.ChallengeID, raw, true)
	} else {
		var c struct {
			ChallengeID string `json:"challenge_id"`
		}
		raw, ok := a.signedBody(w, r, &c)
		if !ok {
			return
		}
		a.finishDevice(w, r, "", c.ChallengeID, raw, false)
	}
}
func (a *AdminAPI) finishDevice(w http.ResponseWriter, r *http.Request, transferID, challengeID string, raw []byte, bind bool) {
	result, token, e := a.service.CompleteDeviceProof(r.Context(), cookie(r, preCookie), transferID, challengeID, r, raw, bind)
	if e != nil {
		a.error(w, r, e)
		return
	}
	clearCookie(w, preCookie)
	setCookie(w, sessionCookie, token.Reveal(), result.ExpiresAt)
	write(w, 200, result)
}
func (a *AdminAPI) deviceContext(w http.ResponseWriter, r *http.Request) {
	result, e := a.service.DeviceContext(r.Context(), cookie(r, sessionCookie))
	if e != nil {
		a.error(w, r, e)
		return
	}
	write(w, 200, result)
}
