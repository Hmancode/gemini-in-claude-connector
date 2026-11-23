# Gemini in Claude Connector

[![CI](https://github.com/Hmancode/gemini-in-claude-connector/actions/workflows/ci.yml/badge.svg)](https://github.com/Hmancode/gemini-in-claude-connector/actions/workflows/ci.yml)

`gemini-bridge` connects Claude to Google's Gemini models and adds keyboard shortcuts on Windows. It's a single .exe built only from the Go standard library.

| What | How |
|---|---|
| Win+C | Show or hide the Claude desktop app (opens it if it's closed) |
| Win+G | Show or hide Gemini in its own window |
| Win+Shift+G | Send the selected text (or your clipboard) to Gemini and show the answer in a popup |
| `gem "question"` | Ask Gemini from any terminal |
| Inside Claude | "Ask Gemini…", "have Gemini watch this video…", "get a second opinion from Gemini…", "make an image with Gemini…" |

## Install (Windows, about 5 minutes)

1. **Get a Gemini API key:** go to https://aistudio.google.com/apikey and choose **Create API key**.
2. **For the hotkeys, install AutoHotkey v2** (free) from https://www.autohotkey.com. Choose v2, not v1.
3. **Download the files:** get `gemini-bridge-windows-x64.zip` from the [latest release](https://github.com/Hmancode/gemini-in-claude-connector/releases/latest), right-click it and choose **Extract All**.
4. **Run setup:** double-click `gemini-bridge.exe`. The program isn't code-signed, so Windows may say "Windows protected your PC". If so, click **More info → Run anyway**. Setup will:
   - ask for your key and check it with Google
   - install the bridge to `%LOCALAPPDATA%\Programs\gemini-bridge`
   - offer to add the `gem` command
   - offer to connect the bridge to the Claude desktop app, keeping your other connected tools and backing up the old config
   - offer to connect it to Claude Code, if Claude Code is installed
   - offer to turn on the hotkeys and start them when you sign in
5. **Fully quit Claude** (right-click its tray icon → Quit) and reopen it. To check it worked, ask Claude: *"what Gemini models can you reach?"*

Run `gem setup` (or double-click the exe again) at any time to change the key or reconnect.

## What Claude gets

Claude gets three tools: **ask_gemini**, **gemini_generate_image** and **gemini_list_models**. It also gets a short note saying when they're useful, so any Claude chat connected to the bridge knows what it's for.

- **ask_gemini** works with text plus local files: PDF, images, audio, video, text and code, .docx and .pptx. It also takes web pages and YouTube links, can base its answer on Google Search, and can hold multi-turn threads (`conversation_id`). Audio, video and big files are uploaded through Google's Files API automatically.
- **gemini_generate_image** creates or edits images and saves them to `Pictures\Gemini`.
- **Model shortcuts:** `flash` (the default, fast), `pro` (strongest), `image` and `image-pro`. These are matched to Google's live model list, so they keep working when Google renames models. `gem models` shows where each one points.

**Which Claude chats can use it:**
- Claude desktop app on the same computer: yes.
- Cloud chats linked to that computer: yes, while the desktop app is open.
- Claude Code: yes, if you chose that during setup.
- Claude on a phone, or claude.ai with no computer link: no.

## Terminal examples

```
gem "explain global workspace theory in 3 sentences"
gem -m pro "check this proof for errors" -f proof.pdf
gem -f lecture.mp4 "summarize this lecture with timestamps"
gem -u https://www.youtube.com/watch?v=... "list the main claims"
gem -s "what changed in the latest Gemini API release?"
Get-Content draft.txt | gem "tighten this paragraph"
gem image -a 16:9 "watercolor of a college quad in fall"
gem image -i photo.jpg "remove the person in the background"
```

## Settings

Settings are stored in `%APPDATA%\gemini-bridge\config.json`:

```json
{
  "api_key": "...",
  "default_model": "flash",
  "pro_model": "",
  "image_dir": "",
  "allowed_dirs": []
}
```

- `pro_model`, `flash_model`, `image_model` and `image_pro_model` lock a shortcut to one exact model instead of picking automatically.
- `allowed_dirs` limits which folders files can be sent from.
- Credential files are always refused. That includes `.env`, SSH keys, `.pem` and `.key` files, and the bridge's own settings file.
- The key is stored only in this file on your computer. It is never part of this repository or the release zip.

**Privacy:** anything you send goes to Google under the terms for your API key. Google's terms for free (unpaid) keys let it use what you send to improve its products, so check the current terms before sending anything private.

**Hotkeys:** edit the settings at the top of `GeminiClaudeHotkeys.ahk`, for example to choose which browser Gemini opens in. Then right-click the green H tray icon → Reload Script. Win+G normally opens the Xbox Game Bar; if it still appears, turn the Game Bar off in Settings → Gaming.

## Uninstall

1. Delete `%LOCALAPPDATA%\Programs\gemini-bridge` and `%APPDATA%\gemini-bridge`.
2. Delete "Gemini Claude Hotkeys" from `shell:startup`.
3. Remove the `"gemini"` entry from `%APPDATA%\Claude\claude_desktop_config.json`.
4. Remove the `gemini-bridge` folder from your user PATH.

## Development

```
go test ./...            # tests use a simulated Gemini API and temp folders only
go build -o gemini-bridge.exe .
```

The tests never touch your real key, Claude config or PATH.

On every push, CI does the following:
- runs the tests on Windows and Linux
- builds the Windows .exe
- smoke-tests the MCP handshake and the `gem` command
- checks `GeminiClaudeHotkeys.ahk` with AutoHotkey v2

Publishing a release on GitHub (Releases → Draft a new release) makes CI build the zip and attach it to that release.

| File | What it does |
|---|---|
| `mcp.go` | MCP server (stdio JSON-RPC) and the tool definitions Claude sees |
| `gemini.go` | Gemini REST client, model-shortcut resolution, Files API uploads |
| `files.go` | Turns local files into Gemini input (inline, upload, .docx/.pptx text) |
| `image.go` | Image generation and editing |
| `cli.go` | `ask`, `image`, `models` commands and the `gem` shortcut |
| `setup.go` | Interactive setup: key, install, PATH, Claude config, hotkeys |
| `GeminiClaudeHotkeys.ahk` | Win+C / Win+G / Win+Shift+G (AutoHotkey v2) |

## License

MIT
