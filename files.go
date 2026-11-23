package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Formats Gemini reads natively. Everything else is sent as text if it is
// text, or extracted (.docx, .pptx), or refused with a hint.
var mediaTypes = map[string]string{
	".pdf":  "application/pdf",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".heic": "image/heic",
	".heif": "image/heif",
	".mp3":  "audio/mp3",
	".wav":  "audio/wav",
	".aac":  "audio/aac",
	".ogg":  "audio/ogg",
	".flac": "audio/flac",
	".aiff": "audio/aiff",
	".aif":  "audio/aiff",
	".m4a":  "audio/mp4",
	".mp4":  "video/mp4",
	".mpeg": "video/mpeg",
	".mpg":  "video/mpg",
	".mov":  "video/mov",
	".avi":  "video/avi",
	".flv":  "video/x-flv",
	".webm": "video/webm",
	".wmv":  "video/wmv",
	".3gp":  "video/3gpp",
}

const (
	maxInlineFile  = 8 << 20  // bigger binary files go through the Files API
	maxInlineTotal = 14 << 20 // keeps the request under Gemini's inline limit
	maxInlineText  = 1 << 20  // bigger text files are uploaded as text/plain
)

func expandPath(p string) (string, error) {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"'`))
	if p == "" {
		return "", fmt.Errorf("empty file path")
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	return filepath.Abs(p)
}

func isWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// checkAllowed keeps obvious secrets (and this tool's own key file) from
// being sent to Google, and enforces allowed_dirs if the user set it.
func checkAllowed(path string, cfg *Config) error {
	slash := "/" + strings.ToLower(filepath.ToSlash(path)) + "/"
	for _, seg := range []string{"/.ssh/", "/.gnupg/", "/.aws/", "/.azure/", "/.kube/", "/.docker/"} {
		if strings.Contains(slash, seg) {
			return fmt.Errorf("refusing to send %s: it's inside a credentials folder", path)
		}
	}
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasPrefix(base, "id_rsa") ||
		strings.HasPrefix(base, "id_ed25519") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") ||
		strings.HasSuffix(base, ".pfx") || base == "credentials" || base == "credentials.json" {
		return fmt.Errorf("refusing to send %s: it looks like a key or credentials file", path)
	}
	if cd, err := configDir(); err == nil && isWithin(path, cd) {
		return fmt.Errorf("refusing to send %s: that's gemini-bridge's own key file", path)
	}
	if cfg != nil && len(cfg.AllowedDirs) > 0 {
		for _, d := range cfg.AllowedDirs {
			if ad, err := expandPath(d); err == nil && isWithin(path, ad) {
				return nil
			}
		}
		return fmt.Errorf("refusing to send %s: it's outside the folders listed in allowed_dirs", path)
	}
	return nil
}

func textPart(name, text string) Part {
	return Part{Text: fmt.Sprintf("--- File: %s ---\n%s\n--- End of %s ---", name, text, name)}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// looksLikeText sniffs the start of a file: no NUL bytes and valid UTF-8.
func looksLikeText(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]
	if bytes.IndexByte(buf, 0) >= 0 {
		return false
	}
	// allow a multi-byte character cut off at the end of the sample
	for i := 0; i < 4 && len(buf) > 0 && !utf8.Valid(buf); i++ {
		buf = buf[:len(buf)-1]
	}
	return utf8.Valid(buf)
}

func (c *Client) prepareFiles(ctx context.Context, paths []string) ([]Part, []string, error) {
	var parts []Part
	var notes []string
	var inlineTotal int64
	for _, raw := range paths {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		path, err := expandPath(raw)
		if err != nil {
			return nil, nil, err
		}
		if err := checkAllowed(path, c.cfg); err != nil {
			return nil, nil, err
		}
		st, err := os.Stat(path)
		if err != nil {
			return nil, nil, fmt.Errorf("can't open %s: %v", raw, err)
		}
		if st.IsDir() {
			return nil, nil, fmt.Errorf("%s is a folder; pass the individual files instead", raw)
		}
		name := filepath.Base(path)
		ext := strings.ToLower(filepath.Ext(path))

		switch {
		case ext == ".docx" || ext == ".pptx":
			text, err := officeText(path, ext)
			if err != nil {
				return nil, nil, err
			}
			parts = append(parts, textPart(name, text))
			notes = append(notes, "extracted text from "+name)

		case mediaTypes[ext] != "":
			mime := mediaTypes[ext]
			av := strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/")
			if av || st.Size() > maxInlineFile || inlineTotal+st.Size() > maxInlineTotal {
				rf, err := c.UploadFile(ctx, path, mime)
				if err != nil {
					return nil, nil, err
				}
				parts = append(parts, Part{FileData: &FileData{MimeType: rf.MimeType, FileURI: rf.URI}})
				notes = append(notes, fmt.Sprintf("uploaded %s (%s) through the Files API", name, humanSize(st.Size())))
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, nil, err
			}
			inlineTotal += st.Size()
			parts = append(parts, Part{InlineData: &Blob{MimeType: mime, Data: base64.StdEncoding.EncodeToString(data)}})

		case ext == ".xlsx" || ext == ".xls" || ext == ".doc" || ext == ".ppt" || ext == ".key" || ext == ".pages":
			return nil, nil, fmt.Errorf("%s: Gemini can't read %s files directly. Export it as PDF (or CSV for spreadsheets) and send that", name, ext)

		case looksLikeText(path):
			if st.Size() > maxInlineText {
				rf, err := c.UploadFile(ctx, path, "text/plain")
				if err != nil {
					return nil, nil, err
				}
				parts = append(parts, Part{FileData: &FileData{MimeType: rf.MimeType, FileURI: rf.URI}})
				notes = append(notes, fmt.Sprintf("uploaded %s (%s) through the Files API", name, humanSize(st.Size())))
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, nil, err
			}
			parts = append(parts, textPart(name, string(data)))

		default:
			return nil, nil, fmt.Errorf("%s: unsupported file type. Gemini reads PDF, images (PNG/JPEG/WebP/HEIC), audio, video, text/code, .docx and .pptx", name)
		}
	}
	return parts, notes, nil
}

// ---------- .docx / .pptx text extraction ----------

var slideRe = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)

func officeText(path, ext string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("can't open %s as an Office file: %v", filepath.Base(path), err)
	}
	defer zr.Close()

	if ext == ".docx" {
		for _, f := range zr.File {
			if f.Name == "word/document.xml" {
				return ooxmlText(f)
			}
		}
		return "", fmt.Errorf("%s doesn't look like a Word document", filepath.Base(path))
	}

	type slide struct {
		n int
		f *zip.File
	}
	var slides []slide
	for _, f := range zr.File {
		if m := slideRe.FindStringSubmatch(f.Name); m != nil {
			n, _ := strconv.Atoi(m[1])
			slides = append(slides, slide{n, f})
		}
	}
	if len(slides) == 0 {
		return "", fmt.Errorf("%s doesn't look like a PowerPoint file", filepath.Base(path))
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].n < slides[j].n })
	var b strings.Builder
	for _, s := range slides {
		t, err := ooxmlText(s.f)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "[Slide %d]\n%s\n\n", s.n, t)
	}
	return strings.TrimSpace(b.String()), nil
}

// ooxmlText pulls the visible text out of a Word or PowerPoint XML part.
func ooxmlText(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	dec := xml.NewDecoder(rc)
	var b strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
	return strings.TrimSpace(b.String()), nil
}
