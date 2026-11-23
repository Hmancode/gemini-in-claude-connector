package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"
)

type askArgs struct {
	opts    AskOptions
	inFile  string
	outFile string
}

// parseAskArgs allows flags anywhere: `gem "question" -f a.pdf` works too.
func parseAskArgs(args []string) (askArgs, error) {
	var a askArgs
	var words []string
	need := func(i int, flag string) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", flag)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-m", "--model", "-f", "--file", "-u", "--url", "--system", "--in", "--out":
			v, err := need(i, arg)
			if err != nil {
				return a, err
			}
			i++
			switch arg {
			case "-m", "--model":
				a.opts.Model = v
			case "-f", "--file":
				a.opts.Files = append(a.opts.Files, v)
			case "-u", "--url":
				a.opts.URLs = append(a.opts.URLs, v)
			case "--system":
				a.opts.System = v
			case "--in":
				a.inFile = v
			case "--out":
				a.outFile = v
			}
		case "-s", "--search":
			a.opts.Search = true
		case "--":
			words = append(words, args[i+1:]...)
			i = len(args)
		default:
			if len(arg) > 1 && strings.HasPrefix(arg, "-") {
				return a, fmt.Errorf("unknown option %s (put text that starts with - after --)", arg)
			}
			words = append(words, arg)
		}
	}
	a.opts.Prompt = strings.Join(words, " ")
	return a, nil
}

func joinPrompt(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n---\n" + b
}

func cliContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	return ctx, func() { stop(); cancel() }
}

func runAsk(args []string) int {
	a, err := parseAskArgs(args)
	fail := func(msg string) int {
		fmt.Fprintln(os.Stderr, msg)
		if a.outFile != "" {
			_ = os.WriteFile(a.outFile, []byte("Error: "+msg+"\n"), 0o644)
		}
		return 1
	}
	if err != nil {
		return fail(err.Error())
	}
	if a.inFile != "" {
		b, err := os.ReadFile(a.inFile)
		if err != nil {
			return fail("can't read " + a.inFile + ": " + err.Error())
		}
		a.opts.Prompt = joinPrompt(a.opts.Prompt, string(b))
	} else if !stdinIsTerminal() {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, 20<<20))
		a.opts.Prompt = joinPrompt(a.opts.Prompt, string(b))
	}
	if strings.TrimSpace(a.opts.Prompt) == "" && len(a.opts.Files) == 0 && len(a.opts.URLs) == 0 {
		printGemUsage()
		return 2
	}
	c, err := newClientFromConfig()
	if err != nil {
		return fail(explain(err))
	}
	ctx, cancel := cliContext()
	defer cancel()
	if a.outFile == "" && stdinIsTerminal() {
		fmt.Fprintln(os.Stderr, "Asking Gemini...")
	}
	res, err := c.Ask(ctx, a.opts)
	if err != nil {
		return fail(explain(err))
	}
	text := formatAnswer(res, false)
	if a.outFile == "" {
		for _, n := range res.Notes {
			fmt.Fprintln(os.Stderr, "("+n+")")
		}
	}
	if a.outFile != "" {
		if err := os.WriteFile(a.outFile, []byte(text+"\n"), 0o644); err != nil {
			return fail(err.Error())
		}
		return 0
	}
	fmt.Println(text)
	return 0
}

func runImage(args []string) int {
	var o ImageOptions
	var words []string
	open := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-i", "--input", "-o", "--out-dir", "-m", "--model", "-a", "--aspect":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, arg, "needs a value")
				return 2
			}
			v := args[i+1]
			i++
			switch arg {
			case "-i", "--input":
				o.InputImages = append(o.InputImages, v)
			case "-o", "--out-dir":
				o.OutDir = v
			case "-m", "--model":
				o.Model = v
			case "-a", "--aspect":
				o.AspectRatio = v
			}
		case "--open":
			open = true
		default:
			words = append(words, arg)
		}
	}
	o.Prompt = strings.Join(words, " ")
	if strings.TrimSpace(o.Prompt) == "" {
		fmt.Fprintln(os.Stderr, `usage: gem image [-i input.png] [-a 16:9] [-m image-pro] [-o folder] [--open] "description"`)
		return 2
	}
	c, err := newClientFromConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, explain(err))
		return 1
	}
	ctx, cancel := cliContext()
	defer cancel()
	fmt.Fprintln(os.Stderr, "Asking Gemini for an image...")
	res, err := c.Image(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, explain(err))
		return 1
	}
	for _, p := range res.Paths {
		fmt.Println(p)
	}
	if res.Text != "" {
		fmt.Fprintln(os.Stderr, res.Text)
	}
	if open && len(res.Paths) > 0 {
		openFile(res.Paths[0])
	}
	return 0
}

func openFile(p string) {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("cmd", "/c", "start", "", p).Start()
	case "darwin":
		_ = exec.Command("open", p).Start()
	default:
		_ = exec.Command("xdg-open", p).Start()
	}
}

func runModels() int {
	c, err := newClientFromConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, explain(err))
		return 1
	}
	ctx, cancel := cliContext()
	defer cancel()
	text, err := modelsReport(ctx, c)
	if err != nil {
		fmt.Fprintln(os.Stderr, explain(err))
		return 1
	}
	fmt.Println(text)
	return 0
}
