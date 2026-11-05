// Package render rasterizes the private invoice template into pixel images.
// Template and font bytes are compiled into the binary: they are deployment
// assets, never served to clients, and their SHA-256 hashes are recorded in
// the ACTIVE render profile manifest so a byte change is a new profile.
package render

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"golang.org/x/image/font/opentype"

	"invoice.local/system/backend/internal/strictjson"
)

//go:embed assets/invoice-mxn-v1
var assetFS embed.FS

const assetDir = "assets/invoice-mxn-v1"

// ErrAssets reports a missing or malformed private template asset.
var ErrAssets = errors.New("render assets unavailable")

// Template is the fixed layout contract of the invoice bundle. The JSON file
// is authoritative: every static text, color, box and size lives here, so any
// layout change is a new content hash and therefore a new render profile.
type Template struct {
	TemplateVersion  string `json:"template_version"`
	RendererProtocol string `json:"renderer_protocol"`
	Canvas           struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"canvas"`
	Colors struct {
		Background string `json:"background"`
		Ink        string `json:"ink"`
		Rose       string `json:"rose"`
		Seal       string `json:"seal"`
		Foot       string `json:"foot"`
		Rule       string `json:"rule"`
	} `json:"colors"`
	Fonts struct {
		Regular   string `json:"regular"`
		Bold      string `json:"bold"`
		Watermark string `json:"watermark"`
	} `json:"fonts"`
	Layout struct {
		Rules   [][4]float64         `json:"rules"`
		QR      QRBlock              `json:"qr"`
		Fields  map[string]*FieldBox `json:"fields"`
		Autofit struct {
			MinSize float64 `json:"min_size"`
		} `json:"autofit"`
	} `json:"layout"`
	Runs   []Run `json:"runs"`
	Labels struct {
		Tax    string `json:"tax"`
		Footer string `json:"footer"`
	} `json:"labels"`
}

// QRBlock places the neutral demonstration QR graphic.
type QRBlock struct {
	X    int `json:"x"`
	Y    int `json:"y"`
	Size int `json:"size"`
}

// FieldBox is one user-input slot: fixed position, size and width budget.
type FieldBox struct {
	X        float64 `json:"x"`
	BL       float64 `json:"bl"`
	Size     float64 `json:"size"`
	MaxW     float64 `json:"max_w"`
	Align    string  `json:"align,omitempty"`
	Bold     bool    `json:"bold,omitempty"`
	LineH    float64 `json:"line_h,omitempty"`
	MaxLines int     `json:"max_lines,omitempty"`
	Group    string  `json:"group,omitempty"`
}

// Run is one static text element; Parts renders mixed bold label + value.
type Run struct {
	Text  string    `json:"t,omitempty"`
	Parts []RunPart `json:"parts,omitempty"`
	X     float64   `json:"x"`
	BL    float64   `json:"bl"`
	S     float64   `json:"s"`
	Bold  bool      `json:"b,omitempty"`
	Color string    `json:"c,omitempty"`
	Align string    `json:"align,omitempty"`
	LsFit float64   `json:"lsFit,omitempty"`
	Group string    `json:"group,omitempty"`
}

// RunPart is one bold-or-regular segment of a static run.
type RunPart struct {
	T string `json:"t"`
	B bool   `json:"b,omitempty"`
}

// Assets is the parsed, render-ready form of the private template bundle.
type Assets struct {
	Template  Template
	regular   *opentype.Font
	bold      *opentype.Font
	watermark *opentype.Font
	hashes    map[string]string
}

func readAsset(name string) ([]byte, error) {
	raw, err := assetFS.ReadFile(assetDir + "/" + name)
	if err != nil || len(raw) == 0 {
		return nil, ErrAssets
	}
	return raw, nil
}

func parseFont(name string) (*opentype.Font, error) {
	raw, err := readAsset(name)
	if err != nil {
		return nil, err
	}
	font, err := opentype.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, ErrAssets)
	}
	return font, nil
}

// requiredFields lists every dynamic box the renderer must find in the
// template; a template missing one is refused rather than silently blank.
var requiredFields = []string{
	"recipient_name", "product_code", "quantity", "unit_code", "unit_description",
	"description", "issued_at", "certification_display_at", "unit_price", "line_amount",
	"subtotal", "tax_label", "tax_amount", "total", "total_words", "tax_detail",
}

// Load parses the embedded template bundle once. Failure means the binary was
// not built with usable assets and every render must refuse.
func Load() (*Assets, error) {
	raw, err := readAsset("template.json")
	if err != nil {
		return nil, err
	}
	var t Template
	if err := strictjson.Decode(bytes.NewReader(raw), &t, 64<<10); err != nil {
		return nil, ErrAssets
	}
	if t.RendererProtocol != "goimage-raster/1" || t.Canvas.Width < 512 || t.Canvas.Height < 512 ||
		t.Labels.Tax == "" || len(t.Runs) == 0 || len(t.Layout.Fields) < len(requiredFields) {
		return nil, ErrAssets
	}
	for _, name := range requiredFields {
		box, ok := t.Layout.Fields[name]
		if !ok || box == nil || box.Size < 8 || box.MaxW <= 0 || box.X < 0 || box.BL < 0 ||
			int(box.X) >= t.Canvas.Width || int(box.BL) >= t.Canvas.Height {
			return nil, ErrAssets
		}
	}
	for i := range t.Runs {
		r := &t.Runs[i]
		if r.S < 8 || (r.Text == "" && len(r.Parts) == 0) || r.BL < 0 || r.X < 0 ||
			int(r.X) >= t.Canvas.Width || int(r.BL) >= t.Canvas.Height {
			return nil, ErrAssets
		}
	}
	regular, err := parseFont(t.Fonts.Regular)
	if err != nil {
		return nil, err
	}
	bold, err := parseFont(t.Fonts.Bold)
	if err != nil {
		return nil, err
	}
	watermark, err := parseFont(t.Fonts.Watermark)
	if err != nil {
		return nil, err
	}
	a := &Assets{Template: t, regular: regular, bold: bold, watermark: watermark, hashes: map[string]string{}}
	for _, name := range []string{"template.json", t.Fonts.Regular, t.Fonts.Bold, t.Fonts.Watermark} {
		raw, err := readAsset(name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		a.hashes[name] = hex.EncodeToString(sum[:])
	}
	return a, nil
}

// AssetHashes maps each asset file name to its SHA-256 hex digest. The ACTIVE
// render profile manifest records exactly these values.
func (a *Assets) AssetHashes() map[string]string {
	out := make(map[string]string, len(a.hashes))
	for k, v := range a.hashes {
		out[k] = v
	}
	return out
}

// ManifestAssets is the canonical assets JSON object recorded in the render
// profile manifest. Keys are sorted so the encoding is deterministic.
func (a *Assets) ManifestAssets() json.RawMessage {
	names := make([]string, 0, len(a.hashes))
	for name := range a.hashes {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	buf.WriteString(`{"bundle":"invoice-mxn-v1","sha256":{`)
	for i, name := range names {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(name)
		val, _ := json.Marshal(a.hashes[name])
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteString("}}")
	return json.RawMessage(buf.Bytes())
}

// Box is a field's on-page rectangle as fractions of the canvas, ready for
// overlay positioning on a client-sized preview image.
type Box struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// FieldBoxes reports every dynamic field's page rectangle in canvas
// fractions. Baselines convert to a top edge at roughly cap height; multi-line
// boxes span their whole line budget.
func (a *Assets) FieldBoxes() map[string]Box {
	t := a.Template
	out := make(map[string]Box, len(t.Layout.Fields))
	for name, box := range t.Layout.Fields {
		if box == nil {
			continue
		}
		lines := box.MaxLines
		if lines < 1 {
			lines = 1
		}
		height := box.Size * 1.25
		if lines > 1 && box.LineH > 0 {
			height = box.LineH*float64(lines-1) + box.Size*1.25
		}
		out[name] = Box{
			X: box.X / float64(t.Canvas.Width),
			Y: (box.BL - box.Size*0.78) / float64(t.Canvas.Height),
			W: box.MaxW / float64(t.Canvas.Width),
			H: height / float64(t.Canvas.Height),
		}
	}
	return out
}
