package render

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/math/f64"
	"golang.org/x/image/math/fixed"
)

// PreviewWatermarkStyle is the style version this binary implements. A render
// profile may only claim this exact style; anything else is a profile the
// compiled renderer cannot honour and must not be rendered.
const PreviewWatermarkStyle = "preview-watermark/1"

const (
	watermarkText    = "预览 未正式生成"
	watermarkAngle   = 30.0
	watermarkAlpha   = 0.32
	watermarkSize    = 40.0
	watermarkGapX    = 90
	watermarkGapY    = 110
	previewWidth     = 1000
	previewByteLimit = 2 << 20
)

// WatermarkSpec is the machine-checkable watermark contract of a render
// profile. It mirrors the profile manifest's watermark block so the worker can
// refuse a manifest whose declared style differs from the compiled one instead
// of silently rendering something the manifest does not describe.
type WatermarkSpec struct {
	StyleVersion string  `json:"style_version"`
	Text         string  `json:"text"`
	AngleDegrees float64 `json:"angle_degrees"`
	Alpha        float64 `json:"alpha"`
	WidthPX      int     `json:"width_px"`
	MaxBytes     int     `json:"max_bytes"`
}

// CompiledWatermark reports the watermark parameters baked into this binary.
func CompiledWatermark() WatermarkSpec {
	return WatermarkSpec{
		StyleVersion: PreviewWatermarkStyle,
		Text:         watermarkText,
		AngleDegrees: watermarkAngle,
		Alpha:        watermarkAlpha,
		WidthPX:      previewWidth,
		MaxBytes:     previewByteLimit,
	}
}

// watermarkTile renders the fixed legend plus the random user identifier
// into one transparent tile with the style alpha baked into every pixel.
func (a *Assets) watermarkTile(identity string) *image.NRGBA {
	alpha := uint8(math.Round(watermarkAlpha * 255))
	ink := color.NRGBA{R: 0x3f, G: 0x46, B: 0x55, A: alpha}
	cjk := a.newFace("watermark", watermarkSize)
	latin := a.newFace("bold", watermarkSize*0.72)
	label := " · " + identity
	cjkWidth := measure(cjk, watermarkText).Ceil()
	latinWidth := measure(latin, label).Ceil()
	tile := image.NewNRGBA(image.Rect(0, 0, cjkWidth+latinWidth+8, 64))
	baseline := 46
	src := image.NewUniform(ink)
	(&font.Drawer{Dst: tile, Src: src, Face: cjk, Dot: fixed.P(0, baseline)}).DrawString(watermarkText)
	(&font.Drawer{Dst: tile, Src: src, Face: latin, Dot: fixed.P(cjkWidth, baseline)}).DrawString(label)
	return tile
}

// rotate blits src rotated by angle degrees about its center into a canvas
// large enough to hold the result.
func rotate(src *image.NRGBA, angle float64) *image.NRGBA {
	theta := angle * math.Pi / 180
	sw, sh := float64(src.Bounds().Dx()), float64(src.Bounds().Dy())
	cos, sin := math.Cos(theta), math.Sin(theta)
	dw := int(math.Ceil(math.Abs(sw*cos) + math.Abs(sh*sin)))
	dh := int(math.Ceil(math.Abs(sw*sin) + math.Abs(sh*cos)))
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	// x/image/draw expects a source-to-destination matrix: rotate the tile
	// about its center and move that center to the destination center.
	m := f64.Aff3{
		cos, -sin, float64(dw)/2 - cos*sw/2 + sin*sh/2,
		sin, cos, float64(dh)/2 - sin*sw/2 - cos*sh/2,
	}
	draw.ApproxBiLinear.Transform(dst, m, src, src.Bounds(), draw.Src, nil)
	return dst
}

// applyPreviewWatermark tiles the rotated legend across the whole page and
// alpha-composites it into the pixels. The result has no separable layer.
func (a *Assets) applyPreviewWatermark(page *image.RGBA, identity string) {
	tile := rotate(a.watermarkTile(identity), watermarkAngle)
	tw, th := tile.Bounds().Dx(), tile.Bounds().Dy()
	stepX, stepY := tw+watermarkGapX, th+watermarkGapY
	bounds := page.Bounds()
	for row, y := 0, -th; y < bounds.Dy()+th; row, y = row+1, y+stepY {
		offset := 0
		if row%2 == 1 {
			offset = stepX / 2
		}
		for x := -tw + offset; x < bounds.Dx()+tw; x += stepX {
			draw.Draw(page, image.Rect(x, y, x+tw, y+th), tile, image.Point{}, draw.Over)
		}
	}
}

// PreviewJPEG renders the private base page, scales it to the preview width,
// burns in the watermark and re-encodes as JPEG within the byte limit. The
// clean base never leaves this function.
func (a *Assets) PreviewJPEG(inv Invoice, identity string) ([]byte, error) {
	base, err := a.RenderInvoice(inv)
	if err != nil {
		return nil, err
	}
	ratio := float64(previewWidth) / float64(base.Bounds().Dx())
	height := int(math.Round(float64(base.Bounds().Dy()) * ratio))
	scaled := image.NewRGBA(image.Rect(0, 0, previewWidth, height))
	draw.BiLinear.Scale(scaled, scaled.Bounds(), base, base.Bounds(), draw.Src, nil)
	a.applyPreviewWatermark(scaled, identity)
	for _, quality := range []int{85, 76, 68} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		if buf.Len() <= previewByteLimit {
			return buf.Bytes(), nil
		}
	}
	return nil, ErrPreviewTooLarge
}
