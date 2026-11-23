package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Sent to every Claude session that connects, so each one knows what the
// bridge is for without being told in conversation.
const serverInstructions = `gemini-bridge gives you Google's Gemini models through the user's own Gemini API key.
Use it when the user asks for Gemini or a second opinion, or when Gemini adds something you lack: watching video and listening to audio (lecture recordings, voice memos, YouTube links), reading very large files without loading them into your own context, generating or editing images, a Google-Search-grounded answer, or an independent cross-check of a fact, calculation or piece of code.
Gemini sees only what you send, so put the context it needs in the prompt. Files are read on the user's computer and sent to Google, so send only files the user has pointed you to.
Treat Gemini's answers as another model's work: check them before relying on them, and tell the user which parts came from Gemini.`

const toolsJSON = `[
 {
  "name": "ask_gemini",
  "title": "Ask Gemini",
  "description": "Send a request to Google Gemini and return its answer. Accepts local files (PDF, images, audio, video, text and code, .docx, .pptx), web pages and YouTube links, and can ground the answer in Google Search. Large files and all audio/video are uploaded through Google's Files API automatically. Reuse a conversation_id to keep a multi-turn thread with Gemini.",
  "inputSchema": {
   "type": "object",
   "properties": {
    "prompt": {"type": "string", "description": "The full request. Gemini sees nothing from this conversation except what you put here and in files/urls, so include the context it needs."},
    "files": {"type": "array", "items": {"type": "string"}, "description": "Absolute paths of files on the user's computer to include."},
    "urls": {"type": "array", "items": {"type": "string"}, "description": "Web pages for Gemini to read, or YouTube links for it to watch."},
    "search": {"type": "boolean", "description": "Ground the answer in Google Search and return the sources used."},
    "model": {"type": "string", "description": "\"flash\" (default, fast), \"pro\" (strongest reasoning, tighter free-tier limits), or an exact model id from gemini_list_models."},
    "system": {"type": "string", "description": "Optional system instruction, such as a role or output format."},
    "conversation_id": {"type": "string", "description": "Any short label. Reuse it to continue the same thread; Gemini keeps earlier turns until the Claude app restarts."}
   },
   "required": ["prompt"]
  },
  "annotations": {"readOnlyHint": true, "openWorldHint": true}
 },
 {
  "name": "gemini_generate_image",
  "title": "Generate image with Gemini",
  "description": "Generate or edit images with Gemini's image model. Saves the images on the user's computer (default: Pictures\\Gemini) and returns their paths plus a preview. To edit, pass existing images in input_images and describe the change in the prompt.",
  "inputSchema": {
   "type": "object",
   "properties": {
    "prompt": {"type": "string", "description": "What to make, or what to change in the input images."},
    "input_images": {"type": "array", "items": {"type": "string"}, "description": "Optional absolute paths of images to edit or use as reference."},
    "aspect_ratio": {"type": "string", "description": "Optional, such as \"1:1\", \"16:9\", \"9:16\", \"4:3\"."},
    "output_dir": {"type": "string", "description": "Optional folder to save into."},
    "model": {"type": "string", "description": "\"image\" (default) or \"image-pro\" (higher quality, slower), or an exact model id."}
   },
   "required": ["prompt"]
  },
  "annotations": {"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true}
 },
 {
  "name": "gemini_list_models",
  "title": "List Gemini models",
  "description": "List the Gemini models this API key can use, and which models the shortcuts \"flash\", \"pro\", \"image\" and \"image-pro\" currently point to.",
  "inputSchema": {"type": "object", "properties": {}},
  "annotations": {"readOnlyHint": true, "openWorldHint": true}
 }
]`

var toolDefs = func() []any {
	var t []any
	if err := json.Unmarshal([]byte(toolsJSON), &t); err != nil {
		panic("bad tool definitions: " + err.Error())
	}
	return t
}()

type mcpServer struct {
	out     io.Writer
	wmu     sync.Mutex
	factory func() (*Client, error)

	cmu    sync.Mutex
	client *Client

	imu      sync.Mutex
	inflight map[string]context.CancelFunc
	wg       sync.WaitGroup
}

func newMCPServer(out io.Writer, factory func() (*Client, error)) *mcpServer {
	return &mcpServer{out: out, factory: factory, inflight: map[string]context.CancelFunc{}}
}

func runServe() int {
	log.SetOutput(os.Stderr) // stdout is reserved for the protocol
	log.SetPrefix("gemini-bridge: ")
	s := newMCPServer(os.Stdout, newClientFromConfig)
	if err := s.serve(os.Stdin); err != nil {
		log.Println(err)
		return 1
	}
	return 0
}

func (s *mcpServer) serve(in io.Reader) error {
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.handleLine(line)
		}
		if err != nil {
			s.imu.Lock()
			for _, cancel := range s.inflight {
				cancel()
			}
			s.imu.Unlock()
			s.wg.Wait()
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (s *mcpServer) send(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		log.Println("encode:", err)
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.out.Write(append(b, '\n'))
}

func (s *mcpServer) result(id json.RawMessage, res any) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
}

func (s *mcpServer) fail(id json.RawMessage, code int, msg string) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (s *mcpServer) handleLine(line []byte) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		s.fail(json.RawMessage("null"), -32700, "parse error")
		return
	}
	isRequest := len(msg.ID) > 0 && string(msg.ID) != "null"
	switch msg.Method {
	case "":
		// a response to something we sent; we never send requests
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		s.result(msg.ID, map[string]any{
			"protocolVersion": firstNonEmpty(p.ProtocolVersion, "2025-06-18"),
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "gemini-bridge", "title": "Gemini Bridge", "version": version},
			"instructions":    serverInstructions,
		})
	case "ping":
		s.result(msg.ID, map[string]any{})
	case "tools/list":
		s.result(msg.ID, map[string]any{"tools": toolDefs})
	case "tools/call":
		if !isRequest {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleCall(msg.ID, msg.Params)
		}()
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			s.imu.Lock()
			if cancel, ok := s.inflight[string(p.RequestID)]; ok {
				cancel()
			}
			s.imu.Unlock()
		}
	default:
		if isRequest {
			s.fail(msg.ID, -32601, "method not found: "+msg.Method)
		}
	}
}

func (s *mcpServer) getClient() (*Client, error) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if s.client != nil {
		return s.client, nil
	}
	c, err := s.factory() // re-read config until it works, so setup takes effect without a restart
	if err != nil {
		return nil, err
	}
	s.client = c
	return c, nil
}

func (s *mcpServer) handleCall(id json.RawMessage, params json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		s.fail(id, -32602, "invalid params")
		return
	}
	if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
		p.Arguments = json.RawMessage("{}")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	key := string(id)
	s.imu.Lock()
	s.inflight[key] = cancel
	s.imu.Unlock()
	defer func() {
		s.imu.Lock()
		delete(s.inflight, key)
		s.imu.Unlock()
		cancel()
	}()

	stop := func() {}
	if tok := p.Meta.ProgressToken; len(tok) > 0 && string(tok) != "null" {
		stop = s.startProgress(ctx, tok)
	}
	res := s.callTool(ctx, p.Name, p.Arguments)
	stop()
	if errors.Is(ctx.Err(), context.Canceled) {
		return // the client cancelled; the spec says not to answer
	}
	s.result(id, res)
}

// startProgress sends a heartbeat every 10s so long video jobs don't look stuck.
func (s *mcpServer) startProgress(ctx context.Context, token json.RawMessage) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for n := 1; ; n++ {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{
					"progressToken": token, "progress": n, "message": fmt.Sprintf("Waiting for Gemini (%ds)", n*10),
				}})
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func textContent(t string) map[string]any { return map[string]any{"type": "text", "text": t} }

func toolError(msg string) map[string]any {
	return map[string]any{"content": []any{textContent(msg)}, "isError": true}
}

func (s *mcpServer) callTool(ctx context.Context, name string, args json.RawMessage) map[string]any {
	switch name {
	case "ask_gemini":
		var a struct {
			Prompt         string   `json:"prompt"`
			Files          []string `json:"files"`
			URLs           []string `json:"urls"`
			Search         bool     `json:"search"`
			Model          string   `json:"model"`
			System         string   `json:"system"`
			ConversationID string   `json:"conversation_id"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return toolError("Invalid arguments: " + err.Error())
		}
		if strings.TrimSpace(a.Prompt) == "" {
			return toolError("prompt is required")
		}
		c, err := s.getClient()
		if err != nil {
			return toolError(explain(err))
		}
		res, err := c.Ask(ctx, AskOptions{Prompt: a.Prompt, Files: a.Files, URLs: a.URLs, Search: a.Search,
			Model: a.Model, System: a.System, ConversationID: a.ConversationID})
		if err != nil {
			return toolError(explain(err))
		}
		return map[string]any{"content": []any{textContent(formatAnswer(res, true))}}

	case "gemini_generate_image":
		var a struct {
			Prompt      string   `json:"prompt"`
			InputImages []string `json:"input_images"`
			AspectRatio string   `json:"aspect_ratio"`
			OutputDir   string   `json:"output_dir"`
			Model       string   `json:"model"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return toolError("Invalid arguments: " + err.Error())
		}
		c, err := s.getClient()
		if err != nil {
			return toolError(explain(err))
		}
		res, err := c.Image(ctx, ImageOptions{Prompt: a.Prompt, InputImages: a.InputImages, OutDir: a.OutputDir,
			Model: a.Model, AspectRatio: a.AspectRatio})
		if err != nil {
			return toolError(explain(err))
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Saved %d image(s) from %s:\n", len(res.Paths), res.Model)
		for _, p := range res.Paths {
			b.WriteString("- " + p + "\n")
		}
		if res.Text != "" {
			b.WriteString("\nGemini's note: " + res.Text)
		}
		content := []any{textContent(strings.TrimSpace(b.String()))}
		for _, pv := range res.Previews {
			content = append(content, map[string]any{"type": "image", "data": pv.Data, "mimeType": pv.MimeType})
		}
		return map[string]any{"content": content}

	case "gemini_list_models":
		c, err := s.getClient()
		if err != nil {
			return toolError(explain(err))
		}
		text, err := modelsReport(ctx, c)
		if err != nil {
			return toolError(explain(err))
		}
		return map[string]any{"content": []any{textContent(text)}}
	}
	return toolError("Unknown tool: " + name)
}

func modelsReport(ctx context.Context, c *Client) (string, error) {
	tiers, err := c.TierSummary(ctx)
	if err != nil {
		return "", err
	}
	models, _ := c.ListModels(ctx, false)
	var b strings.Builder
	b.WriteString("Shortcuts right now:\n")
	for _, t := range tiers {
		fmt.Fprintf(&b, "  %-10s -> %s\n", t[0], t[1])
	}
	b.WriteString("\nAll models that can answer requests:\n")
	for _, m := range models {
		if m.CanGenerate() {
			fmt.Fprintf(&b, "  %s\n", m.ID())
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
