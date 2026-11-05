// Package testfixture constructs independent wire-format Pioneer exports.
package testfixture

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

var le = binary.LittleEndian
var be = binary.BigEndian

const PageSize = 4096
const UnicodeName = "Café ガポ각Ａ.wav"

func Write(t testing.TB, root, rel string, b []byte) {
	t.Helper()
	p := filepath.Join(root, rel)
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, b, 0644); e != nil {
		t.Fatal(e)
	}
}
func str(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 4+2*len(u))
	b[0] = 0x90
	le.PutUint16(b[1:], uint16(len(b)))
	for i, v := range u {
		le.PutUint16(b[4+2*i:], v)
	}
	return b
}
func track(id uint32, name, key string, bpm uint32) []byte {
	b := make([]byte, 136)
	le.PutUint32(b[8:], 48000)
	le.PutUint32(b[32:], id)
	le.PutUint32(b[56:], bpm)
	le.PutUint32(b[68:], 1)
	le.PutUint32(b[72:], id)
	le.PutUint16(b[82:], 24)
	le.PutUint16(b[84:], 2)
	for i := 0; i < 21; i++ {
		s := ""
		switch i {
		case 14:
			if id == 1 {
				s = "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"
			}
		case 17:
			s = name
		case 20:
			if id == 1 {
				s = "/Music/" + UnicodeName
			} else {
				s = "/Music/" + name
			}
		}
		le.PutUint16(b[94+2*i:], uint16(len(b)))
		b = append(b, str(s)...)
	}
	return b
}
func playlist(id, parent, order uint32, name string, folder bool) []byte {
	b := make([]byte, 20)
	le.PutUint32(b, parent)
	le.PutUint32(b[8:], order)
	le.PutUint32(b[12:], id)
	if folder {
		le.PutUint32(b[16:], 1)
	}
	return append(b, str(name)...)
}
func entry(order, track, list uint32) []byte {
	b := make([]byte, 12)
	le.PutUint32(b, order)
	le.PutUint32(b[4:], track)
	le.PutUint32(b[8:], list)
	return b
}

func PDB() []byte {
	type table struct {
		typ  uint32
		rows [][]byte
	}
	ts := []table{{0, nil}, {2, nil}, {5, nil}, {7, nil}, {8, nil}, {16, [][]byte{[]byte("opaque column definition")}}}
	names := []string{UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}
	keys := []string{"8A", "9A", "8B", "11A", "8A"}
	bpms := []uint32{12000, 12400, 11800, 12100, 12401}
	for i, n := range names {
		ts[0].rows = append(ts[0].rows, track(uint32(i+1), n, keys[i], bpms[i]))
		b := make([]byte, 8)
		le.PutUint32(b, uint32(i+1))
		le.PutUint32(b[4:], uint32(i+1))
		ts[2].rows = append(ts[2].rows, append(b, str(keys[i])...))
	}
	b := make([]byte, 10)
	le.PutUint32(b[4:], 1)
	b[9] = 10
	ts[1].rows = [][]byte{append(b, str("Artist")...)}
	ts[3].rows = [][]byte{playlist(10, 0, 1, "Folder", true), playlist(11, 10, 1, "Nested", true), playlist(12, 11, 1, "同名", false), playlist(13, 0, 2, "Other", true), playlist(14, 13, 1, "同名", false)}
	// Input row order intentionally differs from playlist sequence order.
	ts[4].rows = [][]byte{entry(2, 2, 12), entry(1, 1, 12), entry(1, 3, 14), entry(2, 4, 14)}
	// Add a non-data page at the start of the Track chain.
	out := make([]byte, PageSize*(len(ts)+2))
	le.PutUint32(out[4:], PageSize)
	le.PutUint32(out[8:], uint32(len(ts)))
	le.PutUint32(out[12:], uint32(len(ts)+2))
	le.PutUint32(out[20:], 1)
	for i, t := range ts {
		n := i + 2
		h := out[28+i*16:]
		le.PutUint32(h, t.typ)
		le.PutUint32(h[8:], uint32(n))
		le.PutUint32(h[12:], uint32(n))
		if i == 0 {
			le.PutUint32(h[8:], 1)
			p := out[PageSize : 2*PageSize]
			le.PutUint32(p[4:], 1)
			le.PutUint32(p[8:], 0)
			le.PutUint32(p[12:], 2)
			p[27] = 0x40
		}
		p := out[n*PageSize : (n+1)*PageSize]
		le.PutUint32(p[4:], uint32(n))
		le.PutUint32(p[8:], t.typ)
		p[27] = 0x24
		count := len(t.rows)
		le.PutUint16(p[24:], uint16(count))
		off := 40
		for j, row := range t.rows {
			copy(p[off:], row)
			base := PageSize - (j/16)*36
			le.PutUint16(p[base-6-2*(j%16):], uint16(off-40))
			flags := le.Uint16(p[base-4:])
			le.PutUint16(p[base-4:], flags|1<<uint(j%16))
			off += len(row)
		}
		le.PutUint16(p[30:], uint16(off-40))
	}
	return out
}

func Section(tag string, h int, payload []byte) []byte {
	b := make([]byte, 12)
	copy(b, tag)
	be.PutUint32(b[4:], uint32(h))
	be.PutUint32(b[8:], uint32(12+len(payload)))
	return append(b, payload...)
}
func PMAI(sections ...[]byte) []byte {
	b := make([]byte, 28)
	copy(b, "PMAI")
	be.PutUint32(b[4:], 28)
	for _, s := range sections {
		b = append(b, s...)
	}
	be.PutUint32(b[8:], uint32(len(b)))
	return b
}
func cueOld(hot, time uint32) []byte {
	b := make([]byte, 56)
	copy(b, "PCPT")
	be.PutUint32(b[4:], 28)
	be.PutUint32(b[8:], 56)
	be.PutUint32(b[12:], hot)
	b[28] = 1
	be.PutUint32(b[32:], time)
	return b
}
func CueExtended(hot, time, end uint32, name string, color [3]byte) []byte {
	u := utf16.Encode([]rune(name + "\x00"))
	b := make([]byte, 48+2*len(u))
	copy(b, "PCP2")
	be.PutUint32(b[4:], 16)
	be.PutUint32(b[8:], uint32(len(b)))
	be.PutUint32(b[12:], hot)
	b[16] = 1
	if end > 0 {
		b[16] = 2
	}
	be.PutUint32(b[20:], time)
	be.PutUint32(b[24:], end)
	be.PutUint32(b[40:], uint32(2*len(u)))
	for i, v := range u {
		be.PutUint16(b[44+2*i:], v)
	}
	q := 44 + 2*len(u)
	b[q] = 7
	copy(b[q+1:], color[:])
	return b
}
func DAT() []byte {
	beats := make([]byte, 12+4*8)
	be.PutUint32(beats[4:], 0x80000)
	be.PutUint32(beats[8:], 4)
	for i := 0; i < 4; i++ {
		p := beats[12+8*i:]
		be.PutUint16(p, uint16(i+1))
		be.PutUint16(p[2:], 12000)
		be.PutUint32(p[4:], uint32(i*500))
	}
	hot := make([]byte, 12)
	be.PutUint32(hot, 1)
	be.PutUint16(hot[6:], 1)
	hot = append(hot, cueOld(2, 500)...)
	memory := make([]byte, 12)
	be.PutUint16(memory[6:], 1)
	memory = append(memory, cueOld(0, 250)...)
	return PMAI(Section("PQTZ", 24, beats), Section("PCOB", 24, hot), Section("PCOB", 24, memory))
}
func EXT() []byte {
	cues := make([]byte, 8)
	be.PutUint32(cues, 1)
	be.PutUint16(cues[4:], 2)
	cues = append(cues, CueExtended(2, 500, 0, "B 原有", [3]byte{12, 34, 56})...)
	cues = append(cues, CueExtended(8, 1000, 1500, "H Loop", [3]byte{99, 88, 77})...)
	memory := make([]byte, 8)
	be.PutUint16(memory[4:], 2)
	memory = append(memory, CueExtended(0, 250, 0, "Mémoire", [3]byte{55, 66, 77})...)
	memory = append(memory, CueExtended(0, 750, 1000, "Memory Loop", [3]byte{9, 8, 7})...)
	wave := make([]byte, 12+8)
	be.PutUint32(wave, 2)
	be.PutUint32(wave[4:], 4)
	for i := 0; i < 4; i++ {
		be.PutUint16(wave[12+2*i:], 0xfc7c)
	}
	phrases := make([]byte, 20+24)
	be.PutUint32(phrases, 24)
	be.PutUint16(phrases[4:], 1)
	be.PutUint16(phrases[6:], 2)
	be.PutUint16(phrases[14:], 4)
	be.PutUint16(phrases[20:], 1)
	be.PutUint16(phrases[22:], 1)
	be.PutUint16(phrases[24:], 6)
	return PMAI(Section("PCO2", 20, cues), Section("PCO2", 20, memory), Section("PWV5", 24, wave), Section("PSSI", 32, phrases))
}
func EX2() []byte {
	wave := make([]byte, 12+12)
	be.PutUint32(wave, 3)
	be.PutUint32(wave[4:], 4)
	copy(wave[12:], []byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120})
	mono := make([]byte, 12+4)
	be.PutUint32(mono, 1)
	be.PutUint32(mono[4:], 4)
	copy(mono[12:], []byte{1, 2, 3, 4})
	return PMAI(Section("PWV7", 24, wave), Section("PWV3", 24, mono))
}

func WAV(extensible bool) []byte {
	n := 16
	if extensible {
		n = 40
	}
	data := 48000 * 2 * 3 * 2
	b := make([]byte, 12+8+n+8+data)
	copy(b, "RIFF")
	le.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	le.PutUint32(b[16:], uint32(n))
	f := b[20:]
	le.PutUint16(f, 1)
	if extensible {
		le.PutUint16(f, 0xfffe)
		le.PutUint16(f[16:], 22)
		le.PutUint16(f[18:], 24)
		le.PutUint16(f[24:], 1)
		copy(f[26:], []byte{0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71})
	}
	le.PutUint16(f[2:], 2)
	le.PutUint32(f[4:], 48000)
	le.PutUint32(f[8:], 288000)
	le.PutUint16(f[12:], 6)
	le.PutUint16(f[14:], 24)
	copy(b[20+n:], "data")
	le.PutUint32(b[24+n:], uint32(data))
	return b
}
func FLAC() []byte {
	const picture = 2 << 20
	b := make([]byte, 4+4+34+4+picture+16)
	copy(b, "fLaC")
	b[7] = 34
	v := uint64(48000)<<44 | uint64(1)<<41 | uint64(23)<<36 | 96000
	be.PutUint64(b[18:], v)
	b[42] = 0x86
	b[43] = byte(picture >> 16)
	return b
}

func Export(t testing.TB, root string) {
	t.Helper()
	Write(t, root, "PIONEER/rekordbox/export.pdb", PDB())
	base := "PIONEER/USBANLZ/P001/ANLZ0000"
	Write(t, root, base+".DAT", DAT())
	Write(t, root, base+".EXT", EXT())
	Write(t, root, base+".2EX", EX2())
	Write(t, root, "MUSIC/"+norm.NFD.String(UnicodeName), WAV(true))
	Write(t, root, "MUSIC/two.flac", FLAC())
	for _, n := range []string{"three.wav", "four.wav", "five.wav"} {
		Write(t, root, "MUSIC/"+n, WAV(false))
	}
}
