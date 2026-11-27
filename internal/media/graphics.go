package media

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"os"

	"github.com/axin2024/media-stream/internal/catalog"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

const CompositionWidth, CompositionHeight = 1280, 720

type Graphics struct {
	FontPath string
}

type graphicFont struct {
	font   *opentype.Font
	digest [32]byte
}

func (g Graphics) loadFont() (graphicFont, error) {
	invalid := &catalog.ProductionInputError{Field: "font", Cause: "configure the same approved OpenType or TrueType font on the origin and renderers"}
	if g.FontPath == "" {
		return graphicFont{}, invalid
	}
	f, err := os.Open(g.FontPath)
	if err != nil {
		return graphicFont{}, invalid
	}
	defer f.Close()
	const maxFont = 32 << 20
	byt, err := io.ReadAll(io.LimitReader(f, maxFont+1))
	if err != nil || len(byt) == 0 || len(byt) > maxFont {
		return graphicFont{}, invalid
	}
	parsed, err := opentype.Parse(byt)
	if err != nil {
		return graphicFont{}, invalid
	}
	return graphicFont{parsed, sha256.Sum256(byt)}, nil
}

func (g Graphics) Preview(in catalog.OverlayRequest) (image.Image, [32]byte, error) {
	if err := in.Validate(); err != nil {
		return nil, [32]byte{}, err
	}
	loaded, err := g.loadFont()
	if err != nil {
		return nil, [32]byte{}, err
	}
	canvas := image.NewRGBA(image.Rect(0, 0, CompositionWidth, CompositionHeight))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.RGBA{31, 44, 60, 255}), image.Point{}, draw.Src)
	if err := loaded.paint(canvas, in.Template, in.Content, in.Size, catalog.ProductionMaxFrames-1); err != nil {
		return nil, [32]byte{}, err
	}
	return canvas, loaded.digest, nil
}

func (f graphicFont) paint(canvas *image.RGBA, template, text string, size int, frame int64) error {
	fill := color.RGBA{19, 29, 43, 250}
	var x, y, maxWidth int
	switch template {
	case "lower_third":
		x, y, maxWidth = 48, 720-124, 750
	case "logo":
		y, maxWidth = 44, 320
		fill = color.RGBA{23, 78, 106, 255}
	case "timer":
		x, y, maxWidth = 48, 44, 400
		seconds := frame / catalog.ProductionFPS
		if text != "" {
			text += "  "
		}
		text += fmt.Sprintf("%02d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
	default:
		return &catalog.ProductionInputError{Field: "template", Cause: "unknown overlay template"}
	}
	var glyphBuffer sfnt.Buffer
	for _, r := range text {
		glyph, err := f.font.GlyphIndex(&glyphBuffer, r)
		if err != nil || glyph == 0 {
			return &catalog.ProductionInputError{Field: "text", Cause: fmt.Sprintf("approved font does not contain the character %q", r)}
		}
	}
	face, err := opentype.NewFace(f.font, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return err
	}
	defer face.Close()
	ink, advance := font.BoundString(face, text)
	minX := min(0, ink.Min.X.Floor())
	width := max(advance.Ceil(), ink.Max.X.Ceil()) - minX + 32
	height := max(face.Metrics().Height.Ceil(), ink.Max.Y.Ceil()-ink.Min.Y.Floor()) + 16
	if width > maxWidth || height > 88 {
		return &catalog.ProductionInputError{Field: "text", Cause: "rendered text exceeds the template safe area"}
	}
	if template == "logo" {
		x = CompositionWidth - 48 - width
	}
	panel := image.Rect(x, y, x+width, y+height)
	draw.Draw(canvas, panel, image.NewUniform(fill), image.Point{}, draw.Over)
	if template == "lower_third" {
		draw.Draw(canvas, image.Rect(x, y, x+5, y+height), image.NewUniform(color.RGBA{93, 213, 220, 255}), image.Point{}, draw.Src)
	}
	textDrawer := font.Drawer{Dst: canvas, Src: image.NewUniform(color.White), Face: face, Dot: fixed.P(x+16-minX, y+8-ink.Min.Y.Floor())}
	textDrawer.DrawString(text)
	return nil
}

func (g Graphics) frames(overlays []catalog.Overlay, start int64, count int) ([]graphicFrame, error) {
	if len(overlays) == 0 {
		return nil, nil
	}
	f, err := g.loadFont()
	if err != nil {
		return nil, err
	}
	end := start + int64(count)
	var result []graphicFrame
	for _, o := range overlays {
		if hex.EncodeToString(f.digest[:]) != o.Font {
			return nil, &catalog.ProductionInputError{Field: "font", Cause: "renderer font does not match the approved preview"}
		}
		for from := max(start, o.On); from < min(end, o.Off); {
			to := min(end, o.Off)
			if o.Template == "timer" {
				to = min(to, (from/catalog.ProductionFPS+1)*catalog.ProductionFPS)
			}
			canvas := image.NewRGBA(image.Rect(0, 0, CompositionWidth, CompositionHeight))
			if err := f.paint(canvas, o.Template, o.Content, o.Size, from); err != nil {
				return nil, err
			}
			result = append(result, graphicFrame{int(from - start), int(to - start), canvas})
			from = to
		}
	}
	return result, nil
}

type graphicFrame struct {
	from, until int
	image       image.Image
}
