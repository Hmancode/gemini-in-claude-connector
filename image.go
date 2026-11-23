package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ImageOptions struct {
	Prompt      string
	InputImages []string
	OutDir      string
	Model       string
	AspectRatio string
}

type ImageResult struct {
	Paths    []string
	Text     string
	Model    string
	Previews []Blob
}

const previewLimit = 700 << 10

func (c *Client) Image(ctx context.Context, o ImageOptions) (*ImageResult, error) {
	if strings.TrimSpace(o.Prompt) == "" {
		return nil, fmt.Errorf("describe the image you want")
	}
	model, err := c.ResolveModel(ctx, o.Model, "image")
	if err != nil {
		return nil, err
	}
	var parts []Part
	for _, raw := range o.InputImages {
		path, err := expandPath(raw)
		if err != nil {
			return nil, err
		}
		if err := checkAllowed(path, c.cfg); err != nil {
			return nil, err
		}
		mime := mediaTypes[strings.ToLower(filepath.Ext(path))]
		if !strings.HasPrefix(mime, "image/") {
			return nil, fmt.Errorf("%s isn't a PNG, JPEG, WebP or HEIC image", filepath.Base(path))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if len(data) > maxInlineTotal {
			return nil, fmt.Errorf("%s is too large to edit (%s); use a smaller copy", filepath.Base(path), humanSize(int64(len(data))))
		}
		parts = append(parts, Part{InlineData: &Blob{MimeType: mime, Data: base64.StdEncoding.EncodeToString(data)}})
	}
	parts = append(parts, Part{Text: o.Prompt})

	req := &GenerateRequest{
		Contents:         []Content{{Role: "user", Parts: parts}},
		GenerationConfig: &GenConfig{ResponseModalities: []string{"TEXT", "IMAGE"}},
	}
	if ar := strings.TrimSpace(o.AspectRatio); ar != "" {
		req.GenerationConfig.ImageConfig = &ImageConfig{AspectRatio: ar}
	}
	resp, err := c.Generate(ctx, model, req)
	if err != nil {
		return nil, err
	}
	cand, err := firstCandidate(resp)
	if err != nil {
		return nil, err
	}

	outDir, err := expandPath(firstNonEmpty(o.OutDir, c.cfg.ImageDir, defaultImageDir()))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("can't create %s: %v", outDir, err)
	}
	res := &ImageResult{Text: strings.TrimSpace(partsText(cand.Content.Parts)), Model: firstNonEmpty(resp.ModelVersion, model)}
	stamp := time.Now().Format("20060102-150405")
	for _, p := range cand.Content.Parts {
		if p.Thought || p.InlineData == nil || !strings.HasPrefix(p.InlineData.MimeType, "image/") {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
		if err != nil {
			continue
		}
		path := uniquePath(filepath.Join(outDir, fmt.Sprintf("gemini-%s-%d%s", stamp, len(res.Paths)+1, extForMime(p.InlineData.MimeType))))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
		res.Paths = append(res.Paths, path)
		if pv, ok := makePreview(data, p.InlineData.MimeType); ok {
			res.Previews = append(res.Previews, pv)
		}
	}
	if len(res.Paths) == 0 {
		msg := fmt.Sprintf("Gemini didn't return an image (finish reason: %s)", firstNonEmpty(cand.FinishReason, "unknown"))
		if res.Text != "" {
			msg += ". It said: " + res.Text
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return res, nil
}

func extForMime(m string) string {
	switch m {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	}
	return ".png"
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		q := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, err := os.Stat(q); os.IsNotExist(err) {
			return q
		}
	}
}

// makePreview returns a small copy for Claude to look at; the saved file stays full size.
func makePreview(data []byte, mime string) (Blob, bool) {
	if len(data) <= previewLimit {
		return Blob{MimeType: mime, Data: base64.StdEncoding.EncodeToString(data)}, true
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Blob{}, false
	}
	for i := 0; i < 4; i++ {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
			return Blob{}, false
		}
		if buf.Len() <= previewLimit {
			return Blob{MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(buf.Bytes())}, true
		}
		img = halve(img)
	}
	return Blob{}, false
}

func halve(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx()/2, b.Dy()/2
	if w < 1 || h < 1 {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(b.Min.X+2*x, b.Min.Y+2*y))
		}
	}
	return dst
}
