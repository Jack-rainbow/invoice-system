package render

import (
	"bytes"
	"compress/zlib"
	"image"
	"image/png"
	"math"

	"golang.org/x/image/draw"
)

// Formal output geometry. The logical canvas is 1582x2048 at 0.36 pt/pixel,
// which is exactly the 569.52x737.28 pt sheet; the formal PNG doubles the
// raster for archival quality while the PDF embeds the 1x pixels at 100%.
const (
	formalScale  = 2
	paperWidthPt = 569.52
	paperHeighPt = 737.28
	pngByteLimit = 20 << 20
	pdfByteLimit = 20 << 20
)

// CleanPNG renders the watermark-free formal page and exports the 2x PNG.
// This function is the only producer of clean pixels besides CleanPDF; callers
// must only hand the bytes to the artifact store of a settled version.
func (a *Assets) CleanPNG(inv Invoice) ([]byte, error) {
	base, err := a.RenderInvoice(inv)
	if err != nil {
		return nil, err
	}
	w, h := base.Bounds().Dx()*formalScale, base.Bounds().Dy()*formalScale
	scaled := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.BiLinear.Scale(scaled, scaled.Bounds(), base, base.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	if err := png.Encode(&buf, scaled); err != nil {
		return nil, err
	}
	if buf.Len() > pngByteLimit {
		return nil, ErrPreviewTooLarge
	}
	return buf.Bytes(), nil
}

// CleanPDF renders the same formal page and embeds its 1x pixels into a
// single-page PDF at the fixed physical sheet size, zero margins. The image is
// Flate-compressed raw RGB; no JavaScript, annotations or external references
// are emitted.
func (a *Assets) CleanPDF(inv Invoice) ([]byte, error) {
	base, err := a.RenderInvoice(inv)
	if err != nil {
		return nil, err
	}
	w, h := base.Bounds().Dx(), base.Bounds().Dy()
	var raw bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&raw, zlib.DefaultCompression)
	for y := 0; y < h; y++ {
		row := base.Pix[y*base.Stride : y*base.Stride+w*4]
		rgb := make([]byte, w*3)
		for x := 0; x < w; x++ {
			rgb[x*3], rgb[x*3+1], rgb[x*3+2] = row[x*4], row[x*4+1], row[x*4+2]
		}
		if _, err := zw.Write(rgb); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	pdf, err := singlePageImagePDF(w, h, raw.Bytes())
	if err != nil {
		return nil, err
	}
	if len(pdf) > pdfByteLimit {
		return nil, ErrPreviewTooLarge
	}
	return pdf, nil
}

// singlePageImagePDF assembles a deterministic minimal PDF whose one page
// draws the compressed RGB image across the whole sheet.
func singlePageImagePDF(w, h int, flateRGB []byte) ([]byte, error) {
	content := "q\n" + fixedDecimal(paperWidthPt) + " 0 0 " + fixedDecimal(paperHeighPt) + " 0 0 cm\n/Im0 Do\nQ\n"
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 " + fixedDecimal(paperWidthPt) + " " + fixedDecimal(paperHeighPt) +
			"] /Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>"),
		streamObject([]byte(content), nil),
		streamObject(flateRGB, []byte("/Type /XObject /Subtype /Image /Width "+itoa(w)+" /Height "+itoa(h)+
			" /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode")),
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	offsets := make([]int, len(objects)+1)
	for i, body := range objects {
		offsets[i+1] = out.Len()
		out.WriteString(itoa(i+1) + " 0 obj\n")
		out.Write(body)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	out.WriteString("xref\n0 " + itoa(len(objects)+1) + "\n0000000000 65535 f \n")
	for i := range objects {
		var entry [20]byte
		copy(entry[:], []byte(pad10(offsets[i+1])+" 00000 n \n"))
		out.Write(entry[:])
	}
	out.WriteString("trailer\n<< /Size " + itoa(len(objects)+1) + " /Root 1 0 R >>\nstartxref\n" + itoa(xref) + "\n%%EOF\n")
	return out.Bytes(), nil
}

func streamObject(data, extra []byte) []byte {
	head := "<< /Length " + itoa(len(data))
	if extra != nil {
		head += " " + string(extra)
	}
	head += " >>\nstream\n"
	tail := "\nendstream"
	out := make([]byte, 0, len(head)+len(data)+len(tail))
	out = append(out, head...)
	out = append(out, data...)
	return append(out, tail...)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

func pad10(n int) string {
	s := itoa(n)
	for len(s) < 10 {
		s = "0" + s
	}
	return s
}

// fixedDecimal renders a float with exactly two decimals, the PDF number form
// used for the fixed sheet size.
func fixedDecimal(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0.00"
	}
	scaled := int64(math.Round(v * 100))
	neg := ""
	if scaled < 0 {
		neg, scaled = "-", -scaled
	}
	whole, frac := scaled/100, scaled%100
	return neg + itoa(int(whole)) + "." + itoa(int(frac/10)) + itoa(int(frac%10))
}
