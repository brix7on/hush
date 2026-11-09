//go:build windows

package main

import (
	"fmt"
	"math"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

var (
	pGetAsyncKeyState = u32.NewProc("GetAsyncKeyState")
	pMapVirtualKey    = u32.NewProc("MapVirtualKeyW")
	pGetKeyNameText   = u32.NewProc("GetKeyNameTextW")
)

const (
	pttOff    = 0
	pttHold   = 1
	pttToggle = 2

	pttTail = 250 * time.Millisecond
)

type pttCtl struct {
	mode     atomic.Int32
	key      atomic.Int32
	open     atomic.Bool // микрофон сейчас открыт
	binding  atomic.Bool // ждём нажатия, чтобы назначить кнопку
	onBind   func(vk int)
	onChange func(open bool)
}

func newPTT(mode, key int, onBind func(int)) *pttCtl {
	p := &pttCtl{onBind: onBind}
	p.mode.Store(int32(mode))
	p.key.Store(int32(key))
	p.open.Store(!(mode == pttHold && key != 0)) // «удерживать»: на старте молчим, без сигнала
	go p.loop()
	return p
}

func (p *pttCtl) setOpen(v bool, notify bool) {
	if p.open.Swap(v) != v && notify && p.onChange != nil {
		p.onChange(v)
	}
}

func keyDown(vk int) bool {
	r, _, _ := pGetAsyncKeyState.Call(uintptr(vk))
	return r&0x8000 != 0
}

func (p *pttCtl) loop() {
	var prev [256]bool
	var lastDown time.Time
	toggled := true
	lastMode := int32(-1)
	wasBinding := false
	for {
		time.Sleep(10 * time.Millisecond)
		mode := p.mode.Load()
		if mode != lastMode {
			toggled = true
			lastMode = mode
		}
		if p.binding.Load() {
			if !wasBinding {

				for vk := range prev {
					prev[vk] = keyDown(vk)
				}
				wasBinding = true
				continue
			}
			for vk := 3; vk < 255; vk++ { // 1 и 2 — левая/правая кнопки мыши, их не назначаем
				if vk == 0x10 || vk == 0x11 || vk == 0x12 { // общие Shift/Ctrl/Alt — берём левые/правые
					continue
				}
				d := keyDown(vk)
				if d && !prev[vk] {
					p.key.Store(int32(vk))
					p.binding.Store(false)
					if p.onBind != nil {
						p.onBind(vk)
					}
					break
				}
				prev[vk] = d
			}
			continue
		}
		if wasBinding {
			wasBinding = false
			vk := int(p.key.Load())
			if vk > 0 && vk < 256 {
				prev[vk] = keyDown(vk)
			}
		}
		vk := int(p.key.Load())
		if mode == pttOff || vk == 0 {
			p.setOpen(true, false)
			continue
		}
		d := keyDown(vk)
		switch mode {
		case pttHold:
			if d {
				lastDown = time.Now()
				p.setOpen(true, true)
			} else if time.Since(lastDown) > pttTail {
				p.setOpen(false, true)
			}
		case pttToggle:
			if d && !prev[vk] {
				toggled = !toggled
			}
			p.setOpen(toggled, true)
		}
		prev[vk] = d
	}
}

// keyName — человеческое название клавиши.
func keyName(vk int) string {
	switch vk {
	case 0:
		return L("не назначена", "not set")
	case 4:
		return "Mouse 3"
	case 5:
		return "Mouse 4"
	case 6:
		return "Mouse 5"
	case 0xA0:
		return "LShift"
	case 0xA1:
		return "RShift"
	case 0xA2:
		return "LCtrl"
	case 0xA3:
		return "RCtrl"
	case 0xA4:
		return "LAlt"
	case 0xA5:
		return "RAlt"
	case 0x14:
		return "Caps Lock"
	}
	if vk >= 0x70 && vk <= 0x87 {
		return fmt.Sprintf("F%d", vk-0x6F)
	}
	sc, _, _ := pMapVirtualKey.Call(uintptr(vk), 0)
	if sc != 0 {
		lp := sc << 16
		switch vk {
		case 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x2D, 0x2E, 0x6F, 0x90:
			lp |= 1 << 24
		}
		buf := make([]uint16, 64)
		n, _, _ := pGetKeyNameText.Call(lp, uintptr(unsafe.Pointer(&buf[0])), 64)
		if n > 0 {
			return syscall.UTF16ToString(buf[:n])
		}
	}
	return fmt.Sprintf("Key %d", vk)
}

// cueSamples — сигнал: включение — «бульк» вверх, выключение — «бульк» вниз.

func cueSamples(rate int, open bool) []float32 {
	type chirp struct {
		f0, f1  float64 // частота в начале и в конце, Гц
		ms      float64
		bright  float64 // доля гармоник
		startMs float64
	}
	var parts []chirp
	if open {
		parts = []chirp{{400, 900, 130, 0.25, 0}} // зеркально выключению: «бульк» вверх
	} else {
		parts = []chirp{{820, 380, 150, 0.2, 0}}
	}
	total := 0.0
	for _, c := range parts {
		if e := c.startMs + c.ms; e > total {
			total = e
		}
	}
	out := make([]float32, int((total+25)*float64(rate)/1000))
	for _, c := range parts {
		n := int(c.ms * float64(rate) / 1000)
		st := int(c.startMs * float64(rate) / 1000)
		ph := 0.0
		for i := 0; i < n && st+i < len(out); i++ {
			t := float64(i) / float64(n)
			f := c.f0 * math.Pow(c.f1/c.f0, t) // скольжение высоты
			ph += 2 * math.Pi * f / float64(rate)
			sec := float64(i) / float64(rate)
			env := math.Min(1, sec/0.003) * math.Exp(-sec/(c.ms/1000*0.45)) // резкая атака, быстрый спад
			if tail := float64(n-i) / (0.004 * float64(rate)); tail < 1 {
				env *= tail
			}
			v := math.Sin(ph) + c.bright*(0.6*math.Sin(2*ph)+0.3*math.Sin(3*ph))
			out[st+i] += float32(0.22 * env * v)
		}
	}
	return out
}

// cuePlayer играет сигналы в наушники в своём потоке, лишние пропускает.

type cuePlayer struct {
	ch      chan bool
	enabled atomic.Bool
}

func newCuePlayer(device func() string) *cuePlayer {
	c := &cuePlayer{ch: make(chan bool, 1)}
	c.enabled.Store(true)
	go func() {
		runtime.LockOSThread()
		ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
		for open := range c.ch {
			dev, _, err := pickDevice(wca.ERender, device(), "наушники")
			if err != nil {
				continue
			}
			o := open
			playSamples(dev, func(rate int) []float32 { return cueSamples(rate, o) })
			dev.Release()
		}
	}()
	return c
}

func (c *cuePlayer) play(open bool) {
	if c == nil || !c.enabled.Load() {
		return
	}
	select {
	case c.ch <- open:
	default:
		select { // заменяем ждущий сигнал на свежий
		case <-c.ch:
		default:
		}
		select {
		case c.ch <- open:
		default:
		}
	}
}
