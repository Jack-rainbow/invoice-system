package httpapi

import (
	"errors"
	"mime"
	"net/http"
	"strings"

	"invoice.local/system/backend/internal/strictjson"
)

var ErrBodyTooLarge = strictjson.ErrTooLarge
var ErrInvalidJSON = strictjson.ErrInvalid
var ErrUnsupportedMediaType = errors.New("application/json with UTF-8 encoding required")

func DecodeJSON(r *http.Request, dst any, limit int64) error {
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return ErrUnsupportedMediaType
	}
	if charset, ok := params["charset"]; ok && !strings.EqualFold(charset, "utf-8") {
		return ErrUnsupportedMediaType
	}
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return ErrUnsupportedMediaType
	}
	if r.Body == nil {
		return ErrInvalidJSON
	}
	return strictjson.Decode(r.Body, dst, limit)
}
