package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const hotkeyScriptName = "GeminiClaudeHotkeys.ahk"

func readLine(in *bufio.Reader) string {
	s, _ := in.ReadString('\n')
	return strings.TrimSpace(s)
}

func askYesNo(in *bufio.Reader, q string, def bool) bool {
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	fmt.Printf("\n%s %s ", q, hint)
	switch strings.ToLower(readLine(in)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

func runSetup() int {
	in := bufio.NewReader(os.Stdin)
	if ownsConsole() { // launched by double-click: keep the window open at the end
		defer func() {
			fmt.Print("\nPress Enter to close this window...")
			in.ReadString('\n')
		}()
	}

	fmt.Println("Gemini Bridge setup (version " + version + ")")
	fmt.Println(strings.Repeat("-", 44))

	cfg, err := loadConfigFile()
	if err != nil {
		fmt.Println(err)
		return 1
	}

	// 1. API key
	fmt.Println("\nStep 1: your Gemini API key")
	fmt.Println("Get one free at https://aistudio.google.com/apikey (sign in, then \"Create API key\").")
	for attempt := 1; ; attempt++ {
		prompt := "Paste your key: "
		if cfg.APIKey != "" {
			prompt = fmt.Sprintf("Paste a new key, or press Enter to keep the saved one (...%s): ", last4(cfg.APIKey))
		}
		fmt.Print(prompt)
		key := readLine(in)
		if key == "" {
			key = cfg.APIKey
		}
		if key == "" {
			if attempt >= 3 {
				fmt.Println("No key entered; stopping. Run setup again when you have one.")
				return 1
			}
			fmt.Println("A key is required.")
			continue
		}
		fmt.Println("Checking the key with Google...")
		c := newClient(key, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		tiers, err := c.TierSummary(ctx)
		cancel()
		if err != nil {
			fmt.Println("  That didn't work: " + explain(err))
			if attempt >= 3 {
				return 1
			}
			continue
		}
		cfg.APIKey = key
		fmt.Println("  Key works. Model shortcuts right now:")
		for _, t := range tiers {
			fmt.Printf("    %-10s -> %s\n", t[0], t[1])
		}
		break
	}
	p, err := saveConfig(cfg)
	if err != nil {
		fmt.Println("Couldn't save settings:", err)
		return 1
	}
	fmt.Println("  Saved to " + p)

	// 2. Put the exe somewhere stable
	fmt.Println("\nStep 2: installing")
	exe, dir, srcDir, err := installSelf()
	if err != nil {
		fmt.Println("  " + err.Error())
		return 1
	}
	fmt.Println("  Installed to " + exe)

	// 3. gem command
	if runtime.GOOS == "windows" {
		shim := filepath.Join(dir, "gem.cmd")
		if err := os.WriteFile(shim, []byte("@echo off\r\n\"%~dp0gemini-bridge.exe\" gem %*\r\n"), 0o644); err != nil {
			fmt.Println("  Couldn't write gem.cmd:", err)
		} else if askYesNo(in, "Add the `gem` command so you can ask Gemini from any terminal?", true) {
			if msg, err := addToUserPath(dir); err != nil {
				fmt.Println("  Couldn't update PATH:", err)
			} else {
				fmt.Println("  " + msg + " Open a new terminal and try: gem \"hello\"")
			}
		}
	}

	// 4. Claude desktop
	if askYesNo(in, "Connect it to the Claude desktop app?", true) {
		paths, errs := registerClaudeDesktop(exe)
		for _, p := range paths {
			fmt.Println("  Added \"gemini\" to " + p + " (a backup of the old file is next to it)")
		}
		for _, e := range errs {
			fmt.Println("  " + e.Error())
		}
		if len(paths) > 0 {
			fmt.Println("  Quit Claude from its tray icon (closing the window isn't enough) and reopen it.")
		}
	}

	// 5. Claude Code
	if claudeCLI, err := exec.LookPath("claude"); err == nil {
		if askYesNo(in, "Claude Code is installed. Connect it there too?", false) {
			_ = exec.Command(claudeCLI, "mcp", "remove", "--scope", "user", "gemini").Run()
			out, err := exec.Command(claudeCLI, "mcp", "add", "--scope", "user", "gemini", "--", exe, "serve").CombinedOutput()
			if err != nil {
				fmt.Printf("  claude mcp add failed: %v\n%s\n", err, out)
			} else {
				fmt.Println("  Connected to Claude Code.")
			}
		}
	}

	// 6. Hotkeys
	if runtime.GOOS == "windows" {
		setupHotkeys(in, dir, srcDir)
	}

	fmt.Println("\nAll done.")
	return 0
}

func last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

func installDir() (string, error) {
	if runtime.GOOS == "windows" {
		la := os.Getenv("LOCALAPPDATA")
		if la == "" {
			return "", fmt.Errorf("LOCALAPPDATA isn't set")
		}
		return filepath.Join(la, "Programs", "gemini-bridge"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "gemini-bridge"), nil
}

// installSelf copies the running exe to a fixed folder so Claude's config,
// the gem command and the hotkeys can all point at one path.
func installSelf() (exe, dir, srcDir string, err error) {
	src, err := os.Executable()
	if err != nil {
		return "", "", "", err
	}
	if r, err := filepath.EvalSymlinks(src); err == nil {
		src = r
	}
	srcDir = filepath.Dir(src)
	dir, err = installDir()
	if err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", "", err
	}
	name := "gemini-bridge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe = filepath.Join(dir, name)
	if a, e1 := os.Stat(src); e1 == nil {
		if b, e2 := os.Stat(exe); e2 == nil && os.SameFile(a, b) {
			return exe, dir, srcDir, nil
		}
	}
	if err := copyFile(src, exe, 0o755); err != nil {
		return "", "", "", fmt.Errorf("couldn't copy to %s (%v). If Claude is running, quit it from the tray icon and run setup again", exe, err)
	}
	return exe, dir, srcDir, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func powershell(script string) (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func addToUserPath(dir string) (string, error) {
	script := `$d = ` + psQuote(dir) + `; $p = [Environment]::GetEnvironmentVariable('Path','User'); if (-not $p) { $p = '' }; ` +
		`if (($p -split ';') -contains $d) { 'already' } else { [Environment]::SetEnvironmentVariable('Path', (($p.TrimEnd(';') + ';' + $d).TrimStart(';')), 'User'); 'added' }`
	out, err := powershell(script)
	if err != nil {
		return "", fmt.Errorf("%v %s", err, out)
	}
	if strings.Contains(out, "already") {
		return "`gem` was already on your PATH.", nil
	}
	return "Added `gem` to your PATH.", nil
}

// ---------- Claude desktop registration ----------

func claudeDesktopConfigPaths() []string {
	var dirs []string
	switch runtime.GOOS {
	case "windows":
		if a := os.Getenv("APPDATA"); a != "" {
			dirs = append(dirs, filepath.Join(a, "Claude"))
		}
		if la := os.Getenv("LOCALAPPDATA"); la != "" { // Microsoft Store / MSIX installs keep a separate copy
			m, _ := filepath.Glob(filepath.Join(la, "Packages", "*Claude*", "LocalCache", "Roaming", "Claude"))
			dirs = append(dirs, m...)
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "Claude"))
		}
	default:
		if d, err := os.UserConfigDir(); err == nil {
			dirs = append(dirs, filepath.Join(d, "Claude"))
		}
	}
	var out []string
	for _, d := range dirs {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, filepath.Join(d, "claude_desktop_config.json"))
		}
	}
	if len(out) == 0 && len(dirs) > 0 {
		out = []string{filepath.Join(dirs[0], "claude_desktop_config.json")}
	}
	return out
}

func registerClaudeDesktop(exe string) ([]string, []error) {
	var done []string
	var errs []error
	for _, p := range claudeDesktopConfigPaths() {
		if err := mergeClaudeConfig(p, exe); err != nil {
			errs = append(errs, err)
			continue
		}
		done = append(done, p)
	}
	return done, errs
}

// mergeClaudeConfig adds mcpServers.gemini and leaves every other entry alone.
func mergeClaudeConfig(path, exe string) error {
	m := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil && len(bytes.TrimSpace(b)) > 0:
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			return fmt.Errorf("%s isn't valid JSON, so I left it alone (%v). Add this under \"mcpServers\" by hand: \"gemini\": {\"command\": %q, \"args\": [\"serve\"]}", path, err, exe)
		}
		backup := path + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, b, 0o600); err != nil {
			return fmt.Errorf("couldn't back up %s: %v", path, err)
		}
	case err != nil && !os.IsNotExist(err):
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["gemini"] = map[string]any{"command": exe, "args": []string{"serve"}}
	m["mcpServers"] = servers
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o600)
}

// ---------- hotkeys (Windows) ----------

func findAutoHotkey() string {
	pf, la := os.Getenv("ProgramFiles"), os.Getenv("LOCALAPPDATA")
	for _, p := range []string{
		filepath.Join(pf, "AutoHotkey", "v2", "AutoHotkey64.exe"),
		filepath.Join(pf, "AutoHotkey", "v2", "AutoHotkey.exe"),
		filepath.Join(la, "Programs", "AutoHotkey", "v2", "AutoHotkey64.exe"),
		filepath.Join(pf, "AutoHotkey", "AutoHotkey64.exe"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func setupHotkeys(in *bufio.Reader, dir, srcDir string) {
	script := filepath.Join(dir, hotkeyScriptName)
	if src := filepath.Join(srcDir, hotkeyScriptName); srcDir != dir {
		if _, err := os.Stat(src); err == nil {
			if err := copyFile(src, script, 0o644); err != nil {
				fmt.Println("  Couldn't copy the hotkey script:", err)
				return
			}
		}
	}
	if _, err := os.Stat(script); err != nil {
		fmt.Println("\n(Skipping hotkeys: " + hotkeyScriptName + " wasn't next to the exe. Unzip everything into one folder to include them.)")
		return
	}
	if !askYesNo(in, "Turn on the hotkeys (Win+C Claude, Win+G Gemini, Win+Shift+G ask Gemini about selected text) and start them at sign-in?", true) {
		return
	}
	ahk := findAutoHotkey()
	lnk := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "Gemini Claude Hotkeys.lnk")
	target, args := script, ""
	if ahk != "" {
		target, args = ahk, `"`+script+`"`
	}
	ps := `$s = (New-Object -ComObject WScript.Shell).CreateShortcut(` + psQuote(lnk) + `); $s.TargetPath = ` + psQuote(target) +
		`; $s.Arguments = ` + psQuote(args) + `; $s.WorkingDirectory = ` + psQuote(dir) + `; $s.Save()`
	if out, err := powershell(ps); err != nil {
		fmt.Printf("  Couldn't create the startup shortcut: %v %s\n", err, out)
	} else {
		fmt.Println("  Hotkeys will start when you sign in.")
	}
	if ahk == "" {
		fmt.Println("  AutoHotkey v2 isn't installed yet. Get it free from https://www.autohotkey.com (choose v2),")
		fmt.Println("  then double-click " + script + " (or just sign out and back in).")
		return
	}
	if err := exec.Command(ahk, script).Start(); err != nil {
		fmt.Println("  Couldn't start the hotkeys now:", err)
		return
	}
	fmt.Println("  Hotkeys are on now. Look for the green H icon in the tray.")
}
