package main

import (
	"syscall"
	"unsafe"
)

// ownsConsole reports whether this process is alone in its console window,
// which means it was started by double-clicking rather than from a terminal.
func ownsConsole() bool {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	if proc.Find() != nil {
		return false
	}
	var ids [4]uint32
	n, _, _ := proc.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	return n == 1
}
