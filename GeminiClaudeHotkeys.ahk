#Requires AutoHotkey v2.0
#SingleInstance Force

; Gemini + Claude hotkeys
;   Win+C        show / hide the Claude desktop app (opens it if it's closed)
;   Win+G        show / hide Gemini in its own window
;   Win+Shift+G  send the selected text (or your clipboard) to Gemini and show the answer
;
; To change something: edit the settings below, save, then right-click the
; green H icon in the tray and choose "Reload Script".
; Win+G normally opens the Xbox Game Bar and Win+C may open Copilot; this
; script takes both over. If the Game Bar still appears, turn it off in
; Settings > Gaming > Xbox Game Bar.

; ---------------- settings ----------------
GeminiUrl := "https://gemini.google.com/app"
Browser   := ""   ; full path to chrome.exe or msedge.exe; blank = Chrome, then Edge, then your default browser
ClaudeExe := ""   ; full path to Claude's .exe; blank = find it automatically
BridgeExe := ""   ; full path to gemini-bridge.exe; blank = find it automatically
; -------------------------------------------

A_IconTip := "Gemini + Claude hotkeys"

#c:: ToggleClaude()
#g:: ToggleGemini()
#+g:: AskGeminiAboutSelection()

; ---------- Claude ----------

ClaudeWindow() {
    for hwnd in WinGetList("ahk_exe claude.exe") {
        if (WinGetTitle(hwnd) != "")
            return hwnd
    }
    return 0
}

FindClaude() {
    if (ClaudeExe != "" && FileExist(ClaudeExe))
        return ClaudeExe
    p := EnvGet("LOCALAPPDATA") "\AnthropicClaude\claude.exe"
    if FileExist(p)
        return p
    for pattern in ["\Claude.lnk", "\*Claude*.lnk"] {
        for dir in [A_Programs, A_ProgramsCommon] {
            Loop Files dir pattern, "R"
                return A_LoopFileFullPath
        }
    }
    return "claude://"   ; last resort: the app's own link handler
}

ToggleClaude() {
    if (hwnd := ClaudeWindow()) {
        if WinActive(hwnd)
            WinMinimize(hwnd)
        else
            WinActivate(hwnd)
        return
    }
    try Run(FindClaude())
    catch {
        MsgBox("Couldn't find the Claude desktop app. Set ClaudeExe at the top of this script.", "Hotkeys")
        return
    }
    Loop 40 {   ; wait up to 10 seconds for the window
        Sleep(250)
        if (hwnd := ClaudeWindow()) {
            WinActivate(hwnd)
            return
        }
    }
}

; ---------- Gemini ----------

GeminiWindow() {
    for exe in ["chrome.exe", "msedge.exe", "brave.exe", "firefox.exe"] {
        for hwnd in WinGetList("Gemini ahk_exe " exe)
            return hwnd
    }
    return 0
}

FindBrowser() {
    if (Browser != "" && FileExist(Browser))
        return Browser
    pf86 := EnvGet("ProgramFiles(x86)")
    lad := EnvGet("LOCALAPPDATA")
    candidates := []
    candidates.Push(A_ProgramFiles "\Google\Chrome\Application\chrome.exe")
    candidates.Push(pf86 "\Google\Chrome\Application\chrome.exe")
    candidates.Push(lad "\Google\Chrome\Application\chrome.exe")
    candidates.Push(pf86 "\Microsoft\Edge\Application\msedge.exe")
    candidates.Push(A_ProgramFiles "\Microsoft\Edge\Application\msedge.exe")
    for p in candidates {
        if FileExist(p)
            return p
    }
    return ""
}

ToggleGemini() {
    if (hwnd := GeminiWindow()) {
        if WinActive(hwnd)
            WinMinimize(hwnd)
        else
            WinActivate(hwnd)
        return
    }
    b := FindBrowser()
    try {
        if (b != "")
            Run('"' b '" --app=' GeminiUrl)   ; app mode: its own window, no tabs or address bar
        else
            Run(GeminiUrl)
    } catch as e {
        MsgBox("Couldn't open Gemini: " e.Message, "Hotkeys")
        return
    }
    Loop 40 {
        Sleep(250)
        if (hwnd := GeminiWindow()) {
            WinActivate(hwnd)
            return
        }
    }
}

; ---------- Win+Shift+G: ask Gemini about the selection ----------

FindBridge() {
    if (BridgeExe != "" && FileExist(BridgeExe))
        return BridgeExe
    for p in [EnvGet("LOCALAPPDATA") "\Programs\gemini-bridge\gemini-bridge.exe", A_ScriptDir "\gemini-bridge.exe"] {
        if FileExist(p)
            return p
    }
    return ""
}

GrabSelection() {
    KeyWait("LWin", "T1")   ; let go of Win/Shift first so Ctrl+C isn't read as another shortcut
    KeyWait("RWin", "T1")
    KeyWait("Shift", "T1")
    saved := ClipboardAll()
    before := A_Clipboard
    A_Clipboard := ""
    Send("^c")
    text := ClipWait(0.6) ? A_Clipboard : before   ; nothing selected: use what was already copied
    A_Clipboard := saved
    return text
}

AskGeminiAboutSelection() {
    bridge := FindBridge()
    if (bridge = "") {
        MsgBox("Couldn't find gemini-bridge.exe. Run gemini-bridge setup first, or set BridgeExe at the top of this script.", "Ask Gemini")
        return
    }
    text := GrabSelection()
    if (Trim(text) = "") {
        MsgBox("Select some text (or copy it) first, then press Win+Shift+G.", "Ask Gemini")
        return
    }
    preview := StrLen(text) > 300 ? SubStr(text, 1, 300) "..." : text
    ib := InputBox("Text:`n" preview "`n`nWhat should Gemini do with it? (leave blank to just send it)", "Ask Gemini", "w560 h300")
    if (ib.Result != "OK")
        return
    prompt := (Trim(ib.Value) = "") ? text : ib.Value "`n`n---`n" text

    inFile := A_Temp "\gemini-bridge-in.txt"
    outFile := A_Temp "\gemini-bridge-out.txt"
    try FileDelete(inFile)
    try FileDelete(outFile)
    FileAppend(prompt, inFile, "UTF-8-RAW")
    ToolTip("Asking Gemini...")
    try RunWait('"' bridge '" ask --in "' inFile '" --out "' outFile '"', , "Hide")
    catch as e {
        ToolTip()
        MsgBox("Couldn't run gemini-bridge: " e.Message, "Ask Gemini")
        return
    }
    ToolTip()
    answer := FileExist(outFile) ? FileRead(outFile, "UTF-8") : "No answer came back."
    try FileDelete(inFile)
    try FileDelete(outFile)
    ShowAnswer(answer)
}

ShowAnswer(answer) {
    answer := StrReplace(StrReplace(answer, "`r`n", "`n"), "`n", "`r`n")
    g := Gui("+AlwaysOnTop +Resize +MinSize360x200", "Gemini")
    g.SetFont("s10", "Segoe UI")
    ed := g.AddEdit("w680 h440 Multi ReadOnly VScroll", answer)
    copyBtn := g.AddButton("w120", "Copy answer")
    closeBtn := g.AddButton("x+10 w90 Default", "Close")
    copyBtn.OnEvent("Click", (*) => (A_Clipboard := answer, ToolTip("Copied"), SetTimer(() => ToolTip(), -1200)))
    closeBtn.OnEvent("Click", (*) => g.Destroy())
    g.OnEvent("Escape", (*) => g.Destroy())
    g.OnEvent("Size", OnResize)
    g.Show()
    closeBtn.Focus()

    OnResize(guiObj, minMax, w, h) {
        if (minMax = -1)
            return
        ed.Move(, , w - 20, h - 60)
        copyBtn.Move(, h - 40)
        closeBtn.Move(, h - 40)
    }
}
