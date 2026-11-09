package audio

import (
	"encoding/binary"
	"io"
	"math"
)

// WriteWAV24 пишет многоканальный 24-битный PCM WAV (WAVE_FORMAT_EXTENSIBLE).

func WriteWAV24(w io.Writer, rate int, chans [][]float32) error {
	nch := len(chans)
	n := 0
	if nch > 0 {
		n = len(chans[0])
	}
	dataLen := n * nch * 3
	hdr := make([]byte, 0, 68)
	le := binary.LittleEndian
	u16 := func(v uint16) { hdr = le.AppendUint16(hdr, v) }
	u32 := func(v uint32) { hdr = le.AppendUint32(hdr, v) }
	hdr = append(hdr, "RIFF"...)
	u32(uint32(4 + 8 + 40 + 8 + dataLen))
	hdr = append(hdr, "WAVEfmt "...)
	u32(40)
	u16(0xFFFE)
	u16(uint16(nch))
	u32(uint32(rate))
	u32(uint32(rate * nch * 3))
	u16(uint16(nch * 3))
	u16(24)
	u16(22)
	u16(24)
	u32(0)

	hdr = append(hdr, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71)
	hdr = append(hdr, "data"...)
	u32(uint32(dataLen))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	const chunk = 4096
	buf := make([]byte, 0, chunk*nch*3)
	for i := 0; i < n; i += chunk {
		buf = buf[:0]
		end := i + chunk
		if end > n {
			end = n
		}
		for j := i; j < end; j++ {
			for c := 0; c < nch; c++ {
				v := float64(chans[c][j])
				if v > 1 {
					v = 1
				} else if v < -1 {
					v = -1
				}
				x := int32(math.Round(v * 8388607))
				buf = append(buf, byte(x), byte(x>>8), byte(x>>16))
			}
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}
