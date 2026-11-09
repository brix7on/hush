//go:build windows

// hush — убирает из микрофона гарнитуры наводку наушников и отдаёт звук в виртуальный кабель.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"

	"github.com/brix7on/hush/internal/aec"
	"github.com/brix7on/hush/internal/audio"
	"github.com/brix7on/hush/internal/diag"
	"github.com/brix7on/hush/internal/pipe"
)

var (
	k32                 = syscall.NewLazyDLL("kernel32.dll")
	winmm               = syscall.NewLazyDLL("winmm.dll")
	procSetConsoleOutCP = k32.NewProc("SetConsoleOutputCP")
	procQPC             = k32.NewProc("QueryPerformanceCounter")
	procQPF             = k32.NewProc("QueryPerformanceFrequency")
	procTimeBeginPeriod = winmm.NewProc("timeBeginPeriod")
	procCreateEventW    = k32.NewProc("CreateEventW")
	procWaitSingle      = k32.NewProc("WaitForSingleObject")
	procCreateTimerEx   = k32.NewProc("CreateWaitableTimerExW")
	procSetTimer        = k32.NewProc("SetWaitableTimer")
	procSetPrioClass    = k32.NewProc("SetPriorityClass")
	procSetProcInfo     = k32.NewProc("SetProcessInformation")
	avrt                = syscall.NewLazyDLL("avrt.dll")
	procAvSetMmThread   = avrt.NewProc("AvSetMmThreadCharacteristicsW")
	procAvRevertMm      = avrt.NewProc("AvRevertMmThreadCharacteristics")
	qpcFreq             int64

	pGetProcessHandleCnt = k32.NewProc("GetProcessHandleCount")
	pGetGuiResources     = syscall.NewLazyDLL("user32.dll").NewProc("GetGuiResources")
	sessionsStarted      atomic.Int64
)

// memWatch раз в 10 минут пишет в журнал расход памяти и хендлов.

func memWatch() {
	for {
		time.Sleep(10 * time.Minute)
		debug.FreeOSMemory()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		var hc uint32
		self := ^uintptr(0)
		pGetProcessHandleCnt.Call(self, uintptr(unsafe.Pointer(&hc)))
		gdi, _, _ := pGetGuiResources.Call(self, 0)
		usr, _, _ := pGetGuiResources.Call(self, 1)
		logEvent("память: куча %d МБ, от системы %d МБ, горутин %d, хендлов %d, GDI %d, USER %d, сессий %d",
			m.HeapAlloc>>20, m.Sys>>20, runtime.NumGoroutine(), hc, gdi, usr, sessionsStarted.Load())
	}
}

// ---------- журнал ----------

var logMu sync.Mutex

// logEvent дописывает строку в журнал.

func logEvent(format string, a ...interface{}) {
	logMu.Lock()
	defer logMu.Unlock()
	path := dataFile("log.txt")
	rotate(path, 512*1024)
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(fh, "%s  %s\r\n", time.Now().Format("2006-01-02 15:04:05.000"), fmt.Sprintf(format, a...))
	fh.Close()
}

var outStarves atomic.Int64 // провалы звука в кабеле за сессию (для панели)

// tick — точная пауза ~1 мс.

type tick struct{ h uintptr }

func newTick() *tick {
	h, _, _ := procCreateTimerEx.Call(0, 0, 0x2, 0x1F0003)
	return &tick{h: h}
}

func (t *tick) wait() {
	if t.h == 0 {
		time.Sleep(time.Millisecond)
		return
	}
	due := int64(-10000) // 1 мс, относительно сейчас (в 100 нс)
	if r, _, _ := procSetTimer.Call(t.h, uintptr(unsafe.Pointer(&due)), 0, 0, 0, 0); r == 0 {
		time.Sleep(time.Millisecond)
		return
	}
	procWaitSingle.Call(t.h, 50)
}

func (t *tick) close() {
	if t.h != 0 {
		procCloseHandle.Call(t.h)
	}
}

// processForAudio: приоритет процесса чуть выше обычного и отказ от «экономного» режима Windows 11 (EcoQoS).

func processForAudio() {
	self := ^uintptr(0)                 // GetCurrentProcess()
	procSetPrioClass.Call(self, 0x8000) // ABOVE_NORMAL_PRIORITY_CLASS
	st := struct{ Version, ControlMask, StateMask uint32 }{1, 0x1 | 0x4, 0}
	if procSetProcInfo.Find() == nil {
		procSetProcInfo.Call(self, 4, uintptr(unsafe.Pointer(&st)), unsafe.Sizeof(st)) // ProcessPowerThrottling
	}
}

// счётчики «зависаний» звуковых потоков: поток не получал процессор дольше 30 мс
var (
	micStalls, refStalls, outStalls atomic.Int64
	micStallMax, refStallMax        atomic.Int64 // мс
	outStallMax                     atomic.Int64
)

func noteStall(n, mx *atomic.Int64, gap time.Duration) {
	if gap <= 30*time.Millisecond {
		return
	}
	n.Add(1)
	if ms := gap.Milliseconds(); ms > mx.Load() {
		mx.Store(ms)
	}
}

// proAudioThread поднимает приоритет потока через MMCSS («Pro Audio»).

func proAudioThread() func() {
	if avrt.Load() != nil {
		return func() {}
	}
	var idx uint32
	h, _, _ := procAvSetMmThread.Call(uintptr(unsafe.Pointer(u16("Pro Audio"))), uintptr(unsafe.Pointer(&idx)))
	if h == 0 {
		return func() {}
	}
	return func() { procAvRevertMm.Call(h) }
}

// nowQPC — текущее время в тех же единицах (100 нс), что и метки пакетов WASAPI.
func nowQPC() int64 {
	var c int64
	procQPC.Call(uintptr(unsafe.Pointer(&c)))
	return c/qpcFreq*10000000 + (c%qpcFreq)*10000000/qpcFreq
}

func waitEnter() {
	restoreConsole()
	bufio.NewReader(os.Stdin).ReadString('\n')
}

var (
	procGetConsoleMode = k32.NewProc("GetConsoleMode")
	procSetConsoleMode = k32.NewProc("SetConsoleMode")
	origConsoleMode    uint32
	consoleRaw         bool
)

// rawConsole включает посимвольный ввод (без Enter), Ctrl+C продолжает работать.
func rawConsole() bool {
	h := os.Stdin.Fd()
	if r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&origConsoleMode))); r == 0 {
		return false
	}
	if r, _, _ := procSetConsoleMode.Call(h, 0x0001); r == 0 { // ENABLE_PROCESSED_INPUT
		return false
	}
	consoleRaw = true
	return true
}

func restoreConsole() {
	if consoleRaw {
		procSetConsoleMode.Call(os.Stdin.Fd(), uintptr(origConsoleMode))
		consoleRaw = false
	}
}

type appConfig struct {
	GainDB       float64 `json:"gain_db"`
	Mic          string  `json:"mic,omitempty"`
	Ref          string  `json:"ref,omitempty"`
	Out          string  `json:"out,omitempty"`
	Beta         float64 `json:"beta,omitempty"`
	Noise        *int    `json:"noise,omitempty"` // 0..3; не задано — «Лёгкий»
	EQ           int     `json:"eq,omitempty"`
	AGC          bool    `json:"agc,omitempty"`
	PTT          int     `json:"ptt_mode,omitempty"`     // 0 выкл, 1 удерживать, 2 вкл/выкл
	PTTKey       int     `json:"ptt_key,omitempty"`      // код клавиши/кнопки
	NoCue        bool    `json:"ptt_no_sound,omitempty"` // без звука вкл/выкл микрофона
	Shared       bool    `json:"shared,omitempty"`       // вывод в кабель обычным режимом, а не монопольным
	MicEx        bool    `json:"mic_excl,omitempty"`
	AutoSet      bool    `json:"autostart_set,omitempty"`  // автозапуск уже включался сам
	NoIdleSleep  bool    `json:"no_idle_sleep,omitempty"`  // не отправлять ПК в сон при долгой тишине
	IdleSleepMin int     `json:"idle_sleep_min,omitempty"` // через сколько минут тишины спать (по умолчанию 15)
}

const maxGainDB = 45

var nameMu sync.RWMutex

func idleSleepMin() int {
	cfgMu.Lock()
	m := curCfg.IdleSleepMin
	cfgMu.Unlock()
	if m <= 0 {
		return defaultIdleSleepMin
	}
	return m
}

var outBufMs atomic.Int64 // текущий запас буфера вывода, мс

var outLatMs atomic.Int64

// как сейчас захватывается микрофон
var (
	micPeriodUs   atomic.Int64
	micExclActive atomic.Bool // микрофон реально открыт монопольно
	micExclWhy    atomic.Value
	micExclShown  atomic.Value // string: последняя показанная подсказка
	micExclMinP   atomic.Int64
	micGlitches   atomic.Int64
	micOwnClock   atomic.Bool // метки времени драйвера кривые — считаем время сами
	micLossPct    atomic.Int64
)

// micExclFail объясняет, почему микрофон не открылся монопольно, и подсказывает, что делать.
func micExclFail(dev *wca.IMMDevice, micName string, err error) {
	e := err.Error()
	short, long := L("драйвер не дал", "driver refused"), L("Драйвер микрофона не принял монопольный режим (", "The mic driver refused exclusive mode (")+firstLine(e)+L("). Работаю в обычном.", "). Using shared mode.")
	switch {
	case strings.Contains(e, "2290679818"): // 0x8889000A: микрофон уже открыт другой программой
		var act, idle []string
		seen := map[string]bool{}
		me := strings.ToLower(filepath.Base(os.Args[0]))
		if exe, err := os.Executable(); err == nil {
			me = strings.ToLower(filepath.Base(exe))
		}
		for _, ss := range deviceSessions(dev) {
			x := ss.exe
			if seen[x] || strings.ToLower(x) == me {
				continue
			}
			seen[x] = true
			if ss.active {
				act = append(act, x)
			} else {
				idle = append(idle, x)
			}
		}
		who := append(act, idle...)
		if len(who) == 0 {
			short = L("микрофон занят", "mic is busy")
			long = L("Микрофон открыт другой программой, монопольно его не взять.", "Another program has the mic open, so it can't be captured exclusively.")
		} else {
			first := []rune(who[0])
			if len(first) > 18 {
				first = append(first[:17], '…')
			}
			short = L("занят: ", "used by ") + string(first)
			if len(who) > 1 {
				short += fmt.Sprintf(L(" и ещё %d", " and %d more"), len(who)-1)
			}
			long = L("Микрофон держат: ", "The mic is held by: ") + strings.Join(who, ", ") + "."
		}
		long += L(" Закрой их или выбери у них микрофоном «CABLE Output» и включи монопольный захват снова.",
			" Close them or set their mic to \"CABLE Output\", then turn exclusive capture on again.")
	case errors.Is(err, errMicLoses):
		short = L("теряет звук", "drops audio")
		long = L("В монопольном режиме этот микрофон теряет звук, оставлен обычный режим.", "This mic drops audio in exclusive mode, shared mode is kept.")
	case strings.Contains(e, "2290679822"): // 0x8889000E: запрещено в настройках
		short = L("запрещено в Windows", "disabled in Windows")
		long = L("В Windows запрещён монопольный режим. Звук → Запись → «", "Exclusive mode is disabled in Windows. Sound → Recording → \"") + micName +
			L("» → Свойства → Дополнительно → «Разрешить приложениям использовать устройство в монопольном режиме».",
				"\" → Properties → Advanced → \"Allow applications to take exclusive control of this device\".")
	}
	micExclWhy.Store(short)
	fmt.Printf("\n(монопольный режим микрофона: %s — работаю обычным)\n", long)
	if v, _ := micExclShown.Load().(string); v != long {
		micExclShown.Store(long)
		theUI.Balloon(L("Монопольный захват не вышел", "Exclusive capture failed"), long)
	}
}

var exclActive atomic.Bool // вывод сейчас реально монопольный

var uiNote atomic.Value

var (
	cfgMu  sync.Mutex
	curCfg appConfig
)

func updateConfig(f func(*appConfig)) {
	cfgMu.Lock()
	f(&curCfg)
	c := curCfg
	cfgMu.Unlock()
	saveConfig(c)
}

func configPath() string { return dataFile("config.json") }

func loadConfig() appConfig {
	var c appConfig
	if b, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c appConfig) {
	if b, err := json.MarshalIndent(c, "", " "); err == nil {
		os.WriteFile(configPath(), b, 0o644)
	}
}

// gainKeys: «+» / «-» меняют усиление на 1 дБ, «0» — сброс; значение запоминается.
func gainKeys(amp *pipe.Amp) {
	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		for _, ch := range buf[:n] {
			g := amp.GainDB()
			switch ch {
			case '+', '=':
				g++
			case '-', '_':
				g--
			case '0':
				g = 0
			default:
				continue
			}
			g = math.Max(-10, math.Min(maxGainDB, g))
			amp.SetGainDB(g)
			updateConfig(func(c *appConfig) { c.GainDB = g })
		}
	}
}

// ---------- устройства ----------

type devInfo struct {
	dev  *wca.IMMDevice
	name string
}

func newEnumerator() (*wca.IMMDeviceEnumerator, error) {
	var e *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &e); err != nil {
		return nil, err
	}
	return e, nil
}

func friendlyName(dev *wca.IMMDevice) string {
	var ps *wca.IPropertyStore
	if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return "?"
	}
	defer ps.Release()
	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return "?"
	}
	return pv.String()
}

func listDevices(flow uint32) ([]devInfo, error) {
	e, err := newEnumerator()
	if err != nil {
		return nil, err
	}
	defer e.Release()
	var dc *wca.IMMDeviceCollection
	if err := e.EnumAudioEndpoints(flow, wca.DEVICE_STATE_ACTIVE, &dc); err != nil {
		return nil, err
	}
	defer dc.Release()
	var n uint32
	if err := dc.GetCount(&n); err != nil {
		return nil, err
	}
	var res []devInfo
	for i := uint32(0); i < n; i++ {
		var d *wca.IMMDevice
		if err := dc.Item(i, &d); err != nil {
			continue
		}
		res = append(res, devInfo{d, friendlyName(d)})
	}
	return res, nil
}

// isCable — виртуальный кабель (его нельзя брать ни микрофоном, ни «наушниками»).
func isCable(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "cable") || strings.Contains(n, "vb-audio")
}

// pickDevice ищет устройство по части названия; пусто или не нашлось — устройство Windows по умолчанию.
func pickDevice(flow uint32, sub, what string) (*wca.IMMDevice, string, error) {
	return pickDeviceEx(flow, sub, what, false)
}

func pickDeviceEx(flow uint32, sub, what string, required bool) (*wca.IMMDevice, string, error) {
	auto := func() (*wca.IMMDevice, string, error) {
		e, err := newEnumerator()
		if err != nil {
			return nil, "", err
		}
		defer e.Release()
		var d *wca.IMMDevice
		if err := e.GetDefaultAudioEndpoint(flow, wca.EConsole, &d); err == nil {
			if n := friendlyName(d); !isCable(n) {
				return d, n, nil
			}
			d.Release()
		}
		list, err := listDevices(flow)
		if err != nil {
			return nil, "", err
		}
		if d, n := takeDevice(list, func(n string) bool { return !isCable(n) }); d != nil {
			return d, n, nil
		}
		return nil, "", fmt.Errorf(L("не найдено ни одного устройства (%s), кроме виртуального кабеля",
			"no device found (%s) other than the virtual cable"), what)
	}
	if sub == "" {
		return auto()
	}
	list, err := listDevices(flow)
	if err != nil {
		return nil, "", err
	}
	var names []string
	for _, d := range list {
		names = append(names, "  • "+d.name)
	}
	if d, n := takeDevice(list, func(n string) bool { return strings.Contains(strings.ToLower(n), strings.ToLower(sub)) }); d != nil {
		return d, n, nil
	}
	if !required {
		if d, n, err := auto(); err == nil {
			fmt.Printf("(%s: «%s» не найден — беру «%s»)\n", what, sub, n)
			return d, n, nil
		}
	}
	return nil, "", fmt.Errorf(L("%s: не найдено устройство «%s». Доступные:\n%s", "%s: device \"%s\" not found. Available:\n%s"),
		what, sub, strings.Join(names, "\n"))
}

// takeDevice возвращает первое подходящее устройство, остальные освобождает.
func takeDevice(list []devInfo, ok func(string) bool) (*wca.IMMDevice, string) {
	var got *wca.IMMDevice
	var name string
	for _, d := range list {
		if got == nil && ok(d.name) {
			got, name = d.dev, d.name
			continue
		}
		d.dev.Release()
	}
	return got, name
}

// deviceNames — названия активных устройств.
func deviceNames(flow uint32) ([]string, error) {
	list, err := listDevices(flow)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, d := range list {
		names = append(names, d.name)
		d.dev.Release()
	}
	return names, nil
}

func openClient(dev *wca.IMMDevice) (*wca.IAudioClient, *wca.WAVEFORMATEX, audio.Format, error) {
	var ac *wca.IAudioClient
	if err := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
		return nil, nil, audio.Format{}, fmt.Errorf("Activate: %v", err)
	}
	var wfx *wca.WAVEFORMATEX
	if err := ac.GetMixFormat(&wfx); err != nil {
		return nil, nil, audio.Format{}, fmt.Errorf("GetMixFormat: %v", err)
	}
	raw := unsafe.Slice((*byte)(unsafe.Pointer(wfx)), 18+int(wfx.CbSize))
	f, err := audio.ParseWaveFormat(raw)
	if err != nil {
		return nil, nil, f, err
	}
	return ac, wfx, f, nil
}

// openExclusive открывает вывод монопольно: звук не идёт через общий микшер и не попадает в демонстрацию экрана.

func openExclusive(dev *wca.IMMDevice) (*wca.IAudioClient, *wca.WAVEFORMATEX, audio.Format, error) {
	var lastErr error
	for _, rate := range []int{48000, 44100, 96000} {
		for _, bits := range []int{16, 24, 32} {
			for _, ch := range []int{2, 1} {
				var ac *wca.IAudioClient
				if err := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
					return nil, nil, audio.Format{}, fmt.Errorf("Activate: %v", err)
				}
				align := ch * bits / 8
				w := &wca.WAVEFORMATEX{
					WFormatTag:      1, // PCM
					NChannels:       uint16(ch),
					NSamplesPerSec:  uint32(rate),
					NAvgBytesPerSec: uint32(rate * align),
					NBlockAlign:     uint16(align),
					WBitsPerSample:  uint16(bits),
				}
				f := audio.Format{Rate: rate, Channels: ch, Bits: bits, BlockAlign: align}
				var err error
				for _, dur := range []wca.REFERENCE_TIME{1000000, 500000, 200000} { // 100, 50, 20 мс
					err = ac.Initialize(wca.AUDCLNT_SHAREMODE_EXCLUSIVE, 0, dur, dur, w, nil)
					if err != nil && strings.Contains(err.Error(), "2290679833") { // 0x88890019: буфер не выровнен
						var frames uint32
						if e := ac.GetBufferSize(&frames); e == nil && frames > 0 {
							ac.Release()
							if e := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); e != nil {
								return nil, nil, f, e
							}
							d2 := wca.REFERENCE_TIME(10000000.0*float64(frames)/float64(rate) + 0.5)
							err = ac.Initialize(wca.AUDCLNT_SHAREMODE_EXCLUSIVE, 0, d2, d2, w, nil)
						}
					}
					if err == nil {
						return ac, w, f, nil
					}
					ac.Release()
					if e := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); e != nil {
						return nil, nil, f, e
					}
				}
				lastErr = err
				ac.Release()
			}
		}
	}
	return nil, nil, audio.Format{}, lastErr
}

// openCaptureLowLat открывает микрофон в общем режиме, но с минимальным периодом (IAudioClient3, Windows 10+).

func openCaptureLowLat(dev *wca.IMMDevice) (*wca.IAudioClient, audio.Format, uint32, error) {
	var ac3 *wca.IAudioClient3
	if err := dev.Activate(wca.IID_IAudioClient3, wca.CLSCTX_ALL, nil, &ac3); err != nil {
		return nil, audio.Format{}, 0, fmt.Errorf("IAudioClient3: %v", err)
	}
	ac := &ac3.IAudioClient2.IAudioClient
	var wfx *wca.WAVEFORMATEX
	if err := ac.GetMixFormat(&wfx); err != nil {
		ac.Release()
		return nil, audio.Format{}, 0, fmt.Errorf("GetMixFormat: %v", err)
	}
	f, err := audio.ParseWaveFormat(unsafe.Slice((*byte)(unsafe.Pointer(wfx)), 18+int(wfx.CbSize)))
	if err != nil {
		ac.Release()
		return nil, f, 0, err
	}
	var def, fund, mn, mx uint32
	if err := ac3.GetSharedModeEnginePeriod(wfx, &def, &fund, &mn, &mx); err != nil {
		ac.Release()
		return nil, f, 0, fmt.Errorf("GetSharedModeEnginePeriod: %v", err)
	}
	if mn == 0 || mn >= def {
		ac.Release()
		return nil, f, 0, fmt.Errorf("драйвер не умеет период меньше %d кадров", def)
	}
	if err := ac3.InitializeSharedAudioStream(wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK, mn, wfx, nil); err != nil {
		ac.Release()
		return nil, f, 0, fmt.Errorf("InitializeSharedAudioStream: %v", err)
	}
	return ac, f, mn, nil
}

// deviceFormat — формат, в котором устройство работает в Windows.

func deviceFormat(dev *wca.IMMDevice) []byte {
	var ps *wca.IPropertyStore
	if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return nil
	}
	defer ps.Release()
	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_AudioEngine_DeviceFormat, &pv); err != nil || pv.VT != 0x41 { // VT_BLOB
		return nil
	}
	base := unsafe.Pointer(&pv)
	cb := *(*uint32)(unsafe.Add(base, 8))
	ptr := *(**byte)(unsafe.Add(base, 16))
	if ptr == nil || cb < 16 || cb > 1024 {
		return nil
	}
	return append([]byte(nil), unsafe.Slice(ptr, cb)...)
}

// waveExt собирает WAVEFORMATEXTENSIBLE: container — бит на отсчёт в памяти, valid — значащих.
func waveExt(rate, ch, container, valid int, float bool) []byte {
	b := make([]byte, 40)
	le := binary.LittleEndian
	align := ch * container / 8
	le.PutUint16(b[0:], 0xFFFE)
	le.PutUint16(b[2:], uint16(ch))
	le.PutUint32(b[4:], uint32(rate))
	le.PutUint32(b[8:], uint32(rate*align))
	le.PutUint16(b[12:], uint16(align))
	le.PutUint16(b[14:], uint16(container))
	le.PutUint16(b[16:], 22)
	le.PutUint16(b[18:], uint16(valid))
	mask := uint32(0x4) // моно — «центр»
	if ch == 2 {
		mask = 0x3
	}
	le.PutUint32(b[20:], mask)
	sub := uint32(1) // KSDATAFORMAT_SUBTYPE_PCM
	if float {
		sub = 3 // IEEE_FLOAT
	}
	le.PutUint32(b[24:], sub)
	le.PutUint16(b[28:], 0x0000)
	le.PutUint16(b[30:], 0x0010)
	copy(b[32:], []byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71})
	return b
}

func waveEx(rate, ch, bits int) []byte {
	b := make([]byte, 18)
	le := binary.LittleEndian
	align := ch * bits / 8
	le.PutUint16(b[0:], 1)
	le.PutUint16(b[2:], uint16(ch))
	le.PutUint32(b[4:], uint32(rate))
	le.PutUint32(b[8:], uint32(rate*align))
	le.PutUint16(b[12:], uint16(align))
	le.PutUint16(b[14:], uint16(bits))
	return b
}

// audioErr — понятное название частых ошибок WASAPI.
func audioErr(err error) string {
	e := err.Error()
	for code, txt := range map[string]string{
		"2290679816": "формат не подходит",
		"2290679840": "период не подходит",
		"2290679818": "устройство занято",
		"2290679822": "монопольный режим запрещён",
		"2290679833": "период не выровнен",
	} {
		if strings.Contains(e, code) {
			return txt
		}
	}
	return firstLine(e)
}

// openCaptureExclusive открывает микрофон монопольно с малым периодом, по событию.

func openCaptureExclusive(dev *wca.IMMDevice) (*wca.IAudioClient, audio.Format, uint32, error) {
	activate := func() (*wca.IAudioClient, error) {
		var ac *wca.IAudioClient
		if err := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
			return nil, fmt.Errorf("Activate: %v", err)
		}
		return ac, nil
	}
	probe, err := activate()
	if err != nil {
		return nil, audio.Format{}, 0, err
	}
	defer func() {
		if probe != nil {
			probe.Release()
		}
	}()
	var defP, minP wca.REFERENCE_TIME
	if err := probe.GetDevicePeriod(&defP, &minP); err != nil || minP <= 0 {
		minP = 30000
	}
	if minP < 20000 {
		minP = 20000
	}
	if micExclMinP.Load() < 0 {
		return nil, audio.Format{}, 0, errMicLoses
	}
	if fl := wca.REFERENCE_TIME(micExclMinP.Load()); minP < fl { // на таком периоде уже были сбои
		minP = fl
	}
	periods := []wca.REFERENCE_TIME{minP}
	for _, p := range []wca.REFERENCE_TIME{30000, 50000, 100000} { // 3, 5, 10 мс — если первый не пойдёт
		if p > minP {
			periods = append(periods, p)
		}
	}

	// кандидаты форматов
	var cands [][]byte
	devFmt := deviceFormat(dev)
	devDesc := "неизвестен"
	rates, chans := []int{}, []int{}
	if devFmt != nil {
		cands = append(cands, devFmt)
		if df, e := audio.ParseWaveFormat(devFmt); e == nil {
			devDesc = df.String()
			rates, chans = append(rates, df.Rate), append(chans, df.Channels)
		}
	}
	var mix *wca.WAVEFORMATEX
	if probe.GetMixFormat(&mix) == nil {
		if mf, e := audio.ParseWaveFormat(unsafe.Slice((*byte)(unsafe.Pointer(mix)), 18+int(mix.CbSize))); e == nil {
			rates, chans = append(rates, mf.Rate), append(chans, mf.Channels)
		}
	}
	rates = append(rates, 48000, 44100, 96000, 32000, 16000)
	chans = append(chans, 1, 2)
	seenR, seenC := map[int]bool{}, map[int]bool{}
	for _, r := range rates {
		if seenR[r] {
			continue
		}
		seenR[r] = true
		seenC = map[int]bool{}
		for _, c := range chans {
			if seenC[c] || c < 1 || c > 2 {
				continue
			}
			seenC[c] = true
			cands = append(cands,
				waveExt(r, c, 16, 16, false), waveEx(r, c, 16),
				waveExt(r, c, 24, 24, false), waveExt(r, c, 32, 24, false),
				waveExt(r, c, 32, 32, false), waveExt(r, c, 32, 32, true), waveEx(r, c, 24))
		}
	}

	lastErr := fmt.Errorf("ни один формат не подошёл (формат устройства: %s)", devDesc)
	for _, raw := range cands {
		f, e := audio.ParseWaveFormat(raw)
		if e != nil {
			continue
		}
		w := (*wca.WAVEFORMATEX)(unsafe.Pointer(&raw[0]))
		if err := probe.IsFormatSupported(wca.AUDCLNT_SHAREMODE_EXCLUSIVE, w, nil); err != nil {
			if s := err.Error(); strings.Contains(s, "2290679818") || strings.Contains(s, "2290679822") {
				return nil, f, 0, err
			}
			continue
		}
		for _, per := range periods {
			ac, err := activate()
			if err != nil {
				return nil, f, 0, err
			}
			err = ac.Initialize(wca.AUDCLNT_SHAREMODE_EXCLUSIVE, wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK, per, per, w, nil)
			if err != nil && strings.Contains(err.Error(), "2290679833") { // 0x88890019: период не выровнен
				var frames uint32
				if e := ac.GetBufferSize(&frames); e == nil && frames > 0 {
					ac.Release()
					if ac, err = activate(); err != nil {
						return nil, f, 0, err
					}
					d2 := wca.REFERENCE_TIME(10000000.0*float64(frames)/float64(f.Rate) + 0.5)
					err = ac.Initialize(wca.AUDCLNT_SHAREMODE_EXCLUSIVE, wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK, d2, d2, w, nil)
				}
			}
			if err == nil {
				var frames uint32
				if e := ac.GetBufferSize(&frames); e != nil || frames == 0 {
					frames = uint32(int64(per) * int64(f.Rate) / 10000000)
				}
				return ac, f, frames, nil
			}
			ac.Release()
			lastErr = fmt.Errorf("%s: %s (формат устройства: %s)", f, audioErr(err), devDesc)
			if s := err.Error(); strings.Contains(s, "2290679818") || strings.Contains(s, "2290679822") {
				return nil, f, 0, err
			}
		}
	}
	return nil, audio.Format{}, 0, lastErr
}

// ---------- захват (микрофон / loopback) ----------

var (
	errMicLoses = errors.New("монопольно этот микрофон теряет звук")
	errMicDrops = errors.New(L("монопольный микрофон теряет звук", "exclusive mic drops audio"))
)

type ready struct {
	label string
	name  string
	f     audio.Format
	err   error
}

const (
	modeMono = iota // все каналы в один
	modeLR          // левая и правая «стороны»
	modeRaw         // как есть, по каналам
)

// micMode для runCapture: как открывать микрофон.
const (
	capNormal = iota // общий режим как у всех (loopback — всегда так)
	capLowLat        // общий режим с малым периодом, если драйвер умеет
	capExclLow
)

func runCapture(label, sub string, flow uint32, loopback bool, mode int, stream *pipe.StreamIn,
	stop *atomic.Bool, readyCh chan<- ready, errCh chan<- error, wg *sync.WaitGroup, how int) {
	defer wg.Done()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()
	defer proAudioThread()()
	report := how != capNormal
	if report {
		micPeriodUs.Store(0)
		micExclActive.Store(false)
		micExclWhy.Store("")
		micGlitches.Store(0)
		micOwnClock.Store(false)
		micLossPct.Store(0)
	}

	fail := func(err error) {
		readyCh <- ready{err: fmt.Errorf("%s: %v", label, err)}
	}
	dev, name, err := pickDevice(flow, sub, label)
	if err != nil {
		readyCh <- ready{err: err}
		return
	}
	defer dev.Release()
	if loopback && isCable(name) {
		fail(fmt.Errorf(L("устройство вывода по умолчанию — «%s». Выбери наушники в меню hush или сделай их устройством по умолчанию",
			"the default output device is \"%s\". Pick your headphones in the hush menu or make them the default device"), name))
		return
	}
	var ac *wca.IAudioClient
	var f audio.Format
	var event uintptr // != 0 — звук приходит по событию (малый период)
	var periodFrames uint32
	note := ""
	if !loopback && how != capNormal {
		openEvent := func(a *wca.IAudioClient) bool {
			h, _, _ := procCreateEventW.Call(0, 0, 0, 0)
			if h == 0 {
				return false
			}
			if err := a.SetEventHandle(h); err != nil {
				procCloseHandle.Call(h)
				return false
			}
			event = h
			return true
		}
		if how == capExclLow {
			if a, ff, pf, e := openCaptureExclusive(dev); e == nil {
				if openEvent(a) {
					ac, f, periodFrames = a, ff, pf
					micExclActive.Store(true)
					note = " [монопольно]"
				} else {
					a.Release()
				}
			} else {
				micExclFail(dev, name, e)
			}
		}
		if ac == nil {
			if a, ff, pf, e := openCaptureLowLat(dev); e == nil {
				if openEvent(a) {
					ac, f, periodFrames = a, ff, pf
				} else {
					a.Release()
				}
			}
		}
	}
	if event != 0 {
		defer procCloseHandle.Call(event)
	}
	if ac == nil {
		a, wfx, ff, err := openClient(dev)
		if err != nil {
			fail(err)
			return
		}
		flags := uint32(0)
		if loopback {
			flags = wca.AUDCLNT_STREAMFLAGS_LOOPBACK
		}
		if err := a.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, wca.REFERENCE_TIME(1000000), 0, wfx, nil); err != nil {
			a.Release()
			fail(fmt.Errorf("Initialize: %v", err))
			return
		}
		ac, f = a, ff
		var defP, minP wca.REFERENCE_TIME
		if a.GetDevicePeriod(&defP, &minP) == nil && defP > 0 {
			periodFrames = uint32(int64(defP) * int64(f.Rate) / 10000000)
		} else {
			periodFrames = uint32(f.Rate / 100)
		}
	}
	defer ac.Release()
	if f.Rate > 0 && periodFrames > 0 {
		us := int64(periodFrames) * 1000000 / int64(f.Rate)
		if report {
			micPeriodUs.Store(us)
		}
		note += fmt.Sprintf(" [пачки %.1f мс]", float64(us)/1000)
	}
	var acc *wca.IAudioCaptureClient
	if err := ac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
		fail(fmt.Errorf("GetService: %v", err))
		return
	}
	defer acc.Release()
	if err := ac.Start(); err != nil {
		fail(fmt.Errorf("Start: %v", err))
		return
	}
	defer ac.Stop()
	readyCh <- ready{label: label, name: name + note, f: f}

	nout := 1
	switch mode {
	case modeLR:
		nout = 2
	case modeRaw:
		nout = f.Channels
	}
	rss := make([]*audio.Resampler, nout)
	for i := range rss {
		rss[i] = audio.NewResampler(f.Rate, pipe.Rate, false)
	}
	chs := make([][]float32, f.Channels)
	var lastQ, lastDur int64
	badTS := 0
	glitchN, glitchT, started := 0, time.Now(), false
	var rateGot int64
	var firstPkt time.Time
	measuring := false
	mixed := make([][]float32, nout)
	outs := make([][]float32, nout)
	tk := newTick()
	defer tk.close()
	stN, stMax := &refStalls, &refStallMax
	if !loopback {
		stN, stMax = &micStalls, &micStallMax
	}
	if report || loopback {
		stN.Store(0)
		stMax.Store(0)
	}
	lastWake := time.Now()
	for !stop.Load() {
		timedOut := false
		if event != 0 {
			r, _, _ := procWaitSingle.Call(event, 100) // пакет готов — Windows будит сразу
			timedOut = r != 0                          // устройство молчит — это не «зависание» потока
		} else {
			tk.wait()
		}
		if (report || loopback) && !timedOut {
			noteStall(stN, stMax, time.Since(lastWake))
		}
		lastWake = time.Now()
		for {
			var n uint32
			if err := acc.GetNextPacketSize(&n); err != nil {
				errCh <- fmt.Errorf("%s: GetNextPacketSize: %v (устройство отключено?)", label, err)
				return
			}
			if n == 0 {
				break
			}
			var data *byte
			var frames, bflags uint32
			var devPos, qpc uint64
			if err := acc.GetBuffer(&data, &frames, &bflags, &devPos, &qpc); err != nil {
				errCh <- fmt.Errorf("%s: GetBuffer: %v", label, err)
				return
			}
			if bflags&wca.AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY != 0 && started {
				glitchN++
				if report {
					micGlitches.Add(1)
				}
			}
			if !started {
				firstPkt = time.Now()
			}
			started = true

			if micExclActive.Load() && report && !measuring && time.Since(firstPkt) > time.Second {
				measuring, glitchN, glitchT, rateGot = true, 0, time.Now(), 0
			} else if micExclActive.Load() && report && measuring {
				rateGot += int64(frames)
				if el := time.Since(glitchT); el > 3*time.Second {
					want := el.Seconds() * float64(f.Rate)
					loss := 1 - float64(rateGot)/want
					if loss < 0 {
						loss = 0
					}
					micLossPct.Store(int64(loss*1000 + 0.5)) // в десятых долях процента

					if loss > 0.005 || glitchN >= 10 {
						cur := int64(periodFrames) * 10000000 / int64(f.Rate)
						next := cur + 20000 // +2 мс
						var why error
						if next > 100000 {
							micExclMinP.Store(-1)
							why = fmt.Errorf(L("%w: %.0f%% даже пачками по %.1f мс, включён обычный захват", "%w: %.0f%% even with %.1f ms packets, switched to shared capture"), errMicDrops, loss*100, float64(cur)/10000)
						} else {
							micExclMinP.Store(next)
							why = fmt.Errorf(L("%w: %.0f%%, сбоев %d, пробую пачки по %.1f мс", "%w: %.0f%%, %d glitches, trying %.1f ms packets"), errMicDrops, loss*100, glitchN, float64(next)/10000)
						}
						acc.ReleaseBuffer(frames)
						errCh <- why
						return
					}
					glitchN, glitchT, rateGot = 0, time.Now(), 0
				}
			}
			for c := range chs {
				chs[c] = chs[c][:0]
			}
			if bflags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 || data == nil {
				for c := range chs {
					for i := uint32(0); i < frames; i++ {
						chs[c] = append(chs[c], 0)
					}
				}
			} else {
				f.DecodeChannels(unsafe.Slice(data, int(frames)*f.BlockAlign), int(frames), chs)
			}
			acc.ReleaseBuffer(frames)

			switch mode {
			case modeMono:
				mixed[0] = audio.MonoOf(chs, mixed[0][:0])
			case modeLR:
				mixed[0], mixed[1] = audio.MixLR(chs, mixed[0][:0], mixed[1][:0])
			default:
				for c := range chs {
					mixed[c] = append(mixed[c][:0], chs[c]...)
				}
			}

			dur := int64(frames) * 10000000 / int64(f.Rate)
			now := nowQPC()
			obs := now - dur // не раньше этого пакет мог закончиться записываться
			q := int64(qpc)
			if bflags&wca.AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR != 0 || q == 0 || q > now+20000 || now-q > dur+500000 {
				badTS++
				q = obs
				if lastQ != 0 {
					exp := lastQ + lastDur
					switch d := obs - exp; {
					case d < -300000 || d > 300000: // разрыв больше 30 мс — начинаем заново
					case d < 0: // пакет пришёл раньше расчётного — наш счёт отстал
						q = obs
					default:
						q = exp + d/200
					}
				}
			}
			lastQ, lastDur = q, dur
			if report && badTS == 50 { // устойчиво кривые метки, а не случайный сбой
				micOwnClock.Store(true)
			}
			for i := range rss {
				outs[i] = rss[i].Process(mixed[i], outs[i][:0])
			}
			stream.Push(outs, q)
		}
	}
}

// ---------- вывод в виртуальный кабель + обработка ----------

func runRender(sub string, forceShared bool, eng *pipe.Engine, t0 int64, stop *atomic.Bool,
	readyCh chan<- ready, errCh chan<- error, wg *sync.WaitGroup, micS, refS *pipe.StreamIn, quiet bool) {
	defer wg.Done()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()
	defer proAudioThread()()

	fail := func(err error) { readyCh <- ready{err: fmt.Errorf(L("вывод: %v", "output: %v"), err)} }
	dev, name, err := pickDeviceEx(wca.ERender, sub, L("вывод", "output"), true)
	if err != nil {
		readyCh <- ready{err: err}
		return
	}
	defer dev.Release()
	var ac *wca.IAudioClient
	var wfx *wca.WAVEFORMATEX
	var f audio.Format
	exclusive := false
	if !forceShared {
		if a, w, ff, e := openExclusive(dev); e == nil {
			ac, wfx, f, exclusive = a, w, ff, true
		} else {
			fmt.Printf("\n(монопольный режим кабеля не вышел: %v — работаю обычным)\n", e)
		}
	}
	if !exclusive {
		var e error
		ac, wfx, f, e = openClient(dev)
		if e != nil {
			fail(e)
			return
		}
		if e := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, 0, wca.REFERENCE_TIME(2000000), 0, wfx, nil); e != nil {
			fail(fmt.Errorf("Initialize: %v", e))
			return
		}
	}
	_ = wfx
	defer ac.Release()
	var bufFrames uint32
	if err := ac.GetBufferSize(&bufFrames); err != nil {
		fail(err)
		return
	}
	var arc *wca.IAudioRenderClient
	if err := ac.GetService(wca.IID_IAudioRenderClient, &arc); err != nil {
		fail(fmt.Errorf("GetService: %v", err))
		return
	}

	micBase := func() float64 {
		p := 10.0
		if us := micPeriodUs.Load(); us > 0 {
			p = float64(us) / 1000
		}
		return pipe.MicBaseMs(p)
	}
	curBase := micBase()
	sinkMs := int(math.Ceil(curBase))
	exclActive.Store(exclusive)
	if exclusive {
		name += " [монопольно]"
	}
	defer arc.Release()
	sink := pipe.NewSink(f.Rate, sinkMs)
	sink.SetBaseMs(curBase)
	sink.QuietSlack = f.Rate * 4 / 1000 // больше 4 мс лишнего — выкидываем в тишине
	// предзаполняем тишиной, чтобы на старте не было недобора
	pre := uint32(sink.Target)
	var p *byte
	if err := arc.GetBuffer(pre, &p); err != nil {
		fail(fmt.Errorf("GetBuffer: %v", err))
		return
	}
	arc.ReleaseBuffer(pre, wca.AUDCLNT_BUFFERFLAGS_SILENT)
	if err := ac.Start(); err != nil {
		fail(fmt.Errorf("Start: %v", err))
		return
	}
	defer ac.Stop()
	readyCh <- ready{label: "вывод", name: name, f: f}

	var out []float32
	lastStat := time.Now()
	startT := time.Now()
	outBufMs.Store(int64(sinkMs))
	outLatMs.Store(0)
	defer outLatMs.Store(0)
	var latSum float64
	var latN int
	latT := time.Now()
	outStarves.Store(0)
	evT := time.Now()
	var pStarve, pDrops, pLate, pMicRs, pRefRs int
	var pQuiet int
	var pGlitch, pMicSt, pRefSt, pOutSt int64
	var supT time.Time
	logEvent("старт: вывод «%s» [%v], запас %.0f мс, микрофон пачками по %.1f мс", name, f, curBase, float64(micPeriodUs.Load())/1000)
	logCounters := func() {
		fillMs := func() float64 { return float64(sink.Target) * 1000 / float64(f.Rate) }
		if sink.Starves != pStarve {
			outStarves.Store(int64(sink.Starves))
			logEvent("ПРОВАЛ звука в кабеле ×%d (данных не хватило) — запас теперь %.0f мс", sink.Starves-pStarve, fillMs())
			pStarve = sink.Starves
		}
		if sink.Drops != pDrops {
			logEvent("выкинут кусок звука целиком ×%d (накопилось больше 80 мс задержки)", sink.Drops-pDrops)
			pDrops = sink.Drops
		}
		if sink.QuietDrops != pQuiet {
			logEvent("выкинуто %.1f мс в паузах (лишняя задержка)", float64(sink.QuietDrops-pQuiet)*1000/float64(f.Rate))
			pQuiet = sink.QuietDrops
		}
		if eng.LateRef != pLate {
			logEvent("ждали звук наушников ×%d (дольше всего %.0f мс) — выход в это время стоял", eng.LateRef-pLate, eng.LateMaxMs)
			pLate = eng.LateRef
			eng.LateMaxMs = 0
		}
		if r := micS.ResyncCount(); r != pMicRs {
			logEvent("микрофон: разрыв во времени ×%d (пакеты пришли не подряд)", r-pMicRs)
			pMicRs = r
		}
		if r := refS.ResyncCount(); r != pRefRs {
			if r-pRefRs > 1 || time.Since(startT) < 5*time.Second {
				logEvent("наушники: разрыв во времени ×%d (обычно — звук в наушниках начался/кончился)", r-pRefRs)
			}
			pRefRs = r
		}
		for _, x := range []struct {
			n, mx *atomic.Int64
			p     *int64
			what  string
		}{{&micStalls, &micStallMax, &pMicSt, "поток микрофона"}, {&refStalls, &refStallMax, &pRefSt, "поток наушников"}, {&outStalls, &outStallMax, &pOutSt, "поток вывода"}} {
			if v := x.n.Load(); v != *x.p {
				logEvent("%s не получал процессор дольше 30 мс ×%d (самое долгое %d мс) — ПК был занят или Windows притормозила программу", x.what, v-*x.p, x.mx.Load())
				*x.p = v
				x.mx.Store(0)
			}
		}
		if g := micGlitches.Load(); g != pGlitch {
			logEvent("микрофон: драйвер сообщил о пропуске данных ×%d", g-pGlitch)
			pGlitch = g
		}
		if eng.S != nil && time.Since(supT) > 5*time.Second {
			if st := eng.S.Stats(); st.Voice && st.RefDB > -60 && st.ReductionDB > 6 {
				supT = time.Now()
				logEvent("подавитель глушит микрофон на %.0f дБ, пока ты говоришь и в наушниках звук (сила %.1f) — если голос рвётся, это оно", st.ReductionDB, eng.S.Beta())
			}
		}
	}
	defer logEvent("стоп вывода")
	tk := newTick()
	defer tk.close()
	outStalls.Store(0)
	outStallMax.Store(0)
	lastWake := time.Now()
	for !stop.Load() {
		tk.wait()
		noteStall(&outStalls, &outStallMax, time.Since(lastWake))
		lastWake = time.Now()
		var padding uint32
		if err := ac.GetCurrentPadding(&padding); err != nil {
			errCh <- fmt.Errorf("вывод: GetCurrentPadding: %v (устройство отключено?)", err)
			return
		}
		idx := pipe.IdxFromQPC(nowQPC(), t0)
		eng.Hold = sink.HoldFor(int(padding), pipe.Rate*31/1000) // ждём опору, только пока есть запас
		out = eng.Step(idx, out[:0])
		sink.Feed(out)
		if b := micBase(); b != curBase {
			curBase = b
			sink.SetBaseMs(b)
		}
		sink.Adapt(int(padding), time.Since(startT))
		outBufMs.Store(int64(sink.Target * 1000 / f.Rate))
		chunk := sink.Take(int(padding), int(bufFrames-padding))
		if len(chunk) > 0 {
			ditherQuiet(chunk)
			var dst *byte
			if err := arc.GetBuffer(uint32(len(chunk)), &dst); err != nil {
				errCh <- fmt.Errorf("вывод: GetBuffer: %v", err)
				return
			}
			f.EncodeMono(chunk, unsafe.Slice(dst, len(chunk)*f.BlockAlign))
			arc.ReleaseBuffer(uint32(len(chunk)), 0)
		}

		if nx, ok := eng.Next(); ok {
			fill := float64(int(padding)+len(chunk)+sink.Queued()) * pipe.Rate / float64(f.Rate)
			latSum += (float64(idx-(nx-1)+eng.AlgoDelay()) + fill) * 1000 / pipe.Rate
			latN++
		}
		if time.Since(latT) >= 500*time.Millisecond {
			if latN > 0 {
				outLatMs.Store(int64(latSum/float64(latN) + 0.5))
			}
			latSum, latN, latT = 0, 0, time.Now()
		}

		if time.Since(evT) >= time.Second {
			evT = time.Now()
			logCounters()
		}
		if !quiet && time.Since(lastStat) >= time.Second {
			lastStat = time.Now()
			var line string
			if eng.S != nil {
				st := eng.S.Stats()
				state := ""
				switch {
				case st.Calibrating:
					state = "КАЛИБРОВКА — молчи"
				case !st.Found:
					state = "наводка не найдена, микрофон как есть"
				case st.RefDB < -80:
					state = "в наушниках тишина"
				default:
					state = fmt.Sprintf("наводка убрана на %4.1f дБ", st.ReductionDB)
				}
				line = fmt.Sprintf("мик %4.0f науш %4.0f вых %4.0f дБ | %s", st.MicDB, st.RefDB, st.OutDB, state)
			} else {
				s := eng.F.Stats()
				sub := "  --  "
				if s.RefDB > -80 {
					sub = fmt.Sprintf("%+5.1f", -s.ERLEdB)
				}
				echo := "нет"
				if lag, conf, ok := eng.Estimate(); ok {
					echo = fmt.Sprintf("%+.0fмс(%.0f)", lag, conf)
				}
				line = fmt.Sprintf("мик %4.0f науш %4.0f вых %4.0f | изм %s дБ | эхо %s | сбр %d пр %d син %d/%d",
					s.MicDB, s.RefDB, s.OutDB, sub, echo, s.Resets, eng.Corrections, micS.ResyncCount(), refS.ResyncCount())
			}
			g := fmt.Sprintf(" | усил %+.0f дБ", eng.Amp.GainDB())
			if l := eng.Amp.Limiting(); l > 1 {
				g += fmt.Sprintf(" (огр. -%.0f)", l)
			}
			fmt.Printf("\r%-110s", line+g)
		}
	}
}

// recordMode пишет hush-rec.zip: микрофон и все каналы наушников на общей шкале времени.
func recordMode(secs int, micName, refName string) {
	t0 := nowQPC()
	size := pipe.Rate * (secs + 8)
	micS := pipe.NewStreamIn(size, t0, 0)
	refS := pipe.NewStreamIn(size, t0, 0)
	var stop atomic.Bool
	var wg sync.WaitGroup
	readyCh := make(chan ready, 2)
	errCh := make(chan error, 2)
	wg.Add(2)
	go runCapture("микрофон", micName, wca.ECapture, false, modeMono, micS, &stop, readyCh, errCh, &wg, capNormal)
	go runCapture("наушники (loopback)", refName, wca.ERender, true, modeRaw, refS, &stop, readyCh, errCh, &wg, capNormal)
	var info bytes.Buffer
	fmt.Fprintf(&info, "hush %s, запись %d с\n", Version, secs)
	for i := 0; i < 2; i++ {
		r := <-readyCh
		if r.err != nil {
			stop.Store(true)
			wg.Wait()
			fatal("%v", r.err)
		}
		fmt.Printf("  • %s: %s [%v]\n", r.label, r.name, r.f)
		fmt.Fprintf(&info, "%s: %s [%v]\n", r.label, r.name, r.f)
	}
	half := secs / 2
	fmt.Println()
	fmt.Printf(L("СЕЙЧАС (первые %d с): включи музыку/видео ГРОМКО, как обычно играешь, и МОЛЧИ.\n", "NOW (first %d s): play music or a video at your usual volume and STAY QUIET.\n"), half)
	start := time.Now()
	phase2 := false
	for time.Since(start) < time.Duration(secs)*time.Second {
		select {
		case err := <-errCh:
			stop.Store(true)
			wg.Wait()
			fatal("%v", err)
		case <-time.After(200 * time.Millisecond):
		}
		el := int(time.Since(start).Seconds())
		if !phase2 && el >= half {
			phase2 = true
			fmt.Printf(L("\nТЕПЕРЬ (ещё %d с): музыка продолжает играть, а ты ГОВОРИ в микрофон — читай что-нибудь вслух.\n", "\nNOW (another %d s): keep the music playing and TALK into the mic, read something aloud.\n"), secs-half)
		}
		fmt.Printf(L("\r  %2d / %d с   ", "\r  %2d / %d s   "), el, secs)
	}
	stop.Store(true)
	wg.Wait()
	fmt.Println(L("\nГотово, сохраняю...", "\nDone, saving..."))

	chans, notes, err := pipe.Extract(micS, refS, pipe.IdxFromQPC(nowQPC(), t0))
	if err != nil {
		fatal("%v", err)
	}
	for _, l := range notes {
		fmt.Fprintln(&info, l)
	}

	path := dataFile("rec.zip")
	fz, err := os.Create(path)
	if err != nil {
		fatal(L("не могу создать %s: %v", "cannot create %s: %v"), path, err)
	}
	zw := zip.NewWriter(fz)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "rec.wav", Method: zip.Deflate})
	if err := audio.WriteWAV24(w, pipe.Rate, chans); err != nil {
		fatal(L("запись WAV: %v", "writing WAV: %v"), err)
	}
	w, _ = zw.CreateHeader(&zip.FileHeader{Name: "info.txt", Method: zip.Deflate})
	w.Write(info.Bytes())
	if err := zw.Close(); err != nil {
		fatal("zip: %v", err)
	}
	fz.Close()
	st, _ := os.Stat(path)
	fmt.Println()
	fmt.Print(info.String())
	fmt.Printf(L("\nФайл: %s (%.1f МБ)\nПриложи его к issue на GitHub. Нажми Enter, чтобы закрыть окно...\n", "\nFile: %s (%.1f MB)\nAttach it to a GitHub issue. Press Enter to close...\n"), path, float64(st.Size())/1e6)
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			if cf, err := os.OpenFile(dataFile("crash.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
				fmt.Fprintf(cf, "\r\nПАНИКА %s: %v\r\n%s\r\n", time.Now().Format("2006-01-02 15:04:05"), r, debug.Stack())
				cf.Close()
			}
			panic(r)
		}
	}()
	micName := flag.String("mic", "", L("часть названия микрофона (по умолчанию — микрофон Windows по умолчанию)", "part of the microphone name (default: Windows default mic)"))
	outName := flag.String("out", "CABLE Input", L("часть названия устройства вывода (виртуальный кабель)", "part of the output device name (virtual cable)"))
	refName := flag.String("ref", "", L("часть названия наушников (по умолчанию — устройство вывода Windows по умолчанию)", "part of the headphones name (default: Windows default output)"))
	list := flag.Bool("list", false, L("показать устройства и выйти", "list devices and exit"))
	doctor := flag.Bool("doctor", false, L("найти, откуда берётся эхо (проиграет короткое шипение)", "find where an echo comes from (plays a short noise)"))
	record := flag.Int("record", 0, L("записать N секунд (микрофон + наушники) в hush-rec.zip и выйти", "record N seconds of mic and headphones to hush-rec.zip and exit"))
	bypass := flag.Bool("bypass", false, L("ничего не вычитать (для сравнения «на слух»)", "pass the mic through unprocessed (for comparison)"))
	gainFlag := flag.Float64("gain", -999, L("усиление микрофона, дБ (по умолчанию — из hush-config.json)", "mic gain, dB (default: from hush-config.json)"))
	linear := flag.Bool("linear", false, L("старый режим: линейное вычитание вместо подавителя наводки", "legacy mode: linear adaptive filter instead of the suppressor"))
	recal := flag.Bool("recal", false, L("заново откалибровать (шипение 6 с)", "calibrate again on start"))
	monoRef := flag.Bool("monoref", false, L("сводить наушники в одну опору (по умолчанию — левая и правая раздельно)", "mix headphone channels into one reference"))
	mu := flag.Float64("mu", 0.3, L("скорость адаптации фильтра (0.1..1)", "filter adaptation rate (0.1..1), legacy mode"))
	delay := flag.Int("delay-ms", 20, L("запас по времени для опоры, мс (обычно трогать не нужно)", "reference timing margin, ms"))
	parts := flag.Int("partitions", aec.DefaultPartitions, L("длина фильтра в блоках по 5.3 мс (24 ≈ 128 мс)", "filter length in 5.3 ms blocks, legacy mode"))
	micExcl := flag.Bool("micexcl", false, L("захватывать микрофон монопольно (меньше задержка; другие программы его в это время не видят)", "capture the mic exclusively (lower latency, other programs lose it)"))
	shared := flag.Bool("shared", false, L("выводить в кабель обычным (общим) режимом; по умолчанию монопольный, чтобы демка экрана не ловила звук", "shared cable output (default is exclusive, hidden from screen sharing)"))
	beta := flag.Float64("beta", 1.5, L("сила подавителя наводки (1 — мягко, 2 — жёстко, как в первой версии)", "suppression strength (1 soft, 1.5 normal, 2 strong)"))
	quiet := flag.Bool("quiet", false, L("не печатать строку состояния", "do not print the status line"))
	noauto := flag.Bool("noauto", false, L("не подстраивать задержку опоры автоматически", "do not auto-adjust reference delay, legacy mode"))
	noui := flag.Bool("noui", false, L("без панели и значка в трее, только консоль", "no panel or tray icon, console only"))
	background := flag.Bool("background", false, L("запуск в фоне, только значок в трее (так запускает автозапуск)", "start in the tray without the panel (used by autostart)"))
	withConsole := flag.Bool("console", false, L("показывать консоль со строкой состояния", "show a console with live levels"))
	afterUpdate := flag.Bool("after-update", false, L("служебный: запуск после обновления", "internal: start after an update"))
	version := flag.Bool("version", false, L("показать версию и выйти", "print the version and exit"))
	flag.CommandLine.Usage = func() {
		useConsole(true)
		flag.CommandLine.SetOutput(os.Stdout)
		fmt.Printf("hush %s\n\n", Version)
		flag.PrintDefaults()
	}
	flag.Parse()
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	exe := strings.ToLower(filepath.Base(os.Args[0]))
	if *record == 0 && len(os.Args) == 1 && strings.Contains(exe, "record") {
		*record = 24
	}
	if len(os.Args) == 1 && strings.Contains(exe, "doctor") {
		*doctor = true
	}

	if *version {
		useConsole(true)
		fmt.Println("hush", Version)
		return
	}
	needConsole := *list || *doctor || *record > 0 || *noui || *withConsole
	if needConsole {
		useConsole(true)
	}
	if !*list && !*doctor && *record == 0 && os.Getenv("HUSH_UIDEMO") == "" && !singleInstance(!*background, *afterUpdate) {
		return
	}
	procTimeBeginPeriod.Call(1)
	processForAudio()
	debug.SetGCPercent(400)
	crashPath := dataFile("crash.txt")
	rotate(crashPath, 256*1024)
	if cf, err := os.OpenFile(crashPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(cf, "\r\n=== запуск %s ===\r\n", time.Now().Format("2006-01-02 15:04:05"))
		k32.NewProc("SetStdHandle").Call(^uintptr(11), cf.Fd()) // если программа упадёт — причина будет в этом файле
		debug.SetTraceback("all")
	}
	debug.SetMemoryLimit(300 << 20) // мягкий потолок: ближе к нему сборщик работает чаще
	go memWatch()
	go cleanupOldExe()
	var fq int64
	procQPF.Call(uintptr(unsafe.Pointer(&fq)))
	qpcFreq = fq

	runtime.LockOSThread()
	ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)

	if *list {
		for _, fl := range []struct {
			n string
			v uint32
		}{{L("Микрофоны", "Microphones"), wca.ECapture}, {L("Устройства вывода", "Output devices"), wca.ERender}} {
			fmt.Println(fl.n + ":")
			l, err := deviceNames(fl.v)
			if err != nil {
				fatal("%v", err)
			}
			for _, n := range l {
				fmt.Println("  •", n)
			}
		}
		return
	}

	if *doctor {
		doctorMode()
		return
	}
	if os.Getenv("HUSH_UIDEMO") != "" { // проверка интерфейса без звука
		t := 0.0
		u := startUI(func() uiState {
			t += 0.12
			return uiState{Running: true, Found: true, Voice: math.Sin(t) > 0, MicDB: -50 + 25*math.Sin(t), RefDB: -30 + 20*math.Sin(t*0.7), OutDB: -20 + 12*math.Sin(t),
				Reduction: 9.4, GainDB: 6, Beta: 1.5, Noise: 2, EQ: 1, AGC: true, AGCGain: 7, PTT: 1, PTTKey: keyName(5), PTTOpen: math.Sin(t*0.3) > 0, Cue: true, LatencyMs: 28, ExclWant: true, ExclOn: true, Mic: "Headset", Out: "CABLE Input"}
		}, true)
		for a := range u.acts {
			fmt.Println("действие:", a)
		}
		return
	}
	logEvent("hush %s запущен", Version)
	fmt.Println("hush", Version)
	if *record > 0 {
		if *record < 8 || *record > 60 {
			fatal("-record: от 8 до 60 секунд")
		}
		recordMode(*record, *micName, *refName)
		return
	}

	refMode, nrefs := modeLR, 2
	if *monoRef {
		refMode, nrefs = modeMono, 1
	}
	var sup *aec.Suppressor
	var flt *aec.Filter
	if *linear {
		flt = aec.New(aec.Config{Partitions: *parts, Mu: *mu, Refs: nrefs})
	} else {
		sup = aec.NewSuppressor(pipe.Rate)
		sup.SetBeta(*beta)
	}
	cfg := loadConfig()

	if migrateAutostart() {
		cfg.AutoSet = true
	}
	if !cfg.AutoSet {
		if setAutostart(true) == nil {
			updateConfig(func(c *appConfig) { c.AutoSet = true })
			cfg.AutoSet = true
		}
	} else if autostartOn() {
		setAutostart(true)
	}
	if *gainFlag != -999 {
		cfg.GainDB = *gainFlag
	}
	// сохранённый выбор устройств и силы — если не заданы флагами
	if !explicit["mic"] && cfg.Mic != "" {
		*micName = cfg.Mic
	}
	if !explicit["ref"] && cfg.Ref != "" {
		*refName = cfg.Ref
	}
	if !explicit["out"] && cfg.Out != "" {
		*outName = cfg.Out
	}
	if !explicit["beta"] && cfg.Beta != 0 {
		*beta = cfg.Beta
		if sup != nil {
			sup.SetBeta(cfg.Beta)
		}
	}
	if !explicit["shared"] && cfg.Shared {
		*shared = true
	}
	if !explicit["micexcl"] && cfg.MicEx {
		*micExcl = true
	}
	curCfg = cfg
	amp := pipe.NewAmp(cfg.GainDB)
	dn := aec.NewDenoiser()
	dn.SetStrength(1)
	if cfg.Noise != nil {
		dn.SetStrength(*cfg.Noise)
	}
	eq := pipe.NewEQ()
	eq.SetPreset(cfg.EQ)
	agc := pipe.NewAGC()
	agc.SetEnabled(cfg.AGC)
	ptt := newPTT(cfg.PTT, cfg.PTTKey, func(vk int) {
		updateConfig(func(c *appConfig) { c.PTTKey = vk })
		theUI.Balloon(appName, L("Кнопка микрофона: ", "Mic key: ")+keyName(vk))
	})
	cues := newCuePlayer(func() string {
		nameMu.RLock()
		defer nameMu.RUnlock()
		return *refName
	})
	cues.enabled.Store(!cfg.NoCue)
	ptt.onChange = func(open bool) {
		if open {
			logEvent("микрофон по кнопке: ВКЛ")
		} else {
			logEvent("микрофон по кнопке: ВЫКЛ")
		}
		cues.play(open)
	}
	var curEng atomic.Pointer[pipe.Engine]
	var running atomic.Bool
	var ui *uiCtl
	delaySamples := int64(*delay) * pipe.Rate / 1000

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	keysStarted := false

	var recalNext bool

	handleAction := func(a action, quit *bool) bool {
		switch a.Kind {
		case "gain":
			g := math.Max(-10, math.Min(maxGainDB, amp.GainDB()+a.F))
			amp.SetGainDB(g)
			updateConfig(func(c *appConfig) { c.GainDB = g })
		case "beta":
			if sup != nil {
				sup.SetBeta(a.F)
			}
			updateConfig(func(c *appConfig) { c.Beta = a.F })
		case "noise":
			lv := int(a.F)
			dn.SetStrength(lv)
			updateConfig(func(c *appConfig) { c.Noise = &lv })
		case "eq":
			eq.SetPreset(int(a.F))
			p := eq.Preset()
			updateConfig(func(c *appConfig) { c.EQ = p })
		case "agc":
			on := !agc.Enabled()
			if a.F == 0 {
				on = false
			} else if a.F == 1 {
				on = true
			}
			agc.SetEnabled(on)
			updateConfig(func(c *appConfig) { c.AGC = on })
		case "ptt":
			m := int(a.F)
			ptt.mode.Store(int32(m))
			updateConfig(func(c *appConfig) { c.PTT = m })
			if m != pttOff && ptt.key.Load() == 0 {
				ptt.binding.Store(true)
				ui.Balloon(appName, L("Нажми клавишу или кнопку мыши для микрофона", "Press a key or mouse button for the mic"))
			}
		case "bind":
			ptt.binding.Store(true)
		case "excl":
			want := a.F == 0 // 0 — монопольно, 1 — обычно
			if want == !*shared {
				return false
			}
			*shared = !want
			sh := *shared
			updateConfig(func(c *appConfig) { c.Shared = sh })
			return true // пересобираем цепочку с новым режимом
		case "micexcl":
			want := a.F == 1 // 0 — обычно, 1 — монопольно
			if want == *micExcl {
				return false
			}
			*micExcl = want
			micExclMinP.Store(0) // заново пробуем с самого малого периода
			micExclShown.Store("")
			updateConfig(func(c *appConfig) { c.MicEx = want })
			if want {
				ui.Balloon(appName, L("Микрофон захватывается монопольно. Если наводка вернётся, перекалибруй.",
					"The mic is now captured exclusively. If the bleed comes back, recalibrate."))
			}
			return true // пересобираем цепочку с новым режимом
		case "cue":
			on := !cues.enabled.Load()
			cues.enabled.Store(on)
			updateConfig(func(c *appConfig) { c.NoCue = !on })
			if on {
				cues.play(true)
			}
		case "mic":
			nameMu.Lock()
			*micName = a.S
			nameMu.Unlock()
			updateConfig(func(c *appConfig) { c.Mic = a.S })
			return true
		case "ref":
			nameMu.Lock()
			*refName = a.S
			nameMu.Unlock()
			updateConfig(func(c *appConfig) { c.Ref = a.S })
			return true
		case "out":
			nameMu.Lock()
			*outName = a.S
			nameMu.Unlock()
			updateConfig(func(c *appConfig) { c.Out = a.S })
			return true
		case "recal":
			recalNext = true
			return true
		case "resume":
			logEvent("выход из сна — переподключаю устройства")
			time.Sleep(2 * time.Second) // дать звуковой службе проснуться
			return true
		case "autostart":
			if err := setAutostart(!autostartOn()); err != nil {
				ui.Balloon(appName, L("Автозапуск: ", "Autostart: ")+err.Error())
			} else if autostartOn() {
				ui.Balloon(appName, L("Запуск вместе с Windows включён", "Start with Windows is on"))
			} else {
				ui.Balloon(appName, L("Запуск вместе с Windows выключен", "Start with Windows is off"))
			}
		case "idlesleep":
			on := !idleSleepOn.Load()
			idleSleepOn.Store(on)
			updateConfig(func(c *appConfig) { c.NoIdleSleep = !on })
			if on {
				ui.Balloon(appName, fmt.Sprintf(L("ПК уснёт после %d мин тишины без мыши и клавиатуры", "The PC will sleep after %d min of silence and no input"), idleSleepMin()))
			} else {
				ui.Balloon(appName, L("Сон при тишине выключен", "Sleep on silence is off"))
			}
		case "log":
			openURL(dataFile("log.txt"))
		case "update":
			go func() {
				exe, err := installUpdate(dataDir())
				if err != nil {
					logEvent("обновление: %v", err)
					ui.Balloon(L("Обновление не удалось", "Update failed"), err.Error())
					return
				}
				logEvent("обновлено до %s, перезапуск", updateState().Latest)
				if err := restartAfterUpdate(exe); err != nil {
					ui.Balloon(L("Обновление не удалось", "Update failed"), err.Error())
					return
				}
				ui.send(action{Kind: "quit"})
			}()
		case "checkupd":
			go func() {
				tag, _, err := checkUpdate()
				switch {
				case err != nil:
					ui.Balloon(appName, L("Не удалось проверить обновления: ", "Update check failed: ")+err.Error())
				case tag != "":
					ui.Balloon(appName, fmt.Sprintf(L("Доступна версия %s", "Version %s is available"), tag))
				default:
					ui.Balloon(appName, fmt.Sprintf(L("Установлена последняя версия (%s)", "You have the latest version (%s)"), Version))
				}
			}()
		case "folder":
			openURL(dataDir())
		case "quit":
			*quit = true
			return true
		}
		return false
	}
	idleSleepOn.Store(!cfg.NoIdleSleep)
	if sup != nil {
		startIdleSleep(sup, time.Duration(idleSleepMin())*time.Minute, running.Load)
	}
	if !*noui {
		ui = startUI(func() uiState {
			st := uiState{Running: running.Load(),
				Note: func() string { v, _ := uiNote.Load().(string); return v }(), LatencyMs: func() int {
					if v := outLatMs.Load(); v > 0 {
						return int(v)
					}
					return int(outBufMs.Load()) + 16
				}(), MicPeriodMs: float64(micPeriodUs.Load()) / 1000, MicExWant: *micExcl, MicExOn: micExclActive.Load(),
				MicExWhy:   func() string { v, _ := micExclWhy.Load().(string); return v }(),
				OutStarves: int(outStarves.Load()), MicGlitches: int(micGlitches.Load()), MicOwnClock: micOwnClock.Load(), MicLossPct: float64(micLossPct.Load()) / 10, ExclWant: !*shared, ExclOn: exclActive.Load(), GainDB: amp.GainDB(), Limiting: amp.Limiting(), MicDB: -120, RefDB: -120, OutDB: -120,
				Noise: dn.Strength(), EQ: eq.Preset(), AGC: agc.Enabled(), AGCGain: agc.GainDB(),
				PTT: int(ptt.mode.Load()), PTTKey: keyName(int(ptt.key.Load())), PTTOpen: ptt.open.Load(), Binding: ptt.binding.Load(), Cue: cues.enabled.Load()}
			u := updateState()
			st.UpdLatest, st.UpdBusy, st.UpdMsg = u.Latest, u.Busy, u.Msg
			nameMu.RLock()
			st.Mic, st.Ref, st.Out = *micName, *refName, *outName
			nameMu.RUnlock()
			if sup != nil {
				st.Beta = sup.Beta()
				ss := sup.Stats()
				st.MicDB, st.RefDB, st.Reduction = ss.MicDB, ss.RefDB, ss.ReductionDB
				st.Voice, st.Calibrating, st.Found = ss.Voice, ss.Calibrating, ss.Found
			}
			if e := curEng.Load(); e != nil {
				st.OutDB = e.OutDB()
			}
			if !st.Running {
				st.OutDB, st.MicDB = -120, -120
			}
			return st
		}, !*background)
		go func() {
			time.Sleep(20 * time.Second)
			for {
				if tag, fresh, err := checkUpdate(); err == nil && fresh {
					logEvent("доступна версия %s", tag)
					ui.Balloon(appName, fmt.Sprintf(L("Доступна версия %s. Обновить можно на панели или в меню.", "Version %s is available. Update from the panel or the menu."), tag))
				}
				time.Sleep(time.Hour)
			}
		}()
	}

	session := func(first bool, doCal, forceCal bool) (quit bool, startErr error) {
		sessionsStarted.Add(1)
		t0 := nowQPC()
		micS := pipe.NewStreamIn(pipe.Rate*2, t0, 0)
		refS := pipe.NewStreamIn(pipe.Rate*2, t0, delaySamples)
		var eng *pipe.Engine
		if flt != nil {
			flt.Reset()
			eng = pipe.NewEngine(micS, refS, flt)
			eng.NoAuto = *noauto
		} else {
			eng = pipe.NewEngineS(micS, refS, sup, nrefs)
		}
		eng.Bypass = *bypass
		eng.Amp = amp
		eng.Dn, eng.EQ, eng.Agc = dn, eq, agc
		eng.Open = &ptt.open
		curEng.Store(eng)

		var stop atomic.Bool
		var wg sync.WaitGroup
		readyCh := make(chan ready, 3)
		errCh := make(chan error, 4)
		wg.Add(3)
		micHow := capLowLat
		if *micExcl {
			micHow = capExclLow
		}
		go runCapture("микрофон", *micName, wca.ECapture, false, modeMono, micS, &stop, readyCh, errCh, &wg, micHow)
		go runCapture("наушники (loopback)", *refName, wca.ERender, true, refMode, refS, &stop, readyCh, errCh, &wg, capNormal)
		go runRender(*outName, *shared, eng, t0, &stop, readyCh, errCh, &wg, micS, refS, *quiet)

		if first {
			fmt.Println("Устройства:")
		}
		var startedErr error
		for i := 0; i < 3; i++ {
			r := <-readyCh
			if r.err != nil {
				startedErr = r.err
				continue
			}
			if first {
				fmt.Printf("  • %s: %s [%v]\n", r.label, r.name, r.f)
			}
		}
		if startedErr != nil {
			stop.Store(true)
			wg.Wait()
			return false, startedErr
		}
		running.Store(true)
		defer running.Store(false)
		if first {
			if strings.Contains(strings.ToLower(*refName), "cable") {
				fmt.Println("ВНИМАНИЕ: в качестве «наушников» выбран кабель — так делать не нужно.")
			}
			fmt.Println("В нужной программе выбери микрофоном «CABLE Output».")
			if *bypass {
				fmt.Println("РЕЖИМ BYPASS: микрофон проходит без обработки.")
			}
			if *monoRef {
				fmt.Println("Опора: моно (один канал).")
			}
			fmt.Println()
		}
		if doCal && sup != nil && !*bypass {
			if err := calibrate(sup, *refName, forceCal, errCh); err != nil {
				stop.Store(true)
				wg.Wait()
				if first {
					fatal("%v", err)
				}
				ui.Balloon(appName, L("Калибровка не удалась: ", "Calibration failed: ")+firstLine(err.Error()))
				return false, nil
			}
			fmt.Println()
		}
		if first && hasConsole && !keysStarted && rawConsole() {
			keysStarted = true
			go gainKeys(amp)
			fmt.Printf("Усиление: + и − (сейчас %+.0f дБ).\n\n", amp.GainDB())
		}

		done := make(chan struct{})
		nameMu.RLock()
		refNow := *refName
		nameMu.RUnlock()
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
			defer ole.CoUninitialize()
			var defName string
			if refNow == "" {
				if d, n, err := pickDevice(wca.ERender, "", "наушники"); err == nil {
					d.Release()
					defName = n
				}
			}
			tk := time.NewTicker(2 * time.Second)
			defer tk.Stop()
			ticks := 0
			var lastEnd int64
			stall := 0
			lastWall := time.Now().Round(0)
			for {
				select {
				case <-done:
					return
				case <-tk.C:
				}
				ticks++

				if now := time.Now().Round(0); now.Sub(lastWall) > 8*time.Second {
					logEvent("компьютер вышел из сна (пауза %.0f с) — переподключаю устройства", now.Sub(lastWall).Seconds())
					select {
					case errCh <- fmt.Errorf("компьютер вышел из сна"):
					default:
					}
					return
				} else {
					lastWall = now
				}

				if e, ok := micS.End(); ok {
					if e == lastEnd {
						stall++
					} else {
						stall, lastEnd = 0, e
					}
					if stall >= 5 {
						select {
						case errCh <- fmt.Errorf("микрофон перестал присылать звук"):
						default:
						}
						return
					}
				}
				if defName != "" {
					if d, n, err := pickDevice(wca.ERender, "", "наушники"); err == nil {
						d.Release()
						if n != defName && !strings.Contains(strings.ToLower(n), "cable") {
							select {
							case errCh <- fmt.Errorf("устройство вывода по умолчанию сменилось: «%s» → «%s»", defName, n):
							default:
							}
							return
						}
					}
				}
				if sup != nil && ticks%45 == 0 && sup.Adapted() {
					if c := sup.Calib(); c.Found {
						if b, err := json.MarshalIndent(c, "", " "); err == nil {
							os.WriteFile(calibPath(), b, 0o644)
						}
					}
				}
			}
		}()

	wait:
		for {
			var acts chan action
			if ui != nil {
				acts = ui.acts
			}
			select {
			case <-sig:
				quit = true
				break wait
			case err := <-errCh:
				fmt.Printf("\n[%s] %v\n", time.Now().Format("15:04:05"), err)
				logEvent("перезапуск цепочки: %v", err)
				if errors.Is(err, errMicDrops) {
					ui.Balloon(appName, firstLine(err.Error()))
				} else {
					ui.Balloon(appName, L("Устройство отключилось, переподключаюсь", "A device was lost, reconnecting"))
				}
				break wait
			case a := <-acts:
				if handleAction(a, &quit) {
					break wait
				}
			}
		}
		close(done)
		stop.Store(true)
		wg.Wait()
		return quit, nil
	}

	if !waitForCable(*outName, sig, ui) {
		if ui != nil {
			ui.Close()
		}
		return
	}

	first := true
	for {
		doCal, force := first && sup != nil, first && *recal
		if recalNext {
			doCal, force, recalNext = true, true, false
		}
		quit, err := session(first, doCal, force)
		if err != nil {
			if first {
				fatal("%v", err)
			}
			fmt.Printf("[%s] жду устройства: %v\n", time.Now().Format("15:04:05"), firstLine(err.Error()))
			select {
			case <-sig:
				quit = true
			case <-time.After(3 * time.Second):
			}
		} else if !quit {
			fmt.Println("Переподключаюсь...")
			select {
			case <-sig:
				quit = true
			case <-time.After(1500 * time.Millisecond):
			}
		}
		first = false
		if quit {
			break
		}
	}
	if ui != nil {
		ui.Close()
	}
	restoreConsole()
	fmt.Println("\nОстановлено.")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func calibPath() string { return dataFile("calib.json") }

// calibrate загружает калибровку из файла или проводит новую.

func calibrate(sup *aec.Suppressor, refName string, force bool, errCh chan error) error {
	path := calibPath()
	if !force {
		if b, err := os.ReadFile(path); err == nil {
			var c aec.Calib

			if json.Unmarshal(b, &c) == nil && c.Found && sup.SetCalib(c) {
				fmt.Printf("Калибровка загружена: наводка с задержкой %.0f мс\n",
					float64(c.D*sup.H)*1000/float64(sup.Rate))
			}
		}
		return nil // без файла модель выучится сама по звуку наушников
	}
	dev, name, err := pickDevice(wca.ERender, refName, L("наушники", "headphones"))
	if err != nil {
		return err
	}
	defer dev.Release()
	fmt.Printf("КАЛИБРОВКА: молчи ~7 секунд, в наушниках будет шипение.\n")
	theUI.Balloon(L("Калибровка", "Calibration"), L("Помолчи 7 секунд: в наушниках будет шипение.", "Stay quiet for 7 seconds: you will hear noise in the headphones."))
	time.Sleep(1200 * time.Millisecond) // дать потокам запуститься
	frames := (diag.PeriodMs*diag.Periods + 1500) * sup.Rate / 1000 / sup.H
	sup.StartCalib(frames)
	time.Sleep(200 * time.Millisecond)
	if _, err := playTone(dev); err != nil {
		return fmt.Errorf(L("не удалось проиграть шипение в «%s»: %v", "could not play the test noise on \"%s\": %v"), name, err)
	}
	var c aec.Calib
	select {
	case c = <-sup.CalDone():
	case err := <-errCh:
		return err
	case <-time.After(15 * time.Second):
		return errors.New(L("калибровка не завершилась: нет звука с микрофона или наушников", "calibration did not finish: no audio from the mic or headphones"))
	}
	if c.Found {
		msg := fmt.Sprintf(L("Наводка найдена: задержка %.0f мс, на %.0f дБ громче шума.", "Bleed found: %.0f ms delay, %.0f dB above the noise."),
			float64(c.D*sup.H)*1000/float64(sup.Rate), c.LeakDB)
		if c.LeakDB < 6 {
			msg += L(" Слабо: перекалибруй при обычной громкости наушников.", " Weak: recalibrate at your usual headphone volume.")
		}
		fmt.Println(msg)
		theUI.Balloon(L("Калибровка готова", "Calibration done"), msg)
	} else {
		// не затираем прошлую рабочую калибровку неудачной
		if b, err := os.ReadFile(path); err == nil {
			var old aec.Calib
			if json.Unmarshal(b, &old) == nil && old.Found && sup.SetCalib(old) {
				msg := fmt.Sprintf(L("Наводка не найдена (шипение играло в «%s»). Оставлена прошлая калибровка.",
					"No bleed detected (noise played on \"%s\"). The previous calibration is kept."), name)
				fmt.Println(msg)
				theUI.Balloon(L("Калибровка не удалась", "Calibration failed"), msg)
				return nil
			}
		}
		msg := fmt.Sprintf(L("Наводка не найдена (шипение играло в «%s»). Если собеседники слышат себя, поставь обычную громкость и перекалибруй.",
			"No bleed detected (noise played on \"%s\"). If others hear themselves, set your usual volume and recalibrate."), name)
		fmt.Println(msg)
		theUI.Balloon(L("Наводка не найдена", "No bleed detected"), msg)
	}
	if !c.Found {
		return nil
	}
	if b, err := json.MarshalIndent(c, "", " "); err == nil {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			fmt.Println("(не смог сохранить калибровку:", err, ")")
		}
	}
	return nil
}

var (
	pMessageBoxW  = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")
	pShellExecute = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
)

const vbCableURL = "https://vb-audio.com/Cable/"

func openURL(u string) {
	pShellExecute.Call(0, uintptr(unsafe.Pointer(u16("open"))), uintptr(unsafe.Pointer(u16(u))), 0, 0, 1)
}

// waitForCable проверяет, что виртуальный кабель установлен.

func waitForCable(out string, sig chan os.Signal, ui *uiCtl) bool {
	has := func() bool {
		l, err := deviceNames(wca.ERender)
		if err != nil {
			return true
		}
		for _, n := range l {
			if strings.Contains(strings.ToLower(n), strings.ToLower(out)) {
				return true
			}
		}
		return false
	}
	if has() {
		return true
	}
	uiNote.Store(L("жду VB-Cable", "waiting for VB-Cable"))
	defer uiNote.Store("")
	fmt.Printf("Не найдено устройство «%s» — виртуальный кабель не установлен.\n", out)
	fmt.Println("Нужен бесплатный драйвер VB-Cable: " + vbCableURL)
	fmt.Println("Установи его (VBCABLE_Setup_x64.exe → Install Driver), иногда нужна перезагрузка. Жду...")
	if strings.EqualFold(out, "CABLE Input") {
		msg := L("Не найден виртуальный кабель VB-Cable.\n\n"+
			"Через него другие программы получают очищенный микрофон. Это бесплатный драйвер: "+
			"запусти VBCABLE_Setup_x64.exe от имени администратора и нажми Install Driver. "+
			"Иногда после установки нужна перезагрузка.\n\nОткрыть страницу загрузки?",
			"VB-Cable virtual cable not found.\n\n"+
				"Other programs receive the cleaned mic through it. It is a free driver: "+
				"run VBCABLE_Setup_x64.exe as administrator and click Install Driver. "+
				"A reboot may be required.\n\nOpen the download page?")
		if messageBox(msg, 0x4|0x40) == 6 { // IDYES
			openURL(vbCableURL)
		}
	}
	var acts chan action
	if ui != nil {
		acts = ui.acts
	}
	for {
		select {
		case <-sig:
			return false
		case a := <-acts:
			if a.Kind == "quit" {
				return false
			}
		case <-time.After(3 * time.Second):
		}
		if has() {
			fmt.Println("Кабель найден, запускаюсь.")
			return true
		}
	}
}

// ditherQuiet подмешивает в тишину шум около −90 дБ, чтобы Discord не считал микрофон неисправным.

var ditherState uint32 = 2463534242

func ditherQuiet(buf []float32) {
	const lsb = 1.0 / 32767
	for _, v := range buf {
		if v > 4*lsb || v < -4*lsb {
			return // идёт настоящий звук — не трогаем
		}
	}
	x := ditherState
	for i := range buf {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		a := float32(x&0xFFFF) / 65535
		b := float32(x>>16) / 65535
		buf[i] += (a + b - 1) * lsb // треугольный шум −1..+1 бит
	}
	ditherState = x
}
