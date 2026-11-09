//go:build windows

package main

import (
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/brix7on/hush/internal/aec"
)

const defaultIdleSleepMin = 15

var (
	idleSleepOn atomic.Bool
	powrprof    = syscall.NewLazyDLL("powrprof.dll")

	procGetLastInputInfo = u32.NewProc("GetLastInputInfo")
	procGetTickCount     = k32.NewProc("GetTickCount")
	procSetSuspendState  = powrprof.NewProc("SetSuspendState")
)

type lastInputInfo struct {
	Size uint32
	Time uint32
}

// inputIdle — сколько времени никто не трогал мышь и клавиатуру.
func inputIdle() time.Duration {
	li := lastInputInfo{Size: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li))); r == 0 {
		return 0
	}
	tick, _, _ := procGetTickCount.Call()
	return time.Duration(uint32(tick)-li.Time) * time.Millisecond
}

func startIdleSleep(sup *aec.Suppressor, idleSleepAfter time.Duration, running func() bool) {
	go func() {
		last := time.Now() // когда в последний раз был звук
		prev := time.Now() // прошлый такт (чтобы заметить выход из сна)
		for {
			time.Sleep(5 * time.Second)
			now := time.Now().Round(0)
			if now.Sub(prev) > 30*time.Second { // ПК спал — отсчёт тишины начинаем заново
				last = now
			}
			prev = now
			if !running() {
				last = now
				continue
			}
			st := sup.Stats()
			if st.RefDB > -70 || (st.Voice && st.MicDB > -60) {
				last = now
			}
			if !idleSleepOn.Load() || now.Sub(last) < idleSleepAfter || inputIdle() < idleSleepAfter {
				continue
			}
			logEvent("тишина %.0f мин, мышь и клавиатура не тронуты — отправляю ПК в сон", now.Sub(last).Minutes())
			last = now
			procSetSuspendState.Call(0, 0, 0)
		}
	}()
}
