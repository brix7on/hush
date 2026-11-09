package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- самообновление из релизов GitHub ----------

const selfRepo = "brix7on/hush"

type updateInfo struct {
	Latest string
	Busy   bool
	Msg    string
}

var upd struct {
	sync.Mutex
	st      updateInfo
	exeURL  string
	sumsURL string
	told    string
}

func updateState() updateInfo {
	upd.Lock()
	defer upd.Unlock()
	return upd.st
}

func setUpd(f func(*updateInfo)) {
	upd.Lock()
	f(&upd.st)
	upd.Unlock()
}

// newerVersion: a новее b, сравнение чисел через точку.
func newerVersion(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

var httpc = &http.Client{Timeout: 5 * time.Minute}

func httpGet(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", appName+"/"+Version)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// checkUpdate спрашивает GitHub о последнем релизе; true — найдена новая версия, о которой ещё не сообщали.
func checkUpdate() (string, bool, error) {
	if Version == "dev" {
		return "", false, nil
	}
	resp, err := httpGet("https://api.github.com/repos/" + selfRepo + "/releases/latest")
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", false, err
	}
	if !newerVersion(rel.Tag, Version) {
		setUpd(func(s *updateInfo) { s.Latest = "" })
		return "", false, nil
	}
	var exeURL, sums string
	for _, a := range rel.Assets {
		switch a.Name {
		case "hush.exe":
			exeURL = a.URL
		case "SHA256SUMS":
			sums = a.URL
		}
	}
	if exeURL == "" || sums == "" {
		return "", false, nil
	}
	tag := strings.TrimPrefix(rel.Tag, "v")
	upd.Lock()
	defer upd.Unlock()
	upd.st.Latest, upd.exeURL, upd.sumsURL = tag, exeURL, sums
	fresh := upd.told != tag
	upd.told = tag
	return tag, fresh, nil
}

func download(url, dst string, progress func(done, total int64)) error {
	resp, err := httpGet(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	var done int64
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			done += int64(n)
			if progress != nil {
				progress(done, resp.ContentLength)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	return f.Close()
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sumFor(sumsPath, name string) string {
	f, err := os.Open(sumsPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) == 2 && strings.TrimPrefix(fs[1], "*") == name {
			return strings.ToLower(fs[0])
		}
	}
	return ""
}

// installUpdate скачивает новый exe, сверяет сумму и подменяет текущий файл.
func installUpdate(work string) (exe string, err error) {
	upd.Lock()
	if upd.st.Busy || upd.st.Latest == "" {
		upd.Unlock()
		return "", errors.New(L("обновлений нет", "no updates"))
	}
	upd.st.Busy, upd.st.Msg = true, L("загрузка", "downloading")
	exeURL, sumsURL := upd.exeURL, upd.sumsURL
	upd.Unlock()
	defer setUpd(func(s *updateInfo) { s.Busy = false })

	if exe, err = os.Executable(); err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	dir := filepath.Join(work, "update")
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	np, sp := filepath.Join(dir, "hush.exe"), filepath.Join(dir, "SHA256SUMS")
	if err := download(sumsURL, sp, nil); err != nil {
		return "", err
	}
	if err := download(exeURL, np, func(d, t int64) {
		if t > 0 {
			setUpd(func(s *updateInfo) { s.Msg = fmt.Sprintf(L("загрузка %d%%", "downloading %d%%"), d*100/t) })
		}
	}); err != nil {
		return "", err
	}
	want := sumFor(sp, "hush.exe")
	got, err := fileSHA256(np)
	if err != nil {
		return "", err
	}
	if want == "" || want != got {
		return "", errors.New(L("контрольная сумма не совпала", "checksum mismatch"))
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return "", fmt.Errorf(L("не удалось заменить файл: %v", "cannot replace the file: %v"), err)
	}
	if err := copyFile(np, exe); err != nil {
		os.Rename(old, exe)
		return "", fmt.Errorf(L("не удалось заменить файл: %v", "cannot replace the file: %v"), err)
	}
	setUpd(func(s *updateInfo) { s.Msg = L("перезапуск", "restarting") })
	return exe, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// cleanupOldExe удаляет exe, оставшийся от прошлого обновления.
func cleanupOldExe() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	for i := 0; i < 20; i++ {
		if err := os.Remove(exe + ".old"); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(time.Second)
	}
}
