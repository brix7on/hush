//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"

	"github.com/brix7on/hush/internal/diag"
)

var (
	procOpenProcess    = k32.NewProc("OpenProcess")
	procQueryImageName = k32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle    = k32.NewProc("CloseHandle")
)

func exeName(pid uint32) string {
	if pid == 0 {
		return L("Системные звуки", "System sounds")
	}
	h, _, _ := procOpenProcess.Call(0x1000, 0, uintptr(pid)) // PROCESS_QUERY_LIMITED_INFORMATION
	if h == 0 {
		return fmt.Sprintf(L("процесс %d", "process %d"), pid)
	}
	defer procCloseHandle.Call(h)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	r, _, _ := procQueryImageName.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return fmt.Sprintf(L("процесс %d", "process %d"), pid)
	}
	return filepath.Base(syscall.UTF16ToString(buf[:n]))
}

type sessInfo struct {
	exe    string
	active bool
}

func deviceSessions(dev *wca.IMMDevice) []sessInfo {
	var mgr *wca.IAudioSessionManager2
	if err := dev.Activate(wca.IID_IAudioSessionManager2, wca.CLSCTX_ALL, nil, &mgr); err != nil {
		return nil
	}
	defer mgr.Release()
	var en *wca.IAudioSessionEnumerator
	if err := mgr.GetSessionEnumerator(&en); err != nil {
		return nil
	}
	defer en.Release()
	var n int
	if err := en.GetCount(&n); err != nil {
		return nil
	}
	var out []sessInfo
	for i := 0; i < n; i++ {
		var sc *wca.IAudioSessionControl
		if err := en.GetSession(i, &sc); err != nil || sc == nil {
			continue
		}
		var st uint32
		sc.GetState(&st)
		if st == 2 { // истёкшая сессия
			sc.Release()
			continue
		}
		d, err := sc.QueryInterface(wca.IID_IAudioSessionControl2)
		if err == nil && d != nil {
			sc2 := (*wca.IAudioSessionControl2)(unsafe.Pointer(d))
			var pid uint32
			sc2.GetProcessId(&pid)
			sc2.Release()
			out = append(out, sessInfo{exe: exeName(pid), active: st == 1})
		}
		sc.Release()
	}
	return out
}

func deviceID(d *wca.IMMDevice) string {
	var s string
	d.GetId(&s)
	return s
}

type endpoint struct {
	dev              *wca.IMMDevice
	id, name         string
	defCons, defComm bool
	sess             []sessInfo
}

func (e endpoint) has(sub string) bool {
	for _, s := range e.sess {
		if strings.Contains(strings.ToLower(s.exe), sub) {
			return true
		}
	}
	return false
}

func (e endpoint) apps() string {
	seen := map[string]bool{}
	var l []string
	for _, s := range e.sess {
		if !seen[s.exe] {
			seen[s.exe] = true
			l = append(l, s.exe)
		}
	}
	return strings.Join(l, ", ")
}

func endpoints(flow uint32) []endpoint {
	list, err := listDevices(flow)
	if err != nil {
		return nil
	}
	en, err := newEnumerator()
	if err != nil {
		return nil
	}
	defer en.Release()
	defID := func(role uint32) string {
		var d *wca.IMMDevice
		if en.GetDefaultAudioEndpoint(flow, role, &d) != nil || d == nil {
			return ""
		}
		defer d.Release()
		return deviceID(d)
	}
	dc, dm := defID(wca.EConsole), defID(wca.ECommunications)
	var out []endpoint
	for _, d := range list {
		id := deviceID(d.dev)
		out = append(out, endpoint{dev: d.dev, id: id, name: d.name, defCons: id == dc, defComm: id == dm, sess: deviceSessions(d.dev)})
	}
	return out
}

type capRes struct {
	data  []float32
	rate  int
	start time.Time
	err   error
}

func captureMono(dev *wca.IMMDevice, stop *atomic.Bool, res *capRes, started *sync.WaitGroup, done *sync.WaitGroup) {
	defer done.Done()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()
	once := sync.Once{}
	ready := func() { once.Do(started.Done) }
	defer ready()
	ac, wfx, f, err := openClient(dev)
	if err != nil {
		res.err = err
		return
	}
	if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, 0, wca.REFERENCE_TIME(1000000), 0, wfx, nil); err != nil {
		res.err = fmt.Errorf("Initialize: %v", err)
		return
	}
	var acc *wca.IAudioCaptureClient
	if err := ac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
		res.err = err
		return
	}
	if err := ac.Start(); err != nil {
		res.err = err
		return
	}
	defer ac.Stop()
	res.rate = f.Rate
	res.start = time.Now()
	ready()
	for !stop.Load() {
		time.Sleep(3 * time.Millisecond)
		for {
			var n uint32
			if acc.GetNextPacketSize(&n) != nil || n == 0 {
				break
			}
			var data *byte
			var frames, fl uint32
			var p1, p2 uint64
			if acc.GetBuffer(&data, &frames, &fl, &p1, &p2) != nil {
				break
			}
			if fl&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 || data == nil {
				res.data = append(res.data, make([]float32, frames)...)
			} else {
				res.data = f.DecodeMono(unsafe.Slice(data, int(frames)*f.BlockAlign), int(frames), res.data)
			}
			acc.ReleaseBuffer(frames)
		}
	}
}

// playTone играет тестовое шипение на устройстве и возвращает момент начала.
func playTone(dev *wca.IMMDevice) (time.Time, error) {
	return playSamples(dev, func(rate int) []float32 { return diag.Tone(rate, 0.1) })
}

// playSamples проигрывает звук (моно), сгенерированный под частоту устройства.
func playSamples(dev *wca.IMMDevice, toneGen func(rate int) []float32) (time.Time, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ac, wfx, f, err := openClient(dev)
	if err != nil {
		return time.Time{}, err
	}
	defer ac.Release()
	if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, 0, wca.REFERENCE_TIME(2000000), 0, wfx, nil); err != nil {
		return time.Time{}, fmt.Errorf("Initialize: %v", err)
	}
	var bufFrames uint32
	ac.GetBufferSize(&bufFrames)
	var arc *wca.IAudioRenderClient
	if err := ac.GetService(wca.IID_IAudioRenderClient, &arc); err != nil {
		return time.Time{}, err
	}
	defer arc.Release()
	tone := toneGen(f.Rate)
	pos := 0
	write := func() error {
		var pad uint32
		if err := ac.GetCurrentPadding(&pad); err != nil {
			return err
		}
		n := int(bufFrames - pad)
		if n > len(tone)-pos {
			n = len(tone) - pos
		}
		if n <= 0 {
			return nil
		}
		var dst *byte
		if err := arc.GetBuffer(uint32(n), &dst); err != nil {
			return err
		}
		f.EncodeMono(tone[pos:pos+n], unsafe.Slice(dst, n*f.BlockAlign))
		arc.ReleaseBuffer(uint32(n), 0)
		pos += n
		return nil
	}
	if err := write(); err != nil {
		return time.Time{}, err
	}
	if err := ac.Start(); err != nil {
		return time.Time{}, err
	}
	t0 := time.Now()
	for pos < len(tone) {
		time.Sleep(5 * time.Millisecond)
		if err := write(); err != nil {
			ac.Stop()
			return t0, err
		}
	}
	time.Sleep(time.Duration(bufFrames)*time.Second/time.Duration(f.Rate) + 100*time.Millisecond)
	ac.Stop()
	return t0, nil
}

func short(name string) string { return "«" + name + "»" }

func doctorMode() {
	fmt.Println(L("Диагностика эха. Зайди в голосовой канал Дискорда (можно одному) и не закрывай его.", "Echo diagnostics. Join a Discord voice channel (alone is fine) and keep it open."))
	fmt.Println(L("Через пару секунд в наушниках будет короткое шипение (~5 с) — это нормально, молчи.", "In a few seconds you will hear a short noise (~5 s) in the headphones. Stay quiet."))
	fmt.Println()
	time.Sleep(1500 * time.Millisecond)

	renders := endpoints(wca.ERender)
	captures := endpoints(wca.ECapture)
	if len(renders) == 0 || len(captures) == 0 {
		fatal("%s", L("не удалось получить список звуковых устройств", "cannot list audio devices"))
	}

	var discOut, discIn []endpoint
	for _, e := range renders {
		if e.has("discord") {
			discOut = append(discOut, e)
		}
	}
	for _, e := range captures {
		if e.has("discord") {
			discIn = append(discIn, e)
		}
	}
	fmt.Println(L("Устройства вывода:", "Output devices:"))
	for _, e := range renders {
		mark := ""
		if e.defCons {
			mark += L(" [по умолчанию]", " [default]")
		}
		if e.defComm {
			mark += L(" [связь по умолчанию]", " [default communications]")
		}
		apps := e.apps()
		if apps == "" {
			apps = "—"
		}
		fmt.Printf(L("  • %s%s — играют: %s\n", "  • %s%s — playing: %s\n"), e.name, mark, apps)
	}
	fmt.Println(L("Устройства записи:", "Recording devices:"))
	for _, e := range captures {
		mark := ""
		if e.defCons {
			mark += L(" [по умолчанию]", " [default]")
		}
		if e.defComm {
			mark += L(" [связь по умолчанию]", " [default communications]")
		}
		apps := e.apps()
		if apps == "" {
			apps = "—"
		}
		fmt.Printf(L("  • %s%s — записывают: %s\n", "  • %s%s — recording: %s\n"), e.name, mark, apps)
	}
	fmt.Println()

	// куда играть шипение — туда же, куда выводит Дискорд
	var target endpoint
	if len(discOut) > 0 {
		target = discOut[0]
	} else {
		for _, e := range renders {
			if e.defCons {
				target = e
			}
		}
		if target.dev == nil {
			target = renders[0]
		}
		fmt.Println(L("Дискорд не найден среди воспроизводящих программ — шипение пойдёт на устройство по умолчанию.", "Discord is not playing audio, the noise goes to the default device."))
	}
	fmt.Printf(L("Проигрываю шипение в %s и слушаю все входы...\n", "Playing noise on %s and listening to all inputs...\n"), short(target.name))

	var stop atomic.Bool
	var started, done sync.WaitGroup
	res := make([]capRes, len(captures))
	for i := range captures {
		started.Add(1)
		done.Add(1)
		go captureMono(captures[i].dev, &stop, &res[i], &started, &done)
	}
	started.Wait()
	time.Sleep(500 * time.Millisecond)
	toneStart, err := playTone(target.dev)
	time.Sleep(900 * time.Millisecond)
	stop.Store(true)
	done.Wait()
	if err != nil {
		fatal(L("не удалось проиграть шипение: %v", "cannot play the noise: %v"), err)
	}

	fmt.Println()
	fmt.Println(L("Результат:", "Result:"))
	var heard []int
	for i, e := range captures {
		r := res[i]
		if r.err != nil {
			fmt.Printf(L("  • %s: не удалось открыть (%v)\n", "  • %s: cannot open (%v)\n"), e.name, r.err)
			continue
		}
		at := int(toneStart.Sub(r.start) / time.Millisecond)
		d := diag.Detect(r.data, r.rate, at)
		if d.Hears {
			heard = append(heard, i)
			fmt.Printf(L("  • %s: СЛЫШИТ шипение (+%.0f дБ, задержка %d мс)\n", "  • %s: HEARS the noise (+%.0f dB, delay %d ms)\n"), e.name, d.DiffDB, d.LagMs)
		} else {
			fmt.Printf(L("  • %s: не слышит\n", "  • %s: does not hear it\n"), e.name)
		}
	}
	fmt.Println()

	// ищем настоящий микрофон, который не слышит шипение
	realMic := ""
	for i, e := range captures {
		ln := strings.ToLower(e.name)
		h := false
		for _, j := range heard {
			if j == i {
				h = true
			}
		}
		if !h && res[i].err == nil && !strings.Contains(ln, "cable") && !strings.Contains(ln, "микшер") && !strings.Contains(ln, "stereo mix") {
			if realMic == "" {
				realMic = e.name
			}
		}
	}

	fmt.Println(L("════════ ВЫВОД ════════", "════════ VERDICT ════════"))
	if len(discIn) == 0 {
		fmt.Println(L("Не вижу, с какого устройства записывает Дискорд — ты точно в голосовом канале?", "Cannot see which device Discord records from. Are you in a voice channel?"))
	} else {
		var names []string
		for _, e := range discIn {
			names = append(names, short(e.name))
		}
		fmt.Println(L("Дискорд записывает с:", "Discord records from:"), strings.Join(names, ", "))
	}
	found := false
	for _, i := range heard {
		e := captures[i]
		if !e.has("discord") {
			continue
		}
		found = true
		ln := strings.ToLower(e.name)
		fmt.Printf(L("\nПРИЧИНА: Дискорд берёт звук с %s, а туда попадает всё, что играет в %s.\n", "\nCAUSE: Discord records from %s, and everything played on %s ends up there.\n"), short(e.name), short(target.name))
		fmt.Println(L("Поэтому голос друга возвращается к нему.", "That is why others hear themselves."))
		switch {
		case strings.Contains(ln, "cable"):
			var writers []string
			for _, r := range renders {
				if strings.Contains(strings.ToLower(r.name), "cable") {
					for _, s := range r.sess {
						if !strings.Contains(strings.ToLower(s.exe), appName) {
							writers = append(writers, s.exe)
						}
					}
				}
			}
			if len(writers) > 0 {
				fmt.Printf(L("В виртуальный кабель выводят звук: %s.\n", "Programs playing into the virtual cable: %s.\n"), strings.Join(writers, ", "))
				fmt.Println(L("ИСПРАВИТЬ: Параметры Windows → Система → Звук → Микшер громкости → у этих программ", "FIX: Windows Settings → System → Sound → Volume mixer → for these programs"))
				fmt.Println(L("           поставь «Устройство вывода» = «По умолчанию».", "     set \"Output device\" to \"Default\"."))
			}
			if realMic != "" {
				fmt.Printf(L("ИЛИ проще: в Дискорде выбери «Устройство ввода» = %s.\n", "OR simpler: in Discord set \"Input device\" to %s.\n"), short(realMic))
			}
		case strings.Contains(ln, "микшер") || strings.Contains(ln, "stereo mix") || strings.Contains(ln, "what u hear"):
			fmt.Println(L("Это «Стерео микшер» — он записывает всё, что играет на компьютере.", "This is \"Stereo Mix\": it records everything played on the PC."))
			if realMic != "" {
				fmt.Printf(L("ИСПРАВИТЬ: в Дискорде выбери «Устройство ввода» = %s.\n", "FIX: in Discord set \"Input device\" to %s.\n"), short(realMic))
			}
		default:
			fmt.Println(L("Это сам микрофон: Windows или драйвер подмешивают в него звук наушников.", "It is the mic itself: Windows or the driver mixes headphone audio into it."))
			fmt.Println(L("ИСПРАВИТЬ: Панель управления → Звук → Запись → этот микрофон → Свойства:", "FIX: Control Panel → Sound → Recording → this mic → Properties:"))
			fmt.Println(L("  • вкладка «Прослушать» — сними «Прослушивать с данного устройства»;", "  • Listen tab: uncheck \"Listen to this device\";"))
			fmt.Println(L("  • вкладка «Улучшения»/«Дополнительно» — отключи все эффекты;", "  • Enhancements/Advanced tab: disable all effects;"))
			fmt.Println(L("  • в программе гарнитуры (если стоит) — выключи «Мониторинг микрофона»/«Sidetone».", "  • in the headset software, if any: turn off \"Mic monitoring\"/\"Sidetone\"."))
		}
	}
	if !found {
		if len(heard) > 0 {
			fmt.Println(L("\nШипение слышат другие входы, но не тот, с которого пишет Дискорд.", "\nOther inputs hear the noise, but not the one Discord records from."))
		}
		fmt.Println(L("\nWindows эхо не создаёт: звук, который играет Дискорд, не возвращается в его вход.", "\nWindows does not create the echo: what Discord plays does not return to its input."))
		fmt.Println(L("Пришли мне скрин этого окна целиком — разберу, что осталось.", "If the echo persists, attach a screenshot of this window to an issue on GitHub."))
	}
	fmt.Println(L("\nНажми Enter, чтобы закрыть окно...", "\nPress Enter to close..."))
	waitEnter()
}
