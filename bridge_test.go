package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- mock Gemini API ----------

type mockGemini struct {
	t         *testing.T
	srv       *httptest.Server
	mu        sync.Mutex
	requests  []map[string]any // generateContent bodies
	models    []string
	uploads   int
	polls     int
	status    int    // force an HTTP error on generateContent
	errMsg    string // message for forced error
	block     string // promptFeedback.blockReason
	lastModel string
	tried     []string // every model generateContent was called with

	modelErr  map[string]mockErr // per-model forced errors
	searchErr *mockErr           // forced error whenever googleSearch is requested
}

type mockErr struct {
	status int
	msg    string
}

func newMock(t *testing.T) *mockGemini {
	m := &mockGemini{t: t, models: []string{
		"gemini-2.5-flash", "gemini-3.5-flash", "gemini-3.7-flash", "gemini-3.1-flash-lite",
		"gemini-3.1-pro-preview", "gemini-3-pro-preview", "gemini-3.1-flash-image", "gemini-3-pro-image",
		"gemini-2.5-flash-preview-tts", "gemini-3.1-flash-live",
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1beta/models", m.listModels)
	mux.HandleFunc("/v1beta/models/", m.generate)
	mux.HandleFunc("/upload/v1beta/files", m.uploadStart)
	mux.HandleFunc("/upload-session/1", m.uploadBytes)
	mux.HandleFunc("/v1beta/files/abc", m.getFile)
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	t.Setenv("GEMINI_BASE_URL", m.srv.URL)
	return m
}

func (m *mockGemini) checkKey(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("x-goog-api-key") != "test-key" {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`)
		return false
	}
	return true
}

func (m *mockGemini) listModels(w http.ResponseWriter, r *http.Request) {
	if !m.checkKey(w, r) {
		return
	}
	var out []map[string]any
	for _, id := range m.models {
		methods := []string{"generateContent", "countTokens"}
		if strings.Contains(id, "live") {
			methods = []string{"bidiGenerateContent"}
		}
		out = append(out, map[string]any{"name": "models/" + id, "supportedGenerationMethods": methods})
	}
	json.NewEncoder(w).Encode(map[string]any{"models": out})
}

func tinyPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 7), uint8(y * 13), uint8(x ^ y), 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func (m *mockGemini) generate(w http.ResponseWriter, r *http.Request) {
	if !m.checkKey(w, r) {
		return
	}
	model := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1beta/models/"), ":generateContent")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.t.Errorf("bad request body: %v", err)
	}
	m.mu.Lock()
	m.requests = append(m.requests, body)
	m.lastModel = model
	m.tried = append(m.tried, model)
	status, msg, block := m.status, m.errMsg, m.block
	if e, ok := m.modelErr[model]; ok {
		status, msg = e.status, e.msg
	}
	if tools, ok := body["tools"].([]any); ok && m.searchErr != nil {
		for _, tl := range tools {
			if _, ok := tl.(map[string]any)["googleSearch"]; ok {
				status, msg = m.searchErr.status, m.searchErr.msg
			}
		}
	}
	m.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": status, "message": msg}})
		return
	}
	if block != "" {
		json.NewEncoder(w).Encode(map[string]any{"promptFeedback": map[string]any{"blockReason": block}})
		return
	}
	if strings.Contains(model, "image") {
		json.NewEncoder(w).Encode(map[string]any{
			"modelVersion": model,
			"candidates": []any{map[string]any{"finishReason": "STOP", "content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"text": "Here is your fox."},
				map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(tinyPNG(16, 16))}},
				map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(tinyPNG(700, 700))}},
			}}}},
		})
		return
	}
	resp := map[string]any{
		"modelVersion": model,
		"candidates": []any{map[string]any{"finishReason": "STOP", "content": map[string]any{"role": "model", "parts": []any{
			map[string]any{"text": "thinking...", "thought": true},
			map[string]any{"text": "Answer from " + model + ".", "thoughtSignature": "sig123"},
		}}}},
	}
	if tools, ok := body["tools"].([]any); ok {
		for _, tl := range tools {
			if _, ok := tl.(map[string]any)["googleSearch"]; ok {
				resp["candidates"].([]any)[0].(map[string]any)["groundingMetadata"] = map[string]any{"groundingChunks": []any{
					map[string]any{"web": map[string]any{"uri": "https://example.com/a", "title": "example.com"}},
					map[string]any{"web": map[string]any{"uri": "https://example.com/a", "title": "example.com"}},
					map[string]any{"web": map[string]any{"uri": "https://syr.edu/b", "title": "syr.edu"}},
				}}
			}
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func (m *mockGemini) uploadStart(w http.ResponseWriter, r *http.Request) {
	if !m.checkKey(w, r) {
		return
	}
	if r.Header.Get("X-Goog-Upload-Protocol") != "resumable" || r.Header.Get("X-Goog-Upload-Command") != "start" {
		m.t.Errorf("bad upload start headers: %v", r.Header)
	}
	if r.Header.Get("X-Goog-Upload-Header-Content-Type") != "video/mp4" {
		m.t.Errorf("want video/mp4 content type, got %q", r.Header.Get("X-Goog-Upload-Header-Content-Type"))
	}
	w.Header().Set("X-Goog-Upload-URL", m.srv.URL+"/upload-session/1")
	w.WriteHeader(200)
}

func (m *mockGemini) uploadBytes(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	if string(b) != "FAKEVIDEO" || r.Header.Get("X-Goog-Upload-Command") != "upload, finalize" {
		m.t.Errorf("bad upload body/headers: %q %v", b, r.Header)
	}
	m.mu.Lock()
	m.uploads++
	m.mu.Unlock()
	io.WriteString(w, `{"file":{"name":"files/abc","uri":"https://mock/files/abc","mimeType":"video/mp4","state":"PROCESSING"}}`)
}

func (m *mockGemini) getFile(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.polls++
	n := m.polls
	m.mu.Unlock()
	state := "PROCESSING"
	if n >= 2 {
		state = "ACTIVE"
	}
	io.WriteString(w, `{"name":"files/abc","uri":"https://mock/files/abc","mimeType":"video/mp4","state":"`+state+`"}`)
}

func (m *mockGemini) last() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests[len(m.requests)-1]
}

// setupEnv points every home/config location at a temp folder, so running
// the tests never touches your real key, Claude config or PATH.
func setupEnv(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("APPDATA", filepath.Join(dir, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(dir, "AppData", "Local"))
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	pollInterval = 5 * time.Millisecond
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	return dir
}

func makeDocx(t *testing.T, path string) {
	f, _ := os.Create(path)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("word/document.xml")
	io.WriteString(w, `<?xml version="1.0"?><w:document xmlns:w="x"><w:body><w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:tab/><w:t>world</w:t></w:r></w:p><w:p><w:r><w:t>Second para</w:t></w:r></w:p></w:body></w:document>`)
	zw.Close()
	f.Close()
}

func makePptx(t *testing.T, path string) {
	f, _ := os.Create(path)
	zw := zip.NewWriter(f)
	for _, s := range []struct{ name, text string }{{"ppt/slides/slide10.xml", "Ten"}, {"ppt/slides/slide2.xml", "Two"}} {
		w, _ := zw.Create(s.name)
		io.WriteString(w, `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>`+s.text+`</a:t></a:r></a:p></p:sld>`)
	}
	zw.Close()
	f.Close()
}

// ---------- tests ----------

func TestPickModel(t *testing.T) {
	var ms []ModelInfo
	for _, id := range []string{"gemini-2.5-flash", "gemini-3.5-flash", "gemini-3.7-flash", "gemini-3.10-flash-preview",
		"gemini-3.1-flash-lite", "gemini-3.1-pro-preview", "gemini-3-pro-preview", "gemini-2.5-pro",
		"gemini-3.1-flash-image", "gemini-3-pro-image", "gemini-3.1-flash-lite-image"} {
		ms = append(ms, ModelInfo{Name: "models/" + id, SupportedGenerationMethods: []string{"generateContent"}})
	}
	want := map[string]string{"flash": "gemini-3.10-flash-preview", "pro": "gemini-3.1-pro-preview", "image": "gemini-3.1-flash-image", "image-pro": "gemini-3-pro-image"}
	for tier, w := range want {
		if got := pickModel(ms, tier); got != w {
			t.Errorf("%s: got %s want %s", tier, got, w)
		}
	}
	// stable beats preview at the same version
	ms2 := []ModelInfo{{Name: "models/gemini-4-pro-preview", SupportedGenerationMethods: []string{"generateContent"}},
		{Name: "models/gemini-4-pro", SupportedGenerationMethods: []string{"generateContent"}}}
	if got := pickModel(ms2, "pro"); got != "gemini-4-pro" {
		t.Errorf("stable preference: got %s", got)
	}
	// a -latest alias wins
	ms = append(ms, ModelInfo{Name: "models/gemini-flash-latest", SupportedGenerationMethods: []string{"generateContent"}})
	if got := pickModel(ms, "flash"); got != "gemini-flash-latest" {
		t.Errorf("alias: got %s", got)
	}
}

func TestAskFull(t *testing.T) {
	dir := setupEnv(t)
	m := newMock(t)
	c := newClient("test-key", &Config{})

	notes := filepath.Join(dir, "notes.md")
	os.WriteFile(notes, []byte("# Notes\nGWT café"), 0o644)
	pic := filepath.Join(dir, "pic.png")
	os.WriteFile(pic, tinyPNG(4, 4), 0o644)
	vid := filepath.Join(dir, "lecture.mp4")
	os.WriteFile(vid, []byte("FAKEVIDEO"), 0o644)
	doc := filepath.Join(dir, "essay.docx")
	makeDocx(t, doc)
	deck := filepath.Join(dir, "deck.pptx")
	makePptx(t, deck)

	res, err := c.Ask(context.Background(), AskOptions{
		Prompt: "Summarize all of this", Files: []string{notes, pic, vid, doc, deck},
		URLs: []string{"https://youtu.be/abc123", "https://example.com/page"}, Search: true, Model: "pro", System: "Be brief.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.lastModel != "gemini-3.1-pro-preview" {
		t.Errorf("pro resolved to %s", m.lastModel)
	}
	if res.Text != "Answer from gemini-3.1-pro-preview." {
		t.Errorf("thought text leaked or text wrong: %q", res.Text)
	}
	if len(res.Sources) != 2 {
		t.Errorf("want 2 deduped sources, got %v", res.Sources)
	}
	if m.uploads != 1 || m.polls < 2 {
		t.Errorf("upload flow: uploads=%d polls=%d", m.uploads, m.polls)
	}

	body := m.last()
	b, _ := json.Marshal(body)
	s := string(b)
	for _, want := range []string{
		`GWT café`, `"inlineData":{"data":`, `"mimeType":"image/png"`,
		`"fileData":{"fileUri":"https://mock/files/abc","mimeType":"video/mp4"}`,
		`"fileData":{"fileUri":"https://youtu.be/abc123"}`, `Hello\tworld\nSecond para`,
		`[Slide 2]\nTwo\n\n[Slide 10]\nTen`, `"googleSearch":{}`, `"urlContext":{}`,
		`Read these pages:\nhttps://example.com/page`, `"systemInstruction":{"parts":[{"text":"Be brief."}]}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("request missing %s\n%s", want, s)
		}
	}
	parts := body["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	lastPart := parts[len(parts)-1].(map[string]any)
	if !strings.HasPrefix(lastPart["text"].(string), "Summarize all of this") {
		t.Errorf("prompt should come last, got %v", lastPart)
	}
	out := formatAnswer(res, true)
	if !strings.Contains(out, "Sources:\n- example.com: https://example.com/a") || !strings.Contains(out, "[Answered by gemini-3.1-pro-preview; ") {
		t.Errorf("formatAnswer: %s", out)
	}
}

func TestConversationAndDefaultFlash(t *testing.T) {
	setupEnv(t)
	m := newMock(t)
	c := newClient("test-key", &Config{})
	ctx := context.Background()
	if _, err := c.Ask(ctx, AskOptions{Prompt: "one", ConversationID: "x"}); err != nil {
		t.Fatal(err)
	}
	if m.lastModel != "gemini-3.7-flash" {
		t.Errorf("default flash resolved to %s", m.lastModel)
	}
	if _, err := c.Ask(ctx, AskOptions{Prompt: "two", ConversationID: "x"}); err != nil {
		t.Fatal(err)
	}
	contents := m.last()["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("want 3 turns, got %d", len(contents))
	}
	modelTurn, _ := json.Marshal(contents[1])
	if !strings.Contains(string(modelTurn), `"role":"model"`) || !strings.Contains(string(modelTurn), `"thoughtSignature":"sig123"`) {
		t.Errorf("model turn not preserved: %s", modelTurn)
	}
	// a different conversation starts clean
	c.Ask(ctx, AskOptions{Prompt: "three", ConversationID: "y"})
	if n := len(m.last()["contents"].([]any)); n != 1 {
		t.Errorf("new conversation has %d turns", n)
	}
}

func TestBlockedPaths(t *testing.T) {
	dir := setupEnv(t)
	newMock(t)
	c := newClient("test-key", &Config{})
	env := filepath.Join(dir, ".env")
	os.WriteFile(env, []byte("SECRET=1"), 0o644)
	cfgPath, _ := saveConfig(&Config{APIKey: "test-key"})
	for _, p := range []string{env, cfgPath} {
		if _, err := c.Ask(context.Background(), AskOptions{Prompt: "x", Files: []string{p}}); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s should be refused, got %v", p, err)
		}
	}
	other := filepath.Join(dir, "other.txt")
	os.WriteFile(other, []byte("hi"), 0o644)
	c2 := newClient("test-key", &Config{AllowedDirs: []string{filepath.Join(dir, "school")}})
	if _, err := c2.Ask(context.Background(), AskOptions{Prompt: "x", Files: []string{other}}); err == nil || !strings.Contains(err.Error(), "allowed_dirs") {
		t.Errorf("allowed_dirs not enforced: %v", err)
	}
	bin := filepath.Join(dir, "blob.bin")
	os.WriteFile(bin, []byte{0, 1, 2, 3}, 0o644)
	if _, err := c.Ask(context.Background(), AskOptions{Prompt: "x", Files: []string{bin}}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("binary should be unsupported: %v", err)
	}
}

func TestErrorsAndRefusals(t *testing.T) {
	setupEnv(t)
	m := newMock(t)
	c := newClient("test-key", &Config{})
	ctx := context.Background()

	m.status, m.errMsg = 429, "Resource exhausted. Please retry in 41.5s."
	_, err := c.Ask(ctx, AskOptions{Prompt: "x"})
	if e := explain(err); !strings.Contains(e, "rate limit") || !strings.Contains(e, "retry in about 42s") {
		t.Errorf("429: %s", e)
	}

	m.status, m.errMsg = 404, "models/gemini-3.7-flash is not found"
	_, err = c.Ask(ctx, AskOptions{Prompt: "x"})
	if !strings.Contains(explain(err), "wasn't found") || c.models != nil {
		t.Errorf("404: %s (cache cleared: %v)", explain(err), c.models == nil)
	}

	m.status = 0
	m.block = "PROHIBITED_CONTENT"
	_, err = c.Ask(ctx, AskOptions{Prompt: "x"})
	if !strings.Contains(explain(err), "own content rules") {
		t.Errorf("block: %s", explain(err))
	}

	bad := newClient("wrong-key", &Config{})
	_, err = bad.Ask(ctx, AskOptions{Prompt: "x"})
	if !strings.Contains(explain(err), "rejected the API key") {
		t.Errorf("bad key: %s", explain(err))
	}
}

func TestImage(t *testing.T) {
	dir := setupEnv(t)
	m := newMock(t)
	c := newClient("test-key", &Config{ImageDir: filepath.Join(dir, "imgs")})
	in := filepath.Join(dir, "in.png")
	os.WriteFile(in, tinyPNG(8, 8), 0o644)
	res, err := c.Image(context.Background(), ImageOptions{Prompt: "a fox", InputImages: []string{in}, AspectRatio: "16:9"})
	if err != nil {
		t.Fatal(err)
	}
	if m.lastModel != "gemini-3.1-flash-image" {
		t.Errorf("image resolved to %s", m.lastModel)
	}
	if len(res.Paths) != 2 || len(res.Previews) != 2 {
		t.Fatalf("paths=%v previews=%d", res.Paths, len(res.Previews))
	}
	for _, p := range res.Paths {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			t.Errorf("image not saved: %s", p)
		}
	}
	big, _ := os.Stat(res.Paths[1])
	if pv, _ := base64.StdEncoding.DecodeString(res.Previews[1].Data); int64(len(pv)) >= big.Size() && big.Size() > previewLimit {
		t.Errorf("large preview wasn't shrunk")
	}
	b, _ := json.Marshal(m.last())
	if !strings.Contains(string(b), `"responseModalities":["TEXT","IMAGE"]`) || !strings.Contains(string(b), `"aspectRatio":"16:9"`) {
		t.Errorf("image request: %s", b)
	}
	if res.Text != "Here is your fox." {
		t.Errorf("text: %q", res.Text)
	}
}

func TestParseAskArgs(t *testing.T) {
	a, err := parseAskArgs([]string{"summarize", "-f", "a.pdf", "this", "-s", "--model", "pro", "-u", "https://x", "--", "-5 degrees"})
	if err != nil {
		t.Fatal(err)
	}
	if a.opts.Prompt != "summarize this -5 degrees" || a.opts.Files[0] != "a.pdf" || !a.opts.Search || a.opts.Model != "pro" || a.opts.URLs[0] != "https://x" {
		t.Errorf("parsed: %+v", a)
	}
	if _, err := parseAskArgs([]string{"-x"}); err == nil {
		t.Error("unknown flag should fail")
	}
	if _, err := parseAskArgs([]string{"-f"}); err == nil {
		t.Error("missing value should fail")
	}
}

func TestClaudeConfigMerge(t *testing.T) {
	dir := setupEnv(t)
	p := filepath.Join(dir, "Claude", "claude_desktop_config.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	orig := `{"mcpServers":{"Blackboard (Syracuse)":{"command":"node","args":["bb.js"]}},"globalShortcut":"Ctrl+Space","big":12345678901234567890}`
	os.WriteFile(p, []byte(orig), 0o644)
	if err := mergeClaudeConfig(p, `C:\Users\Henry\AppData\Local\Programs\gemini-bridge\gemini-bridge.exe`); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	for _, want := range []string{`"Blackboard (Syracuse)"`, `"globalShortcut": "Ctrl+Space"`, `"gemini": {`, `"serve"`, `12345678901234567890`, `gemini-bridge.exe`} {
		if !strings.Contains(s, want) {
			t.Errorf("merged config missing %s:\n%s", want, s)
		}
	}
	backups, _ := filepath.Glob(p + ".bak-*")
	if len(backups) != 1 {
		t.Errorf("want 1 backup, got %v", backups)
	}
	// invalid JSON is left untouched
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o644)
	if err := mergeClaudeConfig(bad, "x"); err == nil {
		t.Error("invalid JSON should error")
	}
	if b, _ := os.ReadFile(bad); string(b) != "{not json" {
		t.Error("invalid file was modified")
	}
	// missing file gets created
	fresh := filepath.Join(dir, "new", "claude_desktop_config.json")
	if err := mergeClaudeConfig(fresh, "x"); err != nil {
		t.Fatal(err)
	}
	// and the path finder looks where Claude desktop keeps its config
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", dir)
		la := filepath.Join(dir, "Local")
		t.Setenv("LOCALAPPDATA", la)
		if got := claudeDesktopConfigPaths(); len(got) != 1 || got[0] != p {
			t.Errorf("config paths: %v", got)
		}
		// Microsoft Store installs keep a second copy under Packages
		msix := filepath.Join(la, "Packages", "Claude_pzs8sxrjxfjjc", "LocalCache", "Roaming", "Claude")
		os.MkdirAll(msix, 0o755)
		got := claudeDesktopConfigPaths()
		if len(got) != 2 || got[1] != filepath.Join(msix, "claude_desktop_config.json") {
			t.Errorf("MSIX config paths: %v", got)
		}
	case "linux":
		t.Setenv("XDG_CONFIG_HOME", dir)
		if got := claudeDesktopConfigPaths(); len(got) != 1 || got[0] != p {
			t.Errorf("config paths: %v", got)
		}
	}
}

// TestSetupNonInteractive runs the real setup with typed answers piped in:
// key, then "no" to PATH (Windows only), "yes" to Claude desktop, "no" to the rest.
func TestSetupNonInteractive(t *testing.T) {
	dir := setupEnv(t)
	newMock(t)

	answers := "test-key\ny\nn\nn\n"
	if runtime.GOOS == "windows" {
		answers = "test-key\nn\ny\nn\nn\n"
	}
	inR, inW, _ := os.Pipe()
	io.WriteString(inW, answers)
	inW.Close()
	outR, outW, _ := os.Pipe()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	var printed bytes.Buffer
	copied := make(chan struct{})
	go func() { io.Copy(&printed, outR); close(copied) }()

	code := runSetup()

	os.Stdin, os.Stdout = oldIn, oldOut
	outW.Close()
	<-copied
	inR.Close()
	out := printed.String()
	if code != 0 || !strings.Contains(out, "All done.") {
		t.Fatalf("setup exit %d:\n%s", code, out)
	}

	cfg, err := loadConfigFile()
	if err != nil || cfg.APIKey != "test-key" {
		t.Errorf("key not saved: %+v %v", cfg, err)
	}
	inst, _ := installDir()
	exe := filepath.Join(inst, "gemini-bridge")
	if runtime.GOOS == "windows" {
		exe += ".exe"
		shim, err := os.ReadFile(filepath.Join(inst, "gem.cmd"))
		if err != nil || string(shim) != "@echo off\r\n\"%~dp0gemini-bridge.exe\" gem %*\r\n" {
			t.Errorf("gem.cmd: %q %v", shim, err)
		}
		if strings.Contains(os.Getenv("PATH"), inst) {
			t.Error("PATH was changed even though the answer was no")
		}
	}
	if _, err := os.Stat(exe); err != nil {
		t.Errorf("exe not installed: %v", err)
	}
	paths := claudeDesktopConfigPaths()
	if len(paths) == 0 {
		t.Fatal("no Claude config path")
	}
	b, err := os.ReadFile(paths[0])
	if err != nil || !strings.Contains(string(b), `"gemini"`) || !strings.Contains(string(b), "serve") {
		t.Errorf("Claude config not registered at %s: %s %v", paths[0], b, err)
	}
	if !strings.HasPrefix(paths[0], dir) {
		t.Errorf("setup wrote outside the temp folder: %s", paths[0])
	}
}

func TestMCPServer(t *testing.T) {
	dir := setupEnv(t)
	newMock(t)
	factory := func() (*Client, error) {
		return newClient("test-key", &Config{ImageDir: filepath.Join(dir, "imgs")}), nil
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := newMCPServer(outW, factory)
	done := make(chan error, 1)
	go func() { done <- s.serve(inR); outW.Close() }()

	responses := map[string]map[string]any{}
	var rmu sync.Mutex
	got := make(chan struct{}, 10)
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var msg map[string]any
			if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
				t.Errorf("server wrote non-JSON: %s", sc.Text())
				continue
			}
			if id, ok := msg["id"]; ok {
				b, _ := json.Marshal(id)
				rmu.Lock()
				responses[string(b)] = msg
				rmu.Unlock()
				got <- struct{}{}
			}
		}
	}()

	send := func(s string) { io.WriteString(inW, s+"\n") }
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask_gemini","arguments":{"prompt":"hi","search":true},"_meta":{"progressToken":"p1"}}}`)
	send(`{"jsonrpc":"2.0","id":"img","method":"tools/call","params":{"name":"gemini_generate_image","arguments":{"prompt":"fox"}}}`)
	send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"gemini_list_models","arguments":{}}}`)
	send(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"ask_gemini","arguments":{}}}`)
	send(`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	send(`{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	send(`not json`)

	for i := 0; i < 9; i++ {
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out after %d responses", i)
		}
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	rmu.Lock()
	defer rmu.Unlock()
	init := responses["1"]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-11-25" || !strings.Contains(init["instructions"].(string), "gemini-bridge") {
		t.Errorf("initialize: %v", init)
	}
	tools := responses["2"]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 {
		t.Errorf("want 3 tools, got %d", len(tools))
	}
	ask := responses["3"]["result"].(map[string]any)
	askText := ask["content"].([]any)[0].(map[string]any)["text"].(string)
	if ask["isError"] == true || !strings.Contains(askText, "Answer from gemini-3.7-flash.") || !strings.Contains(askText, "Sources:") {
		t.Errorf("ask_gemini: %v", ask)
	}
	img := responses[`"img"`]["result"].(map[string]any)["content"].([]any)
	if len(img) != 3 || img[1].(map[string]any)["type"] != "image" {
		t.Errorf("image result: %d blocks", len(img))
	}
	models := responses["5"]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(models, "flash      -> gemini-3.7-flash") || strings.Contains(models, "live") {
		t.Errorf("models: %s", models)
	}
	if responses["6"]["result"].(map[string]any)["isError"] != true {
		t.Error("missing prompt should be a tool error")
	}
	if responses["7"]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Error("unknown method should be -32601")
	}
	if _, ok := responses["null"]; !ok {
		t.Error("bad JSON should get a parse error")
	}
}

func TestNoKeyMessage(t *testing.T) {
	setupEnv(t)
	s := newMCPServer(io.Discard, newClientFromConfig)
	res := s.callTool(context.Background(), "ask_gemini", json.RawMessage(`{"prompt":"hi"}`))
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] != true || !strings.Contains(text, "gemini-bridge setup") {
		t.Errorf("no-key: %v", res)
	}
	// once a key is saved the same server picks it up without restarting
	newMock(t)
	saveConfig(&Config{APIKey: "test-key"})
	res = s.callTool(context.Background(), "ask_gemini", json.RawMessage(`{"prompt":"hi"}`))
	if res["isError"] == true {
		t.Errorf("after setup: %v", res)
	}
}

// Real Google messages seen on a free key, used by the fallback tests.
const (
	msgOverloaded  = "This model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later."
	msgProZero     = "You exceeded your current quota, please check your plan and billing details.\n* Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 0, model: gemini-3.1-pro\nPlease retry in 14h9m53.422391512s."
	msgImageZero   = "You exceeded your current quota, please check your plan and billing details.\n* Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 0, model: gemini-3.1-flash-image\nPlease retry in 14h8m24.694051182s."
	msgSearchQuota = "You exceeded your current quota, please check your plan and billing details. For more information on this error, head to: https://aistudio.google.com/docs/rate-limits."
)

func TestFallbackWhenFlashIsOverloaded(t *testing.T) {
	setupEnv(t)
	m := newMock(t)
	m.modelErr = map[string]mockErr{"gemini-3.7-flash": {503, msgOverloaded}}
	c := newClient("test-key", &Config{})
	res, err := c.Ask(context.Background(), AskOptions{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "gemini-3.1-flash-lite" || res.Text != "Answer from gemini-3.1-flash-lite." {
		t.Errorf("fallback answered with %s: %q", res.Model, res.Text)
	}
	if len(res.Notes) != 1 || res.Notes[0] != "flash was overloaded at Google, so flash-lite answered instead" {
		t.Errorf("notes: %v", res.Notes)
	}
	// the overloaded model was retried (3 attempts) before falling back
	if n := strings.Count(strings.Join(m.tried, ","), "gemini-3.7-flash"); n != 3 {
		t.Errorf("flash attempts: %d (%v)", n, m.tried)
	}
}

func TestFallbackWhenProIsNotIncluded(t *testing.T) {
	setupEnv(t)
	m := newMock(t)
	m.modelErr = map[string]mockErr{"gemini-3.1-pro-preview": {429, msgProZero}}
	c := newClient("test-key", &Config{})
	res, err := c.Ask(context.Background(), AskOptions{Prompt: "hi", Model: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "gemini-3.7-flash" || res.Notes[0] != "pro was not included on this API key, so flash answered instead" {
		t.Errorf("got %s, notes %v", res.Model, res.Notes)
	}
	if strings.Count(strings.Join(m.tried, ","), "pro") != 1 {
		t.Errorf("a limit-0 model should not be retried: %v", m.tried)
	}
}

func TestExactModelDoesNotFallBack(t *testing.T) {
	setupEnv(t)
	m := newMock(t)
	m.modelErr = map[string]mockErr{"gemini-3.5-flash": {503, msgOverloaded}}
	c := newClient("test-key", &Config{})
	_, err := c.Ask(context.Background(), AskOptions{Prompt: "hi", Model: "gemini-3.5-flash"})
	if err == nil || !strings.Contains(explain(err), "overloaded right now") {
		t.Fatalf("want overload error, got %v", err)
	}
	for _, tried := range m.tried {
		if tried != "gemini-3.5-flash" {
			t.Errorf("exact model fell back to %s", tried)
		}
	}
}

func TestEverythingOverloaded(t *testing.T) {
	setupEnv(t)
	m := newMock(t)
	m.status, m.errMsg = 503, msgOverloaded
	c := newClient("test-key", &Config{})
	_, err := c.Ask(context.Background(), AskOptions{Prompt: "hi", Model: "pro"})
	e := explain(err)
	if !strings.Contains(e, "overloaded right now") || !strings.Contains(e, "high demand") {
		t.Errorf("explain: %s", e)
	}
	if got := strings.Join(m.tried, ","); !strings.Contains(got, "pro") || !strings.Contains(got, "gemini-3.7-flash") || !strings.Contains(got, "lite") {
		t.Errorf("chain not followed: %s", got)
	}
}

func TestSearchNotAvailable(t *testing.T) {
	setupEnv(t)
	m := newMock(t)
	m.searchErr = &mockErr{429, msgSearchQuota}
	c := newClient("test-key", &Config{})
	_, err := c.Ask(context.Background(), AskOptions{Prompt: "weather?", Search: true})
	e := explain(err)
	if !strings.Contains(e, "Google Search option isn't available") || !strings.Contains(e, "Claude can search the web itself") {
		t.Errorf("explain: %s", e)
	}
	// without search the same key still answers
	if _, err := c.Ask(context.Background(), AskOptions{Prompt: "weather?"}); err != nil {
		t.Errorf("plain ask failed: %v", err)
	}
}

func TestImageNotIncluded(t *testing.T) {
	dir := setupEnv(t)
	m := newMock(t)
	m.modelErr = map[string]mockErr{"gemini-3.1-flash-image": {429, msgImageZero}}
	c := newClient("test-key", &Config{ImageDir: filepath.Join(dir, "imgs")})
	_, err := c.Image(context.Background(), ImageOptions{Prompt: "a fox"})
	e := explain(err)
	if !strings.HasPrefix(e, "Image generation (gemini-3.1-flash-image) isn't included on this API key") || !strings.Contains(e, "Don't retry") {
		t.Errorf("explain: %s", e)
	}
	if strings.Contains(e, "14h") {
		t.Errorf("a limit-0 message shouldn't suggest waiting: %s", e)
	}
}

func TestFlashLiteShortcut(t *testing.T) {
	setupEnv(t)
	m := newMock(t)
	c := newClient("test-key", &Config{})
	if _, err := c.Ask(context.Background(), AskOptions{Prompt: "hi", Model: "flash-lite"}); err != nil {
		t.Fatal(err)
	}
	if m.lastModel != "gemini-3.1-flash-lite" {
		t.Errorf("flash-lite resolved to %s", m.lastModel)
	}
}
