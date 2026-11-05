package rekordbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"unicode/utf16"
)

var be = binary.BigEndian

type Beat struct {
	Number uint16
	BPM    float64
	TimeMS uint32
}
type Cue struct {
	HotCue                         uint32 // 0 = memory cue; 1..8 = A..H; never assigned by the bridge
	TimeMS, EndMS                  uint32
	Loop, Active                   bool
	Name                           string
	Color                          [4]byte // A,R,G,B
	ColorID, ColorCode             byte
	LoopNumerator, LoopDenominator uint16
	Extended                       bool
}
type Waveform struct {
	Tag        string
	EntryBytes uint32
	Data       []byte
}
type Phrase struct {
	Index, Beat, Kind uint16
	Raw               []byte
}
type SongStructure struct {
	Mood, EndBeat uint16
	Bank          byte
	Phrases       []Phrase
}
type Analysis struct {
	Beats     []Beat
	Cues      []Cue
	Waveforms []Waveform
	Structure *SongStructure
	Unknown   map[string][][]byte
}

const MaxAnalysisBytes = 128 << 20

// ParseAnalysis validates every section boundary, including unrecognized tags.
func ParseAnalysis(b []byte) (*Analysis, error) {
	if len(b) < 12 || len(b) > MaxAnalysisBytes || string(b[:4]) != "PMAI" {
		return nil, fmt.Errorf("anlz: invalid PMAI header")
	}
	h, n := int(be.Uint32(b[4:])), int(be.Uint32(b[8:]))
	if h < 12 || h > n || n != len(b) {
		return nil, fmt.Errorf("anlz: invalid file length")
	}
	a := &Analysis{Unknown: map[string][][]byte{}}
	for off := h; off < n; {
		if n-off < 12 {
			return nil, fmt.Errorf("anlz: short section")
		}
		s := b[off:]
		tag := string(s[:4])
		sh, sn := int(be.Uint32(s[4:])), int(be.Uint32(s[8:]))
		if sh < 12 || sn < sh || sn > n-off {
			return nil, fmt.Errorf("anlz: invalid %s length", tag)
		}
		s = s[:sn]
		if err := a.section(tag, s, sh); err != nil {
			return nil, fmt.Errorf("anlz %s: %w", tag, err)
		}
		off += sn
	}
	return a, nil
}

func (a *Analysis) section(tag string, s []byte, h int) error {
	bad := func() error { return fmt.Errorf("truncated or invalid data") }
	switch tag {
	case "PQTZ":
		if len(s) < 24 {
			return bad()
		}
		n64 := uint64(be.Uint32(s[20:]))
		if n64 > uint64((len(s)-24)/8) {
			return bad()
		}
		n := int(n64)
		for i := 0; i < n; i++ {
			p := s[24+8*i:]
			a.Beats = append(a.Beats, Beat{be.Uint16(p), float64(be.Uint16(p[2:])) / 100, be.Uint32(p[4:])})
		}
	case "PCOB", "PCO2":
		min, off, countAt := 24, 24, 18
		if tag == "PCO2" {
			min, off, countAt = 20, 20, 16
		}
		if len(s) < min {
			return bad()
		}
		n := int(be.Uint16(s[countAt:]))
		if n > (len(s)-off)/12 {
			return bad()
		}
		for i := 0; i < n; i++ {
			if len(s)-off < 12 {
				return bad()
			}
			p := s[off:]
			size := int(be.Uint32(p[8:]))
			if size < 12 || size > len(p) {
				return bad()
			}
			p = p[:size]
			c := Cue{}
			if tag == "PCOB" {
				if size < 56 || string(p[:4]) != "PCPT" {
					return bad()
				}
				c.HotCue = be.Uint32(p[12:])
				c.Active = be.Uint32(p[16:]) == 4
				c.Loop = p[28] == 2
				c.TimeMS = be.Uint32(p[32:])
				c.EndMS = be.Uint32(p[36:])
			} else {
				if size < 40 || string(p[:4]) != "PCP2" {
					return bad()
				}
				c.Extended = true
				c.HotCue = be.Uint32(p[12:])
				c.Loop = p[16] == 2
				c.TimeMS = be.Uint32(p[20:])
				c.EndMS = be.Uint32(p[24:])
				c.ColorID = p[28]
				c.LoopNumerator = be.Uint16(p[36:])
				c.LoopDenominator = be.Uint16(p[38:])
				if size > 43 {
					n := int(be.Uint32(p[40:]))
					if n < 0 || n%2 != 0 || n > size-44 {
						return bad()
					}
					c.Name = decodeUTF16BE(p[44 : 44+n])
					q := 44 + n
					if size > q {
						c.ColorCode = p[q]
					}
					if size >= q+4 {
						c.Color = [4]byte{255, p[q+1], p[q+2], p[q+3]}
					}
				}
			}
			if c.Loop && c.EndMS < c.TimeMS {
				return fmt.Errorf("loop end precedes start")
			}
			a.mergeCue(c)
			off += size
		}
	case "PWV3", "PWV5", "PWV7", "PWV4", "PWAV", "PWV2":
		if len(s) < 20 {
			return bad()
		}
		width, count := uint32(1), be.Uint32(s[12:])
		if tag == "PWV3" || tag == "PWV4" || tag == "PWV5" || tag == "PWV7" {
			if len(s) < 24 {
				return bad()
			}
			width, count = be.Uint32(s[12:]), be.Uint32(s[16:])
		}
		if tag == "PWV4" {
			width = 6
		}
		if width == 0 || uint64(width)*uint64(count) > uint64(len(s)-h) {
			return bad()
		}
		a.Waveforms = append(a.Waveforms, Waveform{tag, width, append([]byte(nil), s[h:h+int(width*count)]...)})
	case "PSSI":
		if len(s) < 32 {
			return bad()
		}
		width, n := int(be.Uint32(s[12:])), int(be.Uint16(s[16:]))
		if width < 24 {
			return bad()
		}
		body := append([]byte(nil), s[18:]...)
		if be.Uint16(body) > 20 {
			mask := []byte{0xcb, 0xe1, 0xee, 0xfa, 0xe5, 0xee, 0xad, 0xee, 0xe9, 0xd2, 0xe9, 0xeb, 0xe1, 0xe9, 0xf3, 0xe8, 0xe9, 0xf4, 0xe1}
			for i := range body {
				body[i] ^= mask[i%len(mask)] + byte(n)
			}
		}
		if n > (len(body)-14)/width {
			return bad()
		}
		st := &SongStructure{Mood: be.Uint16(body), EndBeat: be.Uint16(body[8:]), Bank: body[12]}
		for i := 0; i < n; i++ {
			p := body[14+i*width : 14+(i+1)*width]
			st.Phrases = append(st.Phrases, Phrase{be.Uint16(p), be.Uint16(p[2:]), be.Uint16(p[4:]), append([]byte(nil), p...)})
		}
		a.Structure = st
	default:
		a.Unknown[tag] = append(a.Unknown[tag], append([]byte(nil), s...))
	}
	return nil
}

func decodeUTF16BE(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = be.Uint16(b[2*i:])
	}
	return strings.TrimRight(string(utf16.Decode(u)), "\x00")
}

func (a *Analysis) mergeCue(c Cue) {
	for i, old := range a.Cues {
		if (c.HotCue != 0 && c.HotCue == old.HotCue) || (c.HotCue == 0 && old.HotCue == 0 && c.TimeMS == old.TimeMS && c.Loop == old.Loop && (!c.Loop || c.EndMS == old.EndMS)) {
			if c.Extended || !old.Extended {
				c.Active = c.Active || old.Active
				a.Cues[i] = c
			} else {
				a.Cues[i].Active = old.Active || c.Active
			}
			return
		}
	}
	a.Cues = append(a.Cues, c)
}

// Merge prefers extended cues while keeping DAT-only memory cues and all waveform variants.
func (a *Analysis) Merge(b *Analysis) {
	if len(b.Beats) > 0 {
		a.Beats = b.Beats
	}
	for _, c := range b.Cues {
		a.mergeCue(c)
	}
	a.Waveforms = append(a.Waveforms, b.Waveforms...)
	if b.Structure != nil {
		a.Structure = b.Structure
	}
	if a.Unknown == nil {
		a.Unknown = map[string][][]byte{}
	}
	for k, v := range b.Unknown {
		a.Unknown[k] = append(a.Unknown[k], v...)
	}
}

func ReadAnalysis(path string) (*Analysis, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if st.Size() > MaxAnalysisBytes {
		return nil, fmt.Errorf("anlz: file too large")
	}
	b := make([]byte, int(st.Size()))
	if _, e = f.ReadAt(b, 0); e != nil {
		return nil, e
	}
	return ParseAnalysis(b)
}
