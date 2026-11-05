package httpapi

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONRejectsDuplicateUnknownAndTrailingData(t *testing.T) {
	type input struct {
		ExpectedRevision int `json:"expected_revision"`
		Fields           struct {
			Name string `json:"name"`
		} `json:"fields"`
	}
	for _, body := range []string{
		`{"expected_revision":1,"expected_revision":2}`,
		`{"expected_revision":1,"EXPECTED_REVISION":2}`,
		`{"Expected_Revision":1}`,
		"\u00a0{\"expected_revision\":1}",
		"{\"expected_revision\":1}\u00a0",
		"\v{\"expected_revision\":1}",
		"{\"expected_revision\":1}\f",
		`{"fields":{"name":"a","name":"b"}}`,
		`{"expected_revision":1,"owner_id":"someone-else"}`,
		`{"expected_revision":1} {"expected_revision":2}`,
		`null`, `[]`, `1`,
		"{\"fields\":{\"name\":\"\xff\"}}",
		strings.Repeat(`{"fields":`, 40) + `{}` + strings.Repeat(`}`, 40),
	} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		var value input
		if err := DecodeJSON(r, &value, 4096); err == nil {
			t.Fatalf("invalid JSON accepted: %.100s", body)
		}
	}
}

func TestDecodeJSONAcceptsOnlyStandardBoundaryWhitespace(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(" \t\r\n{\"value\":\"ok\"}\n\r\t "))
	r.Header.Set("Content-Type", "application/json")
	var input struct {
		Value string `json:"value"`
	}
	if err := DecodeJSON(r, &input, 1024); err != nil || input.Value != "ok" {
		t.Fatalf("valid JSON whitespace rejected: %v %+v", err, input)
	}
}

func TestDecodeJSONPreservesExactValuesAndBoundsBody(t *testing.T) {
	type input struct {
		Price    string `json:"price"`
		Quantity string `json:"quantity"`
	}
	body := `{"price":"100.00","quantity":"0.000001"}`
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	var value input
	if err := DecodeJSON(r, &value, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if value.Price != "100.00" || value.Quantity != "0.000001" {
		t.Fatalf("unexpected values: %+v", value)
	}
	r = httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if err := DecodeJSON(r, &value, int64(len(body)-1)); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("size error=%v", err)
	}
	r = httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "text/plain")
	if err := DecodeJSON(r, &value, 4096); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("media error=%v", err)
	}
	r = httptest.NewRequest("POST", "/", io.NopCloser(strings.NewReader(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Encoding", "gzip")
	if err := DecodeJSON(r, &value, 4096); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("encoding error=%v", err)
	}
}
