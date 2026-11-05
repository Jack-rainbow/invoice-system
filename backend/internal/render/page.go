package render

import (
	"crypto/sha256"
	"errors"
	"image"
	"image/color"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// ErrTextDoesNotFit reports that a validated field cannot fit the fixed
// template boxes at the fixed sizes. It maps to TEXT_DOES_NOT_FIT.
var ErrTextDoesNotFit = errors.New("field text does not fit the template")

// ErrPreviewTooLarge reports a watermarked preview above the 2 MiB limit.
var ErrPreviewTooLarge = errors.New("preview image exceeds byte limit")

// Invoice is the render input: server-normalized fields plus server-computed
// amounts. The renderer never parses or recomputes money itself.
type Invoice struct {
	RecipientName          string
	ProductCode            string
	Quantity               string
	UnitCode               string
	UnitDescription        string
	Description            string
	IssuedAt               string // UTC RFC3339Nano, displayed in America/Mexico_City
	CertificationDisplayAt string // empty when the draft stores JSON null
	UnitPrice              string
	Subtotal               string
	Tax                    string
	Total                  string
	TotalCents             int64
}

func hexColor(s string) color.RGBA {
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	if err != nil || len(s) != 7 {
		return color.RGBA{A: 0xff}
	}
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

func opentypeFace(source *opentype.Font, size float64) (font.Face, error) {
	return opentype.NewFace(source, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
}

func (a *Assets) newFace(kind string, size float64) font.Face {
	source := a.regular
	if kind == "bold" {
		source = a.bold
	}
	if kind == "watermark" {
		source = a.watermark
	}
	face, err := opentypeFace(source, size)
	if err != nil {
		// Font bytes were parsed at Load; a face failure is an asset defect.
		panic(err)
	}
	return face
}

func measure(face font.Face, s string) fixed.Int26_6 {
	return font.MeasureString(face, s)
}

func drawText(dst *image.RGBA, face font.Face, x, baseline int, ink color.Color, s string) {
	d := &font.Drawer{Dst: dst, Src: image.NewUniform(ink), Face: face, Dot: fixed.P(x, baseline)}
	d.DrawString(s)
}

func drawRight(dst *image.RGBA, face font.Face, right, baseline int, ink color.Color, s string) {
	w := measure(face, s).Ceil()
	drawText(dst, face, right-w, baseline, ink, s)
}

// drawSpaced renders text with per-glyph letter spacing (the FACTURA masthead).
func drawSpaced(dst *image.RGBA, face font.Face, x, baseline int, ink color.Color, s string, spacing int) {
	for _, r := range s {
		d := &font.Drawer{Dst: dst, Src: image.NewUniform(ink), Face: face, Dot: fixed.P(x, baseline)}
		d.DrawString(string(r))
		advance, _ := face.GlyphAdvance(r)
		x += advance.Ceil() + spacing
	}
}

// mexicoCity loads the profile timezone; without system tzdata it falls back
// to the fixed post-2022 Mexico City offset so display stays honest.
var mexicoCity = func() *time.Location {
	if loc, err := time.LoadLocation("America/Mexico_City"); err == nil {
		return loc
	}
	return time.FixedZone("America/Mexico_City", -6*3600)
}()

func displayTime(utc string) string {
	parsed, err := time.Parse(time.RFC3339Nano, utc)
	if err != nil {
		return ""
	}
	return parsed.In(mexicoCity).Format("02/01/2006 - 15:04:05")
}

// RenderInvoice rasterizes one server-normalized invoice onto the template
// canvas described by template.json: static runs, separator rules, a neutral
// demonstration QR block and the user's field boxes at fixed sizes. Any box
// overflow fails with ErrTextDoesNotFit; typography floors are part of the
// profile and are never traded for shrinking.
func (a *Assets) RenderInvoice(inv Invoice) (*image.RGBA, error) {
	t := a.Template
	w, h := t.Canvas.Width, t.Canvas.Height
	palette := map[string]color.RGBA{
		"ink": hexColor(t.Colors.Ink), "rose": hexColor(t.Colors.Rose),
		"seal": hexColor(t.Colors.Seal), "foot": hexColor(t.Colors.Foot),
		"rule": hexColor(t.Colors.Rule),
	}
	ink := palette["ink"]

	issued := displayTime(inv.IssuedAt)
	if issued == "" {
		return nil, ErrTextDoesNotFit
	}
	certification := ""
	if inv.CertificationDisplayAt != "" {
		certification = displayTime(inv.CertificationDisplayAt)
		if certification == "" {
			return nil, ErrTextDoesNotFit
		}
	}
	taxDetail := "IVA: 002, Base: $" + inv.Subtotal + ", Tasa: 0.160000, Importe: $" + inv.Tax

	// Wrap multi-line fields first: the "notes" group under the product row
	// shifts down by one line height for every extra description line.
	faceFor := func(box *FieldBox) font.Face {
		if box.Bold {
			return a.newFace("bold", box.Size)
		}
		return a.newFace("regular", box.Size)
	}
	wrap := func(name, text string) []string {
		box := t.Layout.Fields[name]
		face := faceFor(box)
		if box.MaxLines <= 1 {
			if measure(face, text).Ceil() > int(box.MaxW) {
				return nil
			}
			return []string{text}
		}
		return wrapFixed(face, text, int(box.MaxW), box.MaxLines)
	}
	recipientLines := wrap("recipient_name", inv.RecipientName)
	if recipientLines == nil {
		return nil, ErrTextDoesNotFit
	}
	descriptionLines := wrap("description", inv.Description)
	if descriptionLines == nil {
		return nil, ErrTextDoesNotFit
	}
	unitLines := wrap("unit_description", inv.UnitDescription)
	if unitLines == nil {
		return nil, ErrTextDoesNotFit
	}
	wordsLines := wrap("total_words", "("+AmountWords(inv.TotalCents)+")")
	if wordsLines == nil {
		return nil, ErrTextDoesNotFit
	}
	notesShift := (len(descriptionLines) - 1) * int(t.Layout.Fields["description"].LineH+0.5)

	page := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(page, page.Bounds(), image.NewUniform(hexColor(t.Colors.Background)), image.Point{}, draw.Src)

	// Separator rules; the rule flagged with a 4th value moves with the notes.
	ruleInk := palette["rule"]
	for _, line := range t.Layout.Rules {
		x1, y, x2 := int(line[0]), int(line[1]+0.5), int(line[2])
		if line[3] > 0 {
			y += notesShift
		}
		for x := x1; x <= x2; x++ {
			page.SetRGBA(x, y, ruleInk)
			page.SetRGBA(x, y+1, ruleInk)
		}
	}

	// Static runs: every label, fixed metadata line and demonstration seal
	// string lives in the hashed template, so a byte change is a new profile.
	for i := range t.Runs {
		r := &t.Runs[i]
		inkColor := ink
		if r.Color != "" {
			inkColor = palette[r.Color]
		}
		baseline := int(r.BL + 0.5)
		if r.Group == "notes" {
			baseline += notesShift
		}
		if len(r.Parts) > 0 {
			cx := int(r.X + 0.5)
			for _, p := range r.Parts {
				kind := "regular"
				if p.B || r.Bold {
					kind = "bold"
				}
				face := a.newFace(kind, r.S)
				drawText(page, face, cx, baseline, inkColor, p.T)
				cx += measure(face, p.T).Ceil()
			}
			continue
		}
		kind := "regular"
		if r.Bold {
			kind = "bold"
		}
		face := a.newFace(kind, r.S)
		text := r.Text
		width := measure(face, text).Ceil()
		x := int(r.X + 0.5)
		switch r.Align {
		case "r":
			x -= width
		case "c":
			x -= width / 2
		}
		if r.LsFit > 0 && len([]rune(text)) > 1 {
			spacing := (int(r.LsFit) - width) / (len([]rune(text)) - 1)
			drawSpaced(page, face, x, baseline, inkColor, text, spacing)
			continue
		}
		drawText(page, face, x, baseline, inkColor, text)
	}

	// Neutral demonstration QR block: a deterministic pattern with the
	// canonical finder squares. It is decorative and never encodes a claim.
	drawDemoQR(page, t.Layout.QR, ink)

	// Single-line fields autofit like the reference tool: shrink the font to
	// the box, never below the template's floor; past the floor they refuse.
	putLine := func(name, text string) error {
		box := t.Layout.Fields[name]
		if box == nil {
			return ErrTextDoesNotFit
		}
		size := box.Size
		face := faceFor(box)
		if width := measure(face, text).Ceil(); width > int(box.MaxW) {
			floor := t.Layout.Autofit.MinSize
			if floor <= 0 {
				floor = 17
			}
			size = box.Size * box.MaxW / float64(width)
			if size < floor {
				return ErrTextDoesNotFit
			}
			face = a.newFace(map[bool]string{true: "bold", false: "regular"}[box.Bold], size)
		}
		if box.Align == "r" {
			drawRight(page, face, int(box.X+0.5), int(box.BL+0.5), ink, text)
			return nil
		}
		drawText(page, face, int(box.X+0.5), int(box.BL+0.5), ink, text)
		return nil
	}
	putLines := func(name string, lines []string) {
		box := t.Layout.Fields[name]
		face := faceFor(box)
		for i, line := range lines {
			drawText(page, face, int(box.X+0.5), int(box.BL+0.5)+i*int(box.LineH+0.5), ink, line)
		}
	}

	single := [][2]string{
		{"product_code", inv.ProductCode},
		{"quantity", inv.Quantity},
		{"unit_code", inv.UnitCode},
		{"issued_at", issued},
		{"unit_price", "$" + inv.UnitPrice},
		{"line_amount", "$" + inv.Subtotal},
		{"subtotal", "$" + inv.Subtotal},
		{"tax_label", t.Labels.Tax + ":"},
		{"tax_amount", "$" + inv.Tax},
		{"total", "$" + inv.Total},
	}
	for _, kv := range single {
		if err := putLine(kv[0], kv[1]); err != nil {
			return nil, err
		}
	}
	putLines("recipient_name", recipientLines)
	putLines("unit_description", unitLines)
	putLines("description", descriptionLines)
	putLines("total_words", wordsLines)
	detailBox := t.Layout.Fields["tax_detail"]
	detailSize := detailBox.Size
	detailFace := faceFor(detailBox)
	if width := measure(detailFace, taxDetail).Ceil(); width > int(detailBox.MaxW) {
		floor := t.Layout.Autofit.MinSize
		if floor <= 0 {
			floor = 17
		}
		detailSize = detailBox.Size * detailBox.MaxW / float64(width)
		if detailSize < floor {
			return nil, ErrTextDoesNotFit
		}
		detailFace = a.newFace(map[bool]string{true: "bold", false: "regular"}[detailBox.Bold], detailSize)
	}
	drawText(page, detailFace, int(detailBox.X+0.5), int(detailBox.BL+0.5)+notesShift, ink, taxDetail)
	if certification != "" {
		if err := putLine("certification_display_at", certification); err != nil {
			return nil, err
		}
	}
	return page, nil
}

// wrapFixed splits text into at most maxLines lines fitting maxWidth; nil when
// the content cannot fit the budget at the fixed size.
func wrapFixed(face font.Face, text string, maxWidth, maxLines int) []string {
	if text == "" {
		return []string{""}
	}
	fits := func(s string) bool { return measure(face, s).Ceil() <= maxWidth }
	var lines []string
	for _, word := range strings.Fields(text) {
		if len(lines) == 0 {
			if !fits(word) {
				return nil
			}
			lines = append(lines, word)
			continue
		}
		candidate := lines[len(lines)-1] + " " + word
		if fits(candidate) {
			lines[len(lines)-1] = candidate
			continue
		}
		if !fits(word) || len(lines) >= maxLines {
			return nil
		}
		lines = append(lines, word)
	}
	if len(lines) > maxLines {
		return nil
	}
	return lines
}

// drawDemoQR paints a QR-look demonstration module grid derived from a fixed
// digest: three finder squares plus deterministic data modules. It is clearly
// not a scannable SAT code and encodes nothing.
func drawDemoQR(page *image.RGBA, qr QRBlock, ink color.RGBA) {
	if qr.Size <= 0 {
		return
	}
	const modules = 21
	cell := qr.Size / (modules + 4) // two-module quiet zone
	origin := qr.Size % (modules + 4) / 2
	module := func(x, y int) {
		px, py := qr.X+origin+x*cell, qr.Y+origin+y*cell
		for dy := 0; dy < cell; dy++ {
			for dx := 0; dx < cell; dx++ {
				page.SetRGBA(px+dx, py+dy, ink)
			}
		}
	}
	finder := func(mx, my int) {
		for dy := -1; dy <= 7; dy++ {
			for dx := -1; dx <= 7; dx++ {
				x, y := mx+dx, my+dy
				if x < 0 || y < 0 || x >= modules || y >= modules {
					continue
				}
				dark := dx == -1 || dx == 7 || dy == -1 || dy == 7 ||
					(dx >= 1 && dx <= 5 && dy >= 1 && dy <= 5 && (dx == 1 || dx == 5 || dy == 1 || dy == 5))
				if dark {
					module(x, y)
				}
			}
		}
	}
	digest := sha256.Sum256([]byte("invoice-demo-qr-v1"))
	bit := 0
	for y := 0; y < modules; y++ {
		for x := 0; x < modules; x++ {
			inFinder := (x < 9 && y < 9) || (x >= modules-9 && y < 9) || (x < 9 && y >= modules-9)
			if inFinder {
				continue
			}
			if digest[bit/8]&(1<<(bit%8)) != 0 {
				module(x, y)
			}
			bit = (bit + 1) % (len(digest) * 8)
		}
	}
	finder(0, 0)
	finder(modules-7, 0)
	finder(0, modules-7)
}
