package cdj

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// ANLZ file header magic
var ANLZMagic = []byte("PMAI")

// Tag FourCCs
const (
	TagPQTZ = "PQTZ" // Beat grid
	TagPQT2 = "PQT2" // Extended beat grid
	TagPWV5 = "PWV5" // Waveform color detail (in .EXT)
	TagPWV7 = "PWV7" // Waveform 3-band detail (in .2EX)
	TagPWV4 = "PWV4" // Waveform color preview
	TagPWV6 = "PWV6" // Waveform 3-band preview
	TagPWAV = "PWAV" // Waveform preview
	TagPWV2 = "PWV2" // Waveform tiny preview
	TagPWV3 = "PWV3" // Waveform detail
	TagPPTH = "PPTH" // Path
	TagPCOB = "PCOB" // Cues
	TagPCO2 = "PCO2" // Cues 2
	TagPVBR = "PVBR" // VBR seek index
)

// ANLZFile represents a parsed rekordbox analysis file.
type ANLZFile struct {
	HeaderLength uint32
	FileLength   uint32
	Beats        []BeatPoint
	WaveformCols [][3]byte // Normalized [mid, high, low] or RGB levels
	Path         string
	Sections     map[string][]byte
}

// BeatPoint represents an individual beat entry from PQTZ.
type BeatPoint struct {
	Beat   uint16  // 1-4
	BPM    float64 // Exact BPM
	TimeMS uint32  // Milliseconds from track start
}

// ParseANLZ parses an ANLZ (.DAT, .EXT, .2EX) file.
func ParseANLZ(data []byte) (*ANLZFile, error) {
	if len(data) < 28 {
		return nil, fmt.Errorf("anlz: file too short (%d bytes, min 28)", len(data))
	}
	if !bytes.Equal(data[:4], ANLZMagic) {
		return nil, fmt.Errorf("anlz: invalid magic %q, expected PMAI", data[:4])
	}

	hdrLen := binary.BigEndian.Uint32(data[4:8])
	fileLen := binary.BigEndian.Uint32(data[8:12])

	f := &ANLZFile{
		HeaderLength: hdrLen,
		FileLength:   fileLen,
		Sections:     make(map[string][]byte),
	}

	offset := int(hdrLen)
	for offset+12 <= len(data) {
		tag := string(data[offset : offset+4])
		tagHdrLen := binary.BigEndian.Uint32(data[offset+4 : offset+8])
		tagLen := binary.BigEndian.Uint32(data[offset+8 : offset+12])

		if tagLen < 12 || offset+int(tagLen) > len(data) {
			break
		}

		tagData := data[offset : offset+int(tagLen)]
		f.Sections[tag] = tagData

		switch tag {
		case TagPQTZ:
			if err := f.parsePQTZ(tagData, tagHdrLen); err != nil {
				return nil, err
			}
		case TagPWV5:
			f.parsePWV5(tagData, tagHdrLen)
		case TagPWV7:
			f.parsePWV7(tagData, tagHdrLen)
		case TagPPTH:
			f.parsePPTH(tagData, tagHdrLen)
		}

		offset += int(tagLen)
	}

	return f, nil
}

func (f *ANLZFile) parsePQTZ(data []byte, hdrLen uint32) error {
	if len(data) < int(hdrLen) || hdrLen < 24 {
		return fmt.Errorf("anlz: invalid PQTZ header length %d", hdrLen)
	}
	numBeats := binary.BigEndian.Uint32(data[20:24])
	entriesOffset := int(hdrLen)
	if entriesOffset+int(numBeats)*8 > len(data) {
		return fmt.Errorf("anlz: truncated PQTZ entries")
	}

	f.Beats = make([]BeatPoint, numBeats)
	for i := 0; i < int(numBeats); i++ {
		off := entriesOffset + i*8
		beatNum := binary.BigEndian.Uint16(data[off : off+2])
		tempo := binary.BigEndian.Uint16(data[off+2 : off+4])
		timeMS := binary.BigEndian.Uint32(data[off+4 : off+8])

		f.Beats[i] = BeatPoint{
			Beat:   beatNum,
			BPM:    float64(tempo) / 100.0,
			TimeMS: timeMS,
		}
	}
	return nil
}

func (f *ANLZFile) parsePWV5(data []byte, hdrLen uint32) {
	if len(data) < int(hdrLen) || hdrLen < 24 {
		return
	}
	numEntries := binary.BigEndian.Uint32(data[16:20])
	entriesOffset := int(hdrLen)
	if entriesOffset+int(numEntries)*2 > len(data) {
		return
	}

	f.WaveformCols = make([][3]byte, numEntries)
	for i := 0; i < int(numEntries); i++ {
		off := entriesOffset + i*2
		val := binary.BigEndian.Uint16(data[off : off+2])
		red := byte((val >> 13) & 0x07)
		green := byte((val >> 10) & 0x07)
		blue := byte((val >> 7) & 0x07)
		// scale 0..7 to 0..31 roughly
		f.WaveformCols[i] = [3]byte{red << 2, green << 2, blue << 2}
	}
}

func (f *ANLZFile) parsePWV7(data []byte, hdrLen uint32) {
	if len(data) < int(hdrLen) || hdrLen < 24 {
		return
	}
	numEntries := binary.BigEndian.Uint32(data[16:20])
	entriesOffset := int(hdrLen)
	if entriesOffset+int(numEntries)*3 > len(data) {
		return
	}

	f.WaveformCols = make([][3]byte, numEntries)
	for i := 0; i < int(numEntries); i++ {
		off := entriesOffset + i*3
		f.WaveformCols[i] = [3]byte{data[off], data[off+1], data[off+2]}
	}
}

func (f *ANLZFile) parsePPTH(data []byte, hdrLen uint32) {
	if len(data) < int(hdrLen) || hdrLen < 16 {
		return
	}
	lenPath := binary.BigEndian.Uint32(data[12:16])
	if lenPath <= 2 || int(hdrLen)+int(lenPath) > len(data) {
		return
	}
	raw := data[hdrLen : hdrLen+lenPath-2] // exclude trailing NUL uint16
	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = binary.BigEndian.Uint16(raw[2*i : 2*i+2])
	}
	f.Path = string(utf16.Decode(u16))
}
