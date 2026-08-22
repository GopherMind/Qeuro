//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// включаем поддержку ANSI-кодов в консоли Windows
func enableVirtualTerminal() {
	const enableVTProcessing = 0x0004
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getMode := kernel32.NewProc("GetConsoleMode")
	setMode := kernel32.NewProc("SetConsoleMode")

	handle := uintptr(syscall.Stdout)
	var mode uint32
	// #nosec G103 -- unsafe.Pointer is the required calling convention for the
	// Win32 GetConsoleMode out-parameter; there is no safe alternative.
	if ret, _, _ := getMode.Call(handle, uintptr(unsafe.Pointer(&mode))); ret == 0 {
		return
	}
	// #nosec G104 -- enabling ANSI is best-effort: on a console that refuses it
	// the CLI still works, just without colour. Nothing to recover from.
	_, _, _ = setMode.Call(handle, uintptr(mode|enableVTProcessing))
}
