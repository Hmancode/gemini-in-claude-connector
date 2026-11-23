// gemini-bridge connects Claude (and your keyboard) to Google's Gemini models.
//
//	gemini-bridge serve      MCP server over stdio (what Claude runs)
//	gemini-bridge ask ...    one-shot question from a terminal or script
//	gemini-bridge image ...  generate or edit an image
//	gemini-bridge models     list models and what "flash"/"pro"/"image" point to
//	gemini-bridge setup      save your API key and connect everything
//
// Double-clicking the .exe runs setup.
package main

import (
	"fmt"
	"os"
)

// version is overridden at release build time with -ldflags "-X main.version=..."
var version = "0.1.0-dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		// Claude launches us with piped stdin; a person double-clicking gets a console.
		if stdinIsTerminal() {
			return runSetup()
		}
		return runServe()
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return runServe()
	case "ask":
		return runAsk(rest)
	case "image":
		return runImage(rest)
	case "models":
		return runModels()
	case "setup", "install":
		return runSetup()
	case "gem":
		return runGem(rest)
	case "version", "--version", "-v":
		fmt.Println("gemini-bridge", version)
		return 0
	case "help", "--help", "-h", "/?":
		printUsage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "Unknown command %q.\n\n", cmd)
	printUsage()
	return 2
}

// runGem is what the `gem` shortcut calls: `gem <question>` asks, while
// `gem image ...`, `gem models` and `gem setup` reach the other commands.
func runGem(args []string) int {
	if len(args) == 0 {
		printGemUsage()
		return 0
	}
	switch args[0] {
	case "image":
		return runImage(args[1:])
	case "models":
		return runModels()
	case "setup":
		return runSetup()
	case "help", "-h", "--help", "/?":
		printGemUsage()
		return 0
	}
	return runAsk(args)
}

func stdinIsTerminal() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func printUsage() {
	fmt.Print(`gemini-bridge ` + version + ` - connects Claude and your keyboard to Google Gemini

Commands:
  setup     Save your Gemini API key and connect Claude, the gem command and hotkeys
  ask       Ask Gemini something:   gemini-bridge ask [options] "question"
  image     Make an image:          gemini-bridge image [options] "description"
  models    Show available models and what "flash", "pro" and "image" point to
  serve     Run as an MCP server (Claude starts this for you)

Run "gem help" for the short form.
`)
}

func printGemUsage() {
	fmt.Print(`gem - ask Gemini from any terminal

  gem "question"                      ask (fast "flash" model)
  gem -m pro "question"               use the strongest model
  gem -f lecture.mp4 "summarize"      attach files (PDF, images, audio, video, text, .docx, .pptx)
  gem -u https://youtu.be/... "notes" read a web page or watch a YouTube video
  gem -s "question"                   ground the answer in Google Search
  Get-Content notes.txt | gem "fix the grammar"     pipe text in

  gem image "a watercolor fox"        generate an image (saved to Pictures\Gemini)
  gem image -i photo.png "make it night"            edit an image
  gem models                          list models
  gem setup                           change the API key or reconnect things
`)
}
