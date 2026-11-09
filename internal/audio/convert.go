// Package audio — разбор форматов WASAPI, преобразование в моно float32 и обратно.
package audio

import (
	"encoding/binary"
	"fmt"
	"math"
)

type Format struct {
	Rate       int
	Channels   int
	Bits       int
	Float      bool
	BlockAlign int
}

func (f Format) String() string {
	k := "int"
	if f.Float {
		k = "float"
	}
	return fmt.Sprintf("%d Гц, %d кан., %d бит %s", f.Rate, f.Channels, f.Bits, k)
}

// ParseWaveFormat разбирает сырые байты WAVEFORMATEX (или WAVEFORMATEXTENSIBLE).
func ParseWaveFormat(b []byte) (Format, error) {
	if len(b) < 16 {
		return Format{}, fmt.Errorf("слишком короткий WAVEFORMATEX")
	}
	tag := binary.LittleEndian.Uint16(b[0:])
	f := Format{
		Channels:   int(binary.LittleEndian.Uint16(b[2:])),
		Rate:       int(binary.LittleEndian.Uint32(b[4:])),
		BlockAlign: int(binary.LittleEndian.Uint16(b[12:])),
		Bits:       int(binary.LittleEndian.Uint16(b[14:])),
	}
	switch tag {
	case 1:
	case 3:
		f.Float = true
	case 0xFFFE:
		if len(b) < 40 {
			return f, fmt.Errorf("обрезанный WAVEFORMATEXTENSIBLE")
		}
		switch binary.LittleEndian.Uint32(b[24:]) {
		case 1:
		case 3:
			f.Float = true
		default:
			return f, fmt.Errorf("неизвестный SubFormat")
		}
	default:
		return f, fmt.Errorf("неподдерживаемый формат (tag=0x%X)", tag)
	}
	if f.Channels < 1 || f.Rate < 8000 || f.BlockAlign < 1 {
		return f, fmt.Errorf("странный формат: %v", f)
	}
	if f.Float && f.Bits != 32 {
		return f, fmt.Errorf("поддерживается только float32")
	}
	if !f.Float && f.Bits != 16 && f.Bits != 24 && f.Bits != 32 {
		return f, fmt.Errorf("неподдерживаемая разрядность %d", f.Bits)
	}
	return f, nil
}

func (f Format) sample(b []byte) float32 {
	switch {
	case f.Float:
		return math.Float32frombits(binary.LittleEndian.Uint32(b))
	case f.Bits == 16:
		return float32(int16(binary.LittleEndian.Uint16(b))) / 32768
	case f.Bits == 24:
		v := int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24) >> 8
		return float32(v) / 8388608
	default:
		return float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648
	}
}

// DecodeMono дописывает в dst моно-версию (среднее по каналам) frames кадров из data.
func (f Format) DecodeMono(data []byte, frames int, dst []float32) []float32 {
	bps := f.Bits / 8
	for i := 0; i < frames; i++ {
		base := i * f.BlockAlign
		var s float32
		for c := 0; c < f.Channels; c++ {
			s += f.sample(data[base+c*bps:])
		}
		dst = append(dst, s/float32(f.Channels))
	}
	return dst
}

func (f Format) put(b []byte, v float32) {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	switch {
	case f.Float:
		binary.LittleEndian.PutUint32(b, math.Float32bits(v))
	case f.Bits == 16:
		binary.LittleEndian.PutUint16(b, uint16(int16(math.Round(float64(v)*32767))))
	case f.Bits == 24:
		x := int32(math.Round(float64(v) * 8388607))
		b[0], b[1], b[2] = byte(x), byte(x>>8), byte(x>>16)
	default:
		binary.LittleEndian.PutUint32(b, uint32(int32(math.Round(float64(v)*2147483647))))
	}
}

// EncodeMono пишет моно-сигнал во все каналы формата f; dst должен вмещать len(mono)*BlockAlign байт.
func (f Format) EncodeMono(mono []float32, dst []byte) {
	bps := f.Bits / 8
	for i, v := range mono {
		base := i * f.BlockAlign
		for c := 0; c < f.Channels; c++ {
			f.put(dst[base+c*bps:], v)
		}
	}
}

// DecodeChannels дописывает в dst[c] frames отсчётов каждого канала (len(dst) == Channels).
func (f Format) DecodeChannels(data []byte, frames int, dst [][]float32) {
	bps := f.Bits / 8
	for i := 0; i < frames; i++ {
		base := i * f.BlockAlign
		for c := 0; c < f.Channels; c++ {
			dst[c] = append(dst[c], f.sample(data[base+c*bps:]))
		}
	}
}

// MonoOf дописывает в dst среднее по каналам.
func MonoOf(chs [][]float32, dst []float32) []float32 {
	if len(chs) == 0 {
		return dst
	}
	n := len(chs[0])
	inv := 1 / float32(len(chs))
	for i := 0; i < n; i++ {
		var s float32
		for c := range chs {
			s += chs[c][i]
		}
		dst = append(dst, s*inv)
	}
	return dst
}

// MixLR сводит многоканальный сигнал в левую и правую стороны.

func MixLR(chs [][]float32, l, r []float32) ([]float32, []float32) {
	if len(chs) == 0 {
		return l, r
	}
	n := len(chs[0])
	for i := 0; i < n; i++ {
		var a, b float32
		switch len(chs) {
		case 1:
			a, b = chs[0][i], chs[0][i]
		case 2:
			a, b = chs[0][i], chs[1][i]
		case 4:
			a, b = chs[0][i]+chs[2][i], chs[1][i]+chs[3][i]
		case 6, 8:
			c := 0.707 * chs[2][i]
			a = chs[0][i] + c + chs[4][i]
			b = chs[1][i] + c + chs[5][i]
			if len(chs) == 8 {
				a += chs[6][i]
				b += chs[7][i]
			}
		default:
			for c := range chs {
				if c%2 == 0 {
					a += chs[c][i]
				} else {
					b += chs[c][i]
				}
			}
		}
		l = append(l, a)
		r = append(r, b)
	}
	return l, r
}
