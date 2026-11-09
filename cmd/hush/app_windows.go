//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

func systemRussian() bool {
	r, _, _ := k32.NewProc("GetUserDefaultUILanguage").Call()
	switch uint16(r) & 0x3ff {
	case 0x19, 0x22, 0x23, 0x3f: // ru, uk, be, kk
		return true
	}
	return false
}

// ---------- файлы ----------

var (
	dataOnce sync.Once
	dataPath string
)

func exeDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// dataDir — папка с настройками и журналом: рядом с exe, а если туда нельзя писать — %LOCALAPPDATA%\hush.
func dataDir() string {
	dataOnce.Do(func() {
		dataPath = exeDir()
		probe := filepath.Join(dataPath, ".hush-write-test")
		if f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Close()
			os.Remove(probe)
		} else if base := os.Getenv("LOCALAPPDATA"); base != "" {
			dataPath = filepath.Join(base, appName)
			os.MkdirAll(dataPath, 0o755)
		}
		migrateOldFiles(dataPath)
	})
	return dataPath
}

func dataFile(name string) string { return filepath.Join(dataDir(), appName+"-"+name) }

// migrateOldFiles переносит настройки и калибровку прежней версии (micaec-*).
func migrateOldFiles(dir string) {
	for _, n := range []string{"config.json", "calib.json"} {
		old, cur := filepath.Join(dir, "micaec-"+n), filepath.Join(dir, appName+"-"+n)
		if _, err := os.Stat(cur); err == nil {
			continue
		}
		if b, err := os.ReadFile(old); err == nil {
			os.WriteFile(cur, b, 0o644)
		}
	}
}

// rotate обрезает файл, если он вырос больше limit.
func rotate(path string, limit int64) {
	if st, err := os.Stat(path); err == nil && st.Size() > limit {
		os.Remove(path + ".old")
		os.Rename(path, path+".old")
	}
}

// ---------- консоль ----------

var (
	procAttachConsole = k32.NewProc("AttachConsole")
	procAllocConsole  = k32.NewProc("AllocConsole")
	hasConsole        bool
)

// useConsole подключает консоль: родительскую, если запущено из терминала, иначе новую.
func useConsole(allocate bool) bool {
	if hasConsole {
		return true
	}
	if r, _, _ := procAttachConsole.Call(^uintptr(0)); r == 0 {
		if !allocate {
			return false
		}
		if r, _, _ := procAllocConsole.Call(); r == 0 {
			return false
		}
	}
	if f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout = f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
		os.Stdin = f
	}
	procSetConsoleOutCP.Call(65001)
	hasConsole = true
	return true
}

// ---------- сообщения ----------

func messageBox(text string, flags uintptr) uintptr {
	r, _, _ := pMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16(text))), uintptr(unsafe.Pointer(u16(appName))), flags)
	return r
}

func fatal(format string, a ...interface{}) {
	text := fmt.Sprintf(format, a...)
	logEvent("ошибка: %s", text)
	if hasConsole {
		fmt.Printf("\n%s: %s\n", L("ОШИБКА", "ERROR"), text)
		fmt.Println(L("\nНажми Enter, чтобы закрыть окно...", "\nPress Enter to close..."))
		waitEnter()
	} else {
		messageBox(text, 0x10)
	}
	os.Exit(1)
}

// ---------- один экземпляр ----------

var (
	procCreateMutex = k32.NewProc("CreateMutexW")
	pFindWindow     = syscall.NewLazyDLL("user32.dll").NewProc("FindWindowW")
)

const panelClass = "HushPanel"

// singleInstance: false — hush уже запущен; тогда просим его показать панель.
func singleInstance(showOther bool) bool {
	h, _, err := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(u16(`Local\hush-single-instance`))))
	if h == 0 || err != syscall.ERROR_ALREADY_EXISTS {
		return true
	}
	if showOther {
		if w, _, _ := pFindWindow.Call(uintptr(unsafe.Pointer(u16(panelClass))), 0); w != 0 {
			pPostMessage.Call(w, wmShowPanel, 0, 0)
		}
	}
	return false
}
