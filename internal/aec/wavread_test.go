package aec

import (
	"encoding/binary"
	"os"
	"testing"
)

func readWAV24(t *testing.T, p string) (mic []float64, refs [][]float64, rate int) {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var nch, bits int
	pos := 12
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		sz := int(binary.LittleEndian.Uint32(b[pos+4:]))
		body := b[pos+8:]
		if id == "fmt " {
			nch = int(binary.LittleEndian.Uint16(body[2:]))
			rate = int(binary.LittleEndian.Uint32(body[4:]))
			bits = int(binary.LittleEndian.Uint16(body[14:]))
		}
		if id == "data" {
			if bits != 24 {
				t.Fatal("нужен 24 бит")
			}
			n := sz / (3 * nch)
			ch := make([][]float64, nch)
			for c := range ch {
				ch[c] = make([]float64, n)
			}
			for i := 0; i < n; i++ {
				for c := 0; c < nch; c++ {
					o := (i*nch + c) * 3
					v := int32(uint32(body[o])<<8|uint32(body[o+1])<<16|uint32(body[o+2])<<24) >> 8
					ch[c][i] = float64(v) / 8388608
				}
			}
			return ch[0], ch[1:], rate
		}
		pos += 8 + sz
	}
	t.Fatal("нет data")
	return
}

func writeWAVmono(t *testing.T, p string, x []float64, rate int) {
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	le := binary.LittleEndian
	h := make([]byte, 44)
	copy(h, "RIFF")
	le.PutUint32(h[4:], uint32(36+len(x)*2))
	copy(h[8:], "WAVEfmt ")
	le.PutUint32(h[16:], 16)
	le.PutUint16(h[20:], 1)
	le.PutUint16(h[22:], 1)
	le.PutUint32(h[24:], uint32(rate))
	le.PutUint32(h[28:], uint32(rate*2))
	le.PutUint16(h[32:], 2)
	le.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	le.PutUint32(h[40:], uint32(len(x)*2))
	f.Write(h)
	buf := make([]byte, 2*len(x))
	for i, v := range x {
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		le.PutUint16(buf[2*i:], uint16(int16(v*32767)))
	}
	f.Write(buf)
}
