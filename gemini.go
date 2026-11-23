package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com"

var errNoKey = errors.New("no Gemini API key is set up yet. Run `gemini-bridge setup` (or set the GEMINI_API_KEY environment variable). Keys are free at https://aistudio.google.com/apikey")

// pollInterval is how often an uploaded video is checked while Google processes it.
var pollInterval = 3 * time.Second

// ---------- wire types (Gemini REST, camelCase) ----------

type Blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type FileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type Part struct {
	Text             string    `json:"text,omitempty"`
	InlineData       *Blob     `json:"inlineData,omitempty"`
	FileData         *FileData `json:"fileData,omitempty"`
	Thought          bool      `json:"thought,omitempty"`
	ThoughtSignature string    `json:"thoughtSignature,omitempty"`
}

type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts"`
}

type ImageConfig struct {
	AspectRatio string `json:"aspectRatio,omitempty"`
}

type GenConfig struct {
	ResponseModalities []string     `json:"responseModalities,omitempty"`
	ImageConfig        *ImageConfig `json:"imageConfig,omitempty"`
}

type GenerateRequest struct {
	Contents          []Content        `json:"contents"`
	SystemInstruction *Content         `json:"systemInstruction,omitempty"`
	Tools             []map[string]any `json:"tools,omitempty"`
	GenerationConfig  *GenConfig       `json:"generationConfig,omitempty"`
}

type groundingChunk struct {
	Web *struct {
		URI   string `json:"uri"`
		Title string `json:"title"`
	} `json:"web"`
}

type Candidate struct {
	Content           Content `json:"content"`
	FinishReason      string  `json:"finishReason"`
	GroundingMetadata *struct {
		GroundingChunks []groundingChunk `json:"groundingChunks"`
	} `json:"groundingMetadata"`
}

type GenerateResponse struct {
	Candidates     []Candidate `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	ModelVersion string `json:"modelVersion"`
}

type ModelInfo struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	InputTokenLimit            int      `json:"inputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

func (m ModelInfo) ID() string { return strings.TrimPrefix(m.Name, "models/") }

func (m ModelInfo) CanGenerate() bool {
	for _, s := range m.SupportedGenerationMethods {
		if s == "generateContent" {
			return true
		}
	}
	return false
}

// ---------- errors ----------

type APIError struct {
	HTTPStatus int
	Status     string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Gemini API error %d: %s", e.HTTPStatus, e.Message)
	}
	return fmt.Sprintf("Gemini API error %d", e.HTTPStatus)
}

func readAPIError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &APIError{HTTPStatus: resp.StatusCode}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil {
		e.Message, e.Status = env.Error.Message, env.Error.Status
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(b))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300]
		}
	}
	return e
}

// RefusalError means Gemini's own safety system blocked or stopped the answer.
type RefusalError struct{ Reason string }

func (e *RefusalError) Error() string {
	return "Gemini declined or stopped this request (reason: " + e.Reason + ")"
}

// explain turns errors into something a person (or Claude) can act on.
func explain(err error) string {
	var ae *APIError
	var re *RefusalError
	switch {
	case errors.Is(err, errNoKey):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "Gemini took too long to answer and the request timed out. Try a smaller request or the \"flash\" model."
	case errors.Is(err, context.Canceled):
		return "Cancelled."
	case errors.As(err, &re):
		return re.Error() + ". Gemini has its own content rules, separate from Claude's."
	case errors.As(err, &ae):
		low := strings.ToLower(ae.Message)
		switch {
		case ae.HTTPStatus == 400 && strings.Contains(low, "api key"):
			return "Gemini rejected the API key. Run `gemini-bridge setup` to enter a new one. (" + ae.Message + ")"
		case ae.HTTPStatus == 403:
			return "Gemini refused access with this key: " + ae.Message
		case ae.HTTPStatus == 404:
			return "That Gemini model wasn't found (Google retires models often): " + ae.Message +
				" Use the model \"flash\" or \"pro\", or run `gemini-bridge models` to see what's available."
		case ae.HTTPStatus == 429:
			return "Gemini's rate limit or quota was hit: " + ae.Message +
				" Free keys have per-minute and per-day caps (tightest on Pro). Wait a bit or use the \"flash\" model."
		}
		return ae.Error()
	}
	return err.Error()
}

// ---------- client ----------

type Client struct {
	key  string
	base string
	cfg  *Config
	http *http.Client

	mu     sync.Mutex
	models []ModelInfo

	convMu sync.Mutex
	convos map[string][]Content
}

func newClient(key string, cfg *Config) *Client {
	base := strings.TrimRight(os.Getenv("GEMINI_BASE_URL"), "/")
	if base == "" {
		base = defaultBaseURL
	}
	if cfg == nil {
		cfg = &Config{}
	}
	return &Client{key: key, base: base, cfg: cfg, http: &http.Client{}, convos: map[string][]Content{}}
}

func newClientFromConfig() (*Client, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.APIKey == "" {
		return nil, errNoKey
	}
	return newClient(cfg.APIKey, cfg), nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	retryDelays := []time.Duration{2 * time.Second, 6 * time.Second}
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
		if err != nil {
			return err
		}
		req.Header.Set("x-goog-api-key", c.key)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt < len(retryDelays) {
				if sleepCtx(ctx, retryDelays[attempt]) != nil {
					return ctx.Err()
				}
				continue
			}
			return fmt.Errorf("couldn't reach Gemini: %w", err)
		}
		if resp.StatusCode >= 500 && attempt < len(retryDelays) {
			resp.Body.Close()
			if sleepCtx(ctx, retryDelays[attempt]) != nil {
				return ctx.Err()
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			err := readAPIError(resp)
			resp.Body.Close()
			return err
		}
		if out == nil {
			resp.Body.Close()
			return nil
		}
		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("unexpected reply from Gemini: %w", err)
		}
		return nil
	}
}

func (c *Client) ListModels(ctx context.Context, refresh bool) ([]ModelInfo, error) {
	c.mu.Lock()
	if c.models != nil && !refresh {
		m := c.models
		c.mu.Unlock()
		return m, nil
	}
	c.mu.Unlock()

	var all []ModelInfo
	token := ""
	for page := 0; page < 20; page++ {
		u := c.base + "/v1beta/models?pageSize=1000"
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		var resp struct {
			Models        []ModelInfo `json:"models"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if err := c.doJSON(ctx, http.MethodGet, u, nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Models...)
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}
	c.mu.Lock()
	c.models = all
	c.mu.Unlock()
	return all, nil
}

func (c *Client) Generate(ctx context.Context, model string, req *GenerateRequest) (*GenerateResponse, error) {
	var resp GenerateResponse
	endpoint := c.base + "/v1beta/models/" + url.PathEscape(model) + ":generateContent"
	err := c.doJSON(ctx, http.MethodPost, endpoint, req, &resp)
	var ae *APIError
	if errors.As(err, &ae) && ae.HTTPStatus == http.StatusNotFound {
		c.mu.Lock()
		c.models = nil // a model vanished; re-list next time
		c.mu.Unlock()
	}
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------- model shortcuts ----------
//
// Google renames and retires models every few months, so instead of
// hard-coding ids the shortcuts "flash", "pro", "image" and "image-pro" are
// resolved against the live model list: a "-latest" alias if Google offers
// one, otherwise the highest version number, preferring stable over preview.

type tierRule struct {
	alias string
	re    *regexp.Regexp
}

var tierRules = map[string]tierRule{
	"flash":     {"gemini-flash-latest", regexp.MustCompile(`^gemini-(\d+(?:\.\d+)?)-flash(-preview)?(-\d\d-\d{4})?$`)},
	"pro":       {"gemini-pro-latest", regexp.MustCompile(`^gemini-(\d+(?:\.\d+)?)-pro(-preview)?(-\d\d-\d{4})?$`)},
	"image":     {"", regexp.MustCompile(`^gemini-(\d+(?:\.\d+)?)-flash-image(-preview)?(-\d\d-\d{4})?$`)},
	"image-pro": {"", regexp.MustCompile(`^gemini-(\d+(?:\.\d+)?)-pro-image(-preview)?(-\d\d-\d{4})?$`)},
}

var tierOrder = []string{"flash", "pro", "image", "image-pro"}

func versionKey(s string) int {
	major, minor, _ := strings.Cut(s, ".")
	a, _ := strconv.Atoi(major)
	b, _ := strconv.Atoi(minor)
	return a*1000 + b
}

func pickModel(models []ModelInfo, tier string) string {
	rule, ok := tierRules[tier]
	if !ok {
		return ""
	}
	type cand struct {
		id     string
		ver    int
		stable bool
	}
	var cs []cand
	for _, m := range models {
		if !m.CanGenerate() {
			continue
		}
		id := m.ID()
		if rule.alias != "" && id == rule.alias {
			return id
		}
		sub := rule.re.FindStringSubmatch(id)
		if sub == nil {
			continue
		}
		cs = append(cs, cand{id, versionKey(sub[1]), sub[2] == "" && sub[3] == ""})
	}
	if len(cs) == 0 {
		return ""
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].ver != cs[j].ver {
			return cs[i].ver > cs[j].ver
		}
		if cs[i].stable != cs[j].stable {
			return cs[i].stable
		}
		return cs[i].id > cs[j].id
	})
	return cs[0].id
}

// ResolveModel turns "flash"/"pro"/"image"/"image-pro" (or "") into a real
// model id. Anything else is treated as an exact id and passed through.
func (c *Client) ResolveModel(ctx context.Context, want, fallback string) (string, error) {
	name := firstNonEmpty(want, fallback)
	tier := strings.ReplaceAll(strings.ToLower(name), " ", "-")
	if _, ok := tierRules[tier]; !ok {
		return strings.TrimPrefix(name, "models/"), nil
	}
	pinned := map[string]string{"flash": c.cfg.FlashModel, "pro": c.cfg.ProModel, "image": c.cfg.ImageModel, "image-pro": c.cfg.ImageProModel}
	if p := strings.TrimSpace(pinned[tier]); p != "" {
		return strings.TrimPrefix(p, "models/"), nil
	}
	models, err := c.ListModels(ctx, false)
	if err != nil {
		return "", err
	}
	if id := pickModel(models, tier); id != "" {
		return id, nil
	}
	if alt := map[string]string{"image": "image-pro", "image-pro": "image"}[tier]; alt != "" {
		if id := pickModel(models, alt); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("couldn't find a current Gemini %q model for this API key. Run `gemini-bridge models` to see what's available and pass an exact model id", tier)
}

// TierSummary reports what each shortcut points to right now.
func (c *Client) TierSummary(ctx context.Context) ([][2]string, error) {
	if _, err := c.ListModels(ctx, true); err != nil {
		return nil, err
	}
	var out [][2]string
	for _, t := range tierOrder {
		id, err := c.ResolveModel(ctx, t, t)
		if err != nil {
			id = "(none available)"
		}
		out = append(out, [2]string{t, id})
	}
	return out, nil
}

// ---------- ask ----------

type AskOptions struct {
	Prompt         string
	Files          []string
	URLs           []string
	Search         bool
	Model          string
	System         string
	ConversationID string
}

type Source struct{ Title, URL string }

type AskResult struct {
	Text    string
	Model   string
	Sources []Source
	Notes   []string
}

func isYouTube(u string) bool {
	l := strings.ToLower(u)
	for _, s := range []string{"youtube.com/watch", "youtu.be/", "youtube.com/shorts/", "youtube.com/live/"} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func partsText(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if !p.Thought && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func hasImage(parts []Part) bool {
	for _, p := range parts {
		if p.InlineData != nil && strings.HasPrefix(p.InlineData.MimeType, "image/") {
			return true
		}
	}
	return false
}

func firstCandidate(r *GenerateResponse) (*Candidate, error) {
	if r.PromptFeedback != nil && r.PromptFeedback.BlockReason != "" {
		return nil, &RefusalError{Reason: r.PromptFeedback.BlockReason}
	}
	if len(r.Candidates) == 0 {
		return nil, errors.New("Gemini returned no answer")
	}
	cand := &r.Candidates[0]
	switch cand.FinishReason {
	case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII", "RECITATION", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT":
		if strings.TrimSpace(partsText(cand.Content.Parts)) == "" && !hasImage(cand.Content.Parts) {
			return nil, &RefusalError{Reason: cand.FinishReason}
		}
	}
	return cand, nil
}

func sourcesOf(cand *Candidate) []Source {
	if cand.GroundingMetadata == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Source
	for _, ch := range cand.GroundingMetadata.GroundingChunks {
		if ch.Web == nil || ch.Web.URI == "" || seen[ch.Web.URI] {
			continue
		}
		seen[ch.Web.URI] = true
		out = append(out, Source{Title: ch.Web.Title, URL: ch.Web.URI})
	}
	return out
}

const maxHistory = 30

func (c *Client) history(id string) []Content {
	if id == "" {
		return nil
	}
	c.convMu.Lock()
	defer c.convMu.Unlock()
	return append([]Content(nil), c.convos[id]...)
}

func (c *Client) remember(id string, user, model Content) {
	if model.Role == "" {
		model.Role = "model"
	}
	c.convMu.Lock()
	defer c.convMu.Unlock()
	h := append(c.convos[id], user, model)
	if len(h) > maxHistory {
		h = h[len(h)-maxHistory:]
	}
	c.convos[id] = h
}

func (c *Client) Ask(ctx context.Context, o AskOptions) (*AskResult, error) {
	model, err := c.ResolveModel(ctx, o.Model, firstNonEmpty(c.cfg.DefaultModel, "flash"))
	if err != nil {
		return nil, err
	}
	parts, notes, err := c.prepareFiles(ctx, o.Files)
	if err != nil {
		return nil, err
	}
	prompt := strings.TrimSpace(o.Prompt)
	var pages []string
	for _, u := range o.URLs {
		u = strings.TrimSpace(u)
		switch {
		case u == "":
		case isYouTube(u):
			parts = append(parts, Part{FileData: &FileData{FileURI: u}})
			notes = append(notes, "sent YouTube video "+u)
		default:
			pages = append(pages, u)
		}
	}
	if len(pages) > 0 {
		prompt += "\n\nRead these pages:\n" + strings.Join(pages, "\n")
	}
	if prompt == "" {
		prompt = "Describe and summarize the attached material."
	}
	parts = append(parts, Part{Text: prompt}) // media first, instructions last

	user := Content{Role: "user", Parts: parts}
	req := &GenerateRequest{Contents: append(c.history(o.ConversationID), user)}
	if s := strings.TrimSpace(o.System); s != "" {
		req.SystemInstruction = &Content{Parts: []Part{{Text: s}}}
	}
	if o.Search {
		req.Tools = append(req.Tools, map[string]any{"googleSearch": map[string]any{}})
	}
	if len(pages) > 0 {
		req.Tools = append(req.Tools, map[string]any{"urlContext": map[string]any{}})
	}

	resp, err := c.Generate(ctx, model, req)
	if err != nil {
		return nil, err
	}
	cand, err := firstCandidate(resp)
	if err != nil {
		return nil, err
	}
	text := partsText(cand.Content.Parts)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("Gemini returned an empty answer (finish reason: %s)", firstNonEmpty(cand.FinishReason, "unknown"))
	}
	if o.ConversationID != "" {
		c.remember(o.ConversationID, user, cand.Content)
	}
	return &AskResult{Text: text, Model: firstNonEmpty(resp.ModelVersion, model), Sources: sourcesOf(cand), Notes: notes}, nil
}

func formatAnswer(r *AskResult, forClaude bool) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(r.Text))
	if len(r.Sources) > 0 {
		b.WriteString("\n\nSources:\n")
		for _, s := range r.Sources {
			fmt.Fprintf(&b, "- %s: %s\n", firstNonEmpty(s.Title, s.URL), s.URL)
		}
	}
	if forClaude {
		note := "[Answered by " + r.Model
		if len(r.Notes) > 0 {
			note += "; " + strings.Join(r.Notes, "; ")
		}
		b.WriteString("\n\n" + note + "]")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------- Files API (big files, audio, video) ----------

type remoteFile struct {
	Name     string `json:"name"`
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	State    string `json:"state"`
}

func (c *Client) UploadFile(ctx context.Context, path, mime string) (*remoteFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	meta, _ := json.Marshal(map[string]any{"file": map[string]string{"display_name": filepath.Base(path)}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/upload/v1beta/files", bytes.NewReader(meta))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-goog-api-key", c.key)
	req.Header.Set("X-Goog-Upload-Protocol", "resumable")
	req.Header.Set("X-Goog-Upload-Command", "start")
	req.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.FormatInt(st.Size(), 10))
	req.Header.Set("X-Goog-Upload-Header-Content-Type", mime)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't start uploading %s: %w", filepath.Base(path), err)
	}
	if resp.StatusCode != http.StatusOK {
		e := readAPIError(resp)
		resp.Body.Close()
		return nil, e
	}
	resp.Body.Close()
	uploadURL := resp.Header.Get("X-Goog-Upload-URL")
	if uploadURL == "" {
		return nil, errors.New("Gemini didn't return an upload address")
	}

	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, f)
	if err != nil {
		return nil, err
	}
	req2.ContentLength = st.Size()
	req2.Header.Set("X-Goog-Upload-Offset", "0")
	req2.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	resp2, err := c.http.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("upload of %s failed: %w", filepath.Base(path), err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return nil, readAPIError(resp2)
	}
	var out struct {
		File remoteFile `json:"file"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("unexpected upload reply: %w", err)
	}
	file := out.File

	// Video and audio are processed before they can be used.
	deadline := time.Now().Add(20 * time.Minute)
	for file.State == "PROCESSING" {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("Google is still processing %s after 20 minutes", filepath.Base(path))
		}
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return nil, err
		}
		var next remoteFile
		if err := c.doJSON(ctx, http.MethodGet, c.base+"/v1beta/"+file.Name, nil, &next); err != nil {
			return nil, err
		}
		file = next
	}
	if file.State == "FAILED" {
		return nil, fmt.Errorf("Google couldn't process %s", filepath.Base(path))
	}
	if file.MimeType == "" {
		file.MimeType = mime
	}
	return &file, nil
}
