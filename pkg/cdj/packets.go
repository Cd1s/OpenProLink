package cdj

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strings"
	"unicode/utf16"
)

// Magic header present on all Pro DJ Link packets.
var MagicHeader = []byte("Qspt1WmJOL")

// Port constants
const (
	PortDiscovery = 50000
	PortBeat      = 50001
	PortStatus    = 50002
	PortDBServer  = 12523
)

// Packet types
const (
	TypeKeepalive        byte = 0x06
	TypeStatus           byte = 0x0a
	TypeBeat             byte = 0x28
	TypeAbsolutePosition byte = 0x0b
	TypeMediaQuery       byte = 0x05
	TypeMediaResponse    byte = 0x06
)

// Track source slots (Sr)
const (
	SlotNoTrack    byte = 0x00
	SlotCD         byte = 0x01
	SlotSD         byte = 0x02
	SlotUSB        byte = 0x03
	SlotCollection byte = 0x04
	SlotStreaming  byte = 0x06
	SlotUSB2       byte = 0x07
	SlotBeatport   byte = 0x09
)

// Track types (Tr)
const (
	TrackTypeNone       byte = 0x00
	TrackTypeRekordbox  byte = 0x01
	TrackTypeUnanalyzed byte = 0x02
	TrackTypeCDAudio    byte = 0x05
	TrackTypeStreaming  byte = 0x06
)

// Play state 1 (P1)
const (
	PlayState1NoTrack       byte = 0x00
	PlayState1Loading       byte = 0x02
	PlayState1Playing       byte = 0x03
	PlayState1Looping       byte = 0x04
	PlayState1Paused        byte = 0x05
	PlayState1PausedCue     byte = 0x06
	PlayState1CuePlay       byte = 0x07
	PlayState1CueScratch    byte = 0x08
	PlayState1Searching     byte = 0x09
	PlayState1CDSpunDown    byte = 0x0e
	PlayState1EndOfTrack    byte = 0x11
	PlayState1EmergencyLoop byte = 0x12
)

// Play state 2 (P2) low nibble
const (
	PlayState2Moving  byte = 0x0a
	PlayState2Stopped byte = 0x0e
)

// Status Flags (byte 0x89)
const (
	FlagBPMSync byte = 0x02
	FlagOnAir   byte = 0x08
	FlagSync    byte = 0x10
	FlagMaster  byte = 0x20
	FlagPlay    byte = 0x40
)

// Pitch conversions
func PitchToWord(pct float64) uint32 {
	return uint32(math.Round(float64(0x100000) * (1 + pct/100)))
}

func WordToPitch(w uint32) float64 {
	return (float64(w) - float64(0x100000)) / float64(0x100000) * 100
}

// Keepalive packet (UDP 50000, 54 bytes)
type Keepalive struct {
	DeviceName      string
	PlayerNumber    byte
	FirstDevice     bool
	FirstDeviceFlag byte // 0x02 if booted alone, 0x01 if joined existing network
	MAC             net.HardwareAddr
	IP              net.IP
	PeerCount       byte
	DeviceClass     byte // 0x01 CDJ, 0x02 Mixer
	ModelCapability byte // 0x64 for CDJ-3000 compatibility, 0x00 legacy
}

// NewKeepalivePacket constructs a 54-byte UDP 50000 keepalive packet.
func NewKeepalivePacket(k Keepalive) []byte {
	b := make([]byte, 54)
	copy(b, MagicHeader)
	b[10] = TypeKeepalive
	b[11] = 0x00

	name := k.DeviceName
	if name == "" {
		name = "CDJ-3000"
	}
	copy(b[12:32], name)
	b[32] = 0x01 // terminator at 0x20
	b[0x21] = 0x02
	binary.BigEndian.PutUint16(b[0x22:], 54)
	b[0x24] = k.PlayerNumber

	flag := k.FirstDeviceFlag
	if flag == 0 {
		if k.FirstDevice {
			flag = 0x02
		} else {
			flag = 0x01
		}
	}
	b[0x25] = flag

	if len(k.MAC) >= 6 {
		copy(b[0x26:0x2c], k.MAC[:6])
	} else {
		copy(b[0x26:0x2c], []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x20})
	}

	if ip4 := k.IP.To4(); ip4 != nil {
		copy(b[0x2c:0x30], ip4)
	}

	peerCount := k.PeerCount
	if peerCount == 0 {
		peerCount = 1
	}
	b[0x30] = peerCount

	devClass := k.DeviceClass
	if devClass == 0 {
		devClass = 0x01 // CDJ
	}
	b[0x34] = devClass

	b[0x35] = k.ModelCapability
	return b
}

// ParseKeepalivePacket parses a UDP 50000 keepalive packet.
func ParseKeepalivePacket(b []byte) (*Keepalive, error) {
	if len(b) < 54 {
		return nil, fmt.Errorf("cdj: keepalive packet too short (%d bytes, expected 54)", len(b))
	}
	if !bytes.Equal(b[:10], MagicHeader) || b[10] != TypeKeepalive {
		return nil, fmt.Errorf("cdj: not a valid keepalive packet")
	}

	nameEnd := 32
	for i := 12; i < 32; i++ {
		if b[i] == 0 {
			nameEnd = i
			break
		}
	}
	name := string(b[12:nameEnd])

	mac := make(net.HardwareAddr, 6)
	copy(mac, b[0x26:0x2c])

	ip := net.IPv4(b[0x2c], b[0x2d], b[0x2e], b[0x2f])

	return &Keepalive{
		DeviceName:      name,
		PlayerNumber:    b[0x24],
		FirstDevice:     b[0x25] == 0x02,
		FirstDeviceFlag: b[0x25],
		MAC:             mac,
		IP:              ip,
		PeerCount:       b[0x30],
		DeviceClass:     b[0x34],
		ModelCapability: b[0x35],
	}, nil
}

// Status packet representation
type Status struct {
	DeviceName         string
	PlayerNumber       byte
	PacketVersion      byte // Subtype at 0x20: 0x03, 0x04, 0x05, 0x06
	Active             bool // Activity flag at 0x27
	TrackSourcePlayer  byte // 0x28
	TrackSourceSlot    byte // 0x29
	TrackType          byte // 0x2a
	TrackID            uint32
	TrackIndex         uint16
	USBPresent         bool // 0x6f == 0
	SDPresent          bool // 0x73 == 0
	USBLocalState      byte // 0x6f: 0=mounted, 2/3=unmounting, 4=empty
	SDLocalState       byte // 0x73: 0=mounted, 2/3=unmounting, 4=empty
	LinkAvailable      bool // 0x75 == 1
	PlayState1         byte // 0x7b
	Firmware           string
	Playing            bool
	Master             bool
	Sync               bool
	OnAir              bool
	BPMSync            bool
	PlayState2         byte // 0x8b
	Pitch              float64
	RawPitch           uint32
	BPM                float64
	RawBPM             uint16
	MasterValidBPM     bool // 0x90 == 0x8000
	PlayState3         byte // 0x9d
	MasterHandoff      byte // 0x9f (0xff if none)
	Beat               uint32
	BeatWithinBar      byte
	MediaPresence      byte // 0xb7 on CDJ-3000
	UnsafeEjectUSB     bool // 0xb8
	UnsafeEjectSD      bool // 0xb9
	EmergencyLoop      bool // 0xba
	TouchAudio         bool // 0xcd bit 5
	WaveformColor      byte // 0xda (Settings block 1, byte 10): 1 Blue, 3 RGB, 4 3-band
	MasterTempo        bool // 0x158 (CDJ-3000)
	BufferFull         bool // 0x11f (CDJ-3000)
	LoopStartMS        uint32
	LoopEndMS          uint32
	LoopBeats          uint16
	RawLengthRemaining uint16
}

func statusHeader(size int, number byte, name string, version byte) []byte {
	b := make([]byte, size)
	copy(b, MagicHeader)
	b[10] = TypeStatus

	if name == "" {
		name = "CDJ-3000"
	}
	copy(b[11:31], name)
	b[31] = 0x01 // 0x1f

	if version == 0 {
		version = 0x06
	}
	b[0x20] = version
	b[0x21] = number
	binary.BigEndian.PutUint16(b[0x22:], uint16(size-0x24))
	b[0x24] = number
	b[0x25] = 0x00
	b[0x26] = 0x01
	return b
}

// NewCDJ3000StatusPacket constructs a 512-byte (0x200) CDJ-3000 status packet.
func NewCDJ3000StatusPacket(s Status) []byte {
	b := statusHeader(0x200, s.PlayerNumber, s.DeviceName, s.PacketVersion)

	if s.Active || s.Playing {
		b[0x27] = 0x01
	}

	b[0x28] = s.TrackSourcePlayer
	b[0x29] = s.TrackSourceSlot
	b[0x2a] = s.TrackType

	binary.BigEndian.PutUint32(b[0x2c:], s.TrackID)
	binary.BigEndian.PutUint16(b[0x32:], s.TrackIndex)

	// USB / SD activity and status
	b[0x6a] = 0x04
	b[0x6b] = 0x04

	// USB local state: 0x00 mounted, 0x04 empty
	if s.USBPresent || s.USBLocalState == 0 {
		b[0x6f] = s.USBLocalState
	} else {
		b[0x6f] = 0x04
	}

	// SD local state: 0x00 mounted, 0x04 empty
	if s.SDPresent || s.SDLocalState == 0 {
		b[0x73] = s.SDLocalState
	} else {
		b[0x73] = 0x04
	}

	if s.LinkAvailable || b[0x6f] == 0 || b[0x73] == 0 {
		b[0x75] = 0x01
	}

	// PlayState1
	p1 := s.PlayState1
	if p1 == 0 {
		if s.Playing {
			p1 = PlayState1Playing
		} else if s.TrackID > 0 {
			p1 = PlayState1Paused
		}
	}
	b[0x7b] = p1

	fw := s.Firmware
	if fw == "" {
		fw = "3.20"
	}
	copy(b[0x7c:0x80], fw)

	// Status flags
	var flags byte = 0xb8 // default base
	if s.Playing {
		flags |= FlagPlay
	} else {
		flags &^= FlagPlay
	}
	if s.Master {
		flags |= FlagMaster
	}
	if s.Sync {
		flags |= FlagSync
	}
	if s.OnAir {
		flags |= FlagOnAir
	}
	if s.BPMSync {
		flags |= FlagBPMSync
	}
	b[0x89] = flags

	// PlayState2
	p2 := s.PlayState2
	if p2 == 0 {
		if s.Playing {
			p2 = 0xfa
		} else {
			p2 = 0xfe
		}
	}
	b[0x8b] = p2

	// Pitch
	pw := s.RawPitch
	if pw == 0 {
		pw = PitchToWord(s.Pitch)
	}
	for _, off := range []int{0x8c, 0x98, 0xc0, 0xc4} {
		binary.BigEndian.PutUint32(b[off:], pw)
	}

	// BPM
	if s.MasterValidBPM || s.TrackID > 0 {
		binary.BigEndian.PutUint16(b[0x90:], 0x8000)
	} else {
		binary.BigEndian.PutUint16(b[0x90:], 0x7fff)
	}

	rawBPM := s.RawBPM
	if rawBPM == 0 && s.BPM > 0 {
		rawBPM = uint16(math.Round(s.BPM * 100))
	} else if rawBPM == 0 && s.TrackID == 0 {
		rawBPM = 0xffff
	}
	binary.BigEndian.PutUint16(b[0x92:], rawBPM)

	b[0x94] = 0x7f
	b[0x95] = 0xff
	b[0x96] = 0xff
	b[0x97] = 0xff

	p3 := s.PlayState3
	if p3 == 0 && s.Playing {
		p3 = 0x09 // forward vinyl
	}
	b[0x9d] = p3

	if s.Master {
		b[0x9e] = 0x01
	}

	mh := s.MasterHandoff
	if mh == 0 {
		mh = 0xff
	}
	b[0x9f] = mh

	binary.BigEndian.PutUint32(b[0xa0:], s.Beat)
	b[0xa6] = s.BeatWithinBar

	// Media presence bitmask at 0xb7 on CDJ-3000
	mp := s.MediaPresence
	if mp == 0 {
		if b[0x6f] == 0 {
			mp |= 0x02 // USB
		}
		if b[0x73] == 0 {
			mp |= 0x01 // SD
		}
	}
	b[0xb7] = mp

	if s.UnsafeEjectUSB {
		b[0xb8] = 0x01
	}
	if s.UnsafeEjectSD {
		b[0xb9] = 0x01
	}
	if s.EmergencyLoop {
		b[0xba] = 0x01
	}

	b[0xcc] = 0x1f // CDJ-3000 generation
	if s.TouchAudio {
		b[0xcd] = 0x20 // bit 5
	}

	// Settings block 1 (0xd0..0xef)
	copy(b[0xd0:], []byte{0x12, 0x34, 0x56, 0x78, 0x00, 0x00, 0x00, 0x01, 0x01, 0x01})
	wfColor := s.WaveformColor
	if wfColor == 0 {
		wfColor = 0x04 // default 3-band for CDJ-3000
	}
	b[0xda] = wfColor
	b[0xdb] = 0x01
	b[0xdc] = 0x01

	// Settings block 2 (0xf0..0x10f)
	copy(b[0xf0:], []byte{0x12, 0x34, 0x56, 0x78, 0x00, 0x00, 0x00, 0x01})

	// CDJ-3000 extensions
	if s.BufferFull {
		b[0x11f] = 0x01
	}
	if s.MasterTempo {
		b[0x158] = 0x01
	}
	if s.LoopStartMS > 0 {
		val := uint32(uint64(s.LoopStartMS) * 1000 / 65536)
		binary.BigEndian.PutUint32(b[0x1b6:], val)
	}
	if s.LoopEndMS > 0 {
		val := uint32(uint64(s.LoopEndMS) * 1000 / 65536)
		binary.BigEndian.PutUint32(b[0x1be:], val)
	}
	if s.LoopBeats > 0 {
		binary.BigEndian.PutUint16(b[0x1c8:], s.LoopBeats)
	}

	return b
}

// NewCDJ2000NXS2StatusPacket constructs a 284-byte (0x11c) CDJ-2000NXS2 status packet.
func NewCDJ2000NXS2StatusPacket(s Status) []byte {
	b := statusHeader(0x11c, s.PlayerNumber, s.DeviceName, s.PacketVersion)

	if s.Active || s.Playing {
		b[0x27] = 0x01
	}

	b[0x28] = s.TrackSourcePlayer
	b[0x29] = s.TrackSourceSlot
	b[0x2a] = s.TrackType

	binary.BigEndian.PutUint32(b[0x2c:], s.TrackID)
	binary.BigEndian.PutUint16(b[0x32:], s.TrackIndex)

	b[0x6a] = 0x04
	b[0x6b] = 0x04

	if s.USBPresent || s.USBLocalState == 0 {
		b[0x6f] = s.USBLocalState
	} else {
		b[0x6f] = 0x04
	}

	if s.SDPresent || s.SDLocalState == 0 {
		b[0x73] = s.SDLocalState
	} else {
		b[0x73] = 0x04
	}

	if s.LinkAvailable || b[0x6f] == 0 || b[0x73] == 0 {
		b[0x75] = 0x01
	}

	p1 := s.PlayState1
	if p1 == 0 {
		if s.Playing {
			p1 = PlayState1Playing
		} else if s.TrackID > 0 {
			p1 = PlayState1Paused
		}
	}
	b[0x7b] = p1

	fw := s.Firmware
	if fw == "" {
		fw = "1.85"
	}
	copy(b[0x7c:0x80], fw)

	var flags byte = 0xb8
	if s.Playing {
		flags |= FlagPlay
	} else {
		flags &^= FlagPlay
	}
	if s.Master {
		flags |= FlagMaster
	}
	if s.Sync {
		flags |= FlagSync
	}
	if s.OnAir {
		flags |= FlagOnAir
	}
	if s.BPMSync {
		flags |= FlagBPMSync
	}
	b[0x89] = flags

	p2 := s.PlayState2
	if p2 == 0 {
		if s.Playing {
			p2 = 0xfa
		} else {
			p2 = 0xfe
		}
	}
	b[0x8b] = p2

	pw := s.RawPitch
	if pw == 0 {
		pw = PitchToWord(s.Pitch)
	}
	for _, off := range []int{0x8c, 0x98, 0xc0, 0xc4} {
		binary.BigEndian.PutUint32(b[off:], pw)
	}

	if s.MasterValidBPM || s.TrackID > 0 {
		binary.BigEndian.PutUint16(b[0x90:], 0x8000)
	} else {
		binary.BigEndian.PutUint16(b[0x90:], 0x7fff)
	}

	rawBPM := s.RawBPM
	if rawBPM == 0 && s.BPM > 0 {
		rawBPM = uint16(math.Round(s.BPM * 100))
	} else if rawBPM == 0 && s.TrackID == 0 {
		rawBPM = 0xffff
	}
	binary.BigEndian.PutUint16(b[0x92:], rawBPM)

	b[0x94] = 0x7f
	b[0x95] = 0xff
	b[0x96] = 0xff
	b[0x97] = 0xff

	p3 := s.PlayState3
	if p3 == 0 && s.Playing {
		p3 = 0x09
	}
	b[0x9d] = p3

	if s.Master {
		b[0x9e] = 0x01
	}

	mh := s.MasterHandoff
	if mh == 0 {
		mh = 0xff
	}
	b[0x9f] = mh

	binary.BigEndian.PutUint32(b[0xa0:], s.Beat)
	b[0xa6] = s.BeatWithinBar

	b[0xcc] = 0x0f // NXS2 generation

	// Settings block 1 (0xd0..0xef)
	copy(b[0xd0:], []byte{0x12, 0x34, 0x56, 0x78, 0x00, 0x00, 0x00, 0x01, 0x01, 0x01})
	wfColor := s.WaveformColor
	if wfColor == 0 {
		wfColor = 0x03 // default RGB for NXS2
	}
	b[0xda] = wfColor
	b[0xdb] = 0x01
	b[0xdc] = 0x01

	return b
}

// ParseStatusPacket parses a CDJ status packet (0xd0, 0xd4, 0x11c, 0x124, 0x200 bytes).
func ParseStatusPacket(b []byte) (*Status, error) {
	if len(b) < 0xcc {
		return nil, fmt.Errorf("cdj: status packet too short (%d bytes, min 0xcc)", len(b))
	}
	if !bytes.Equal(b[:10], MagicHeader) || b[10] != TypeStatus {
		return nil, fmt.Errorf("cdj: not a valid status packet")
	}

	nameEnd := 31
	for i := 11; i < 31; i++ {
		if b[i] == 0 {
			nameEnd = i
			break
		}
	}
	name := string(b[11:nameEnd])

	st := &Status{
		DeviceName:         name,
		PacketVersion:      b[0x20],
		PlayerNumber:       b[0x21],
		RawLengthRemaining: binary.BigEndian.Uint16(b[0x22:0x24]),
		Active:             b[0x27] != 0,
		TrackSourcePlayer:  b[0x28],
		TrackSourceSlot:    b[0x29],
		TrackType:          b[0x2a],
		TrackID:            binary.BigEndian.Uint32(b[0x2c:0x30]),
		TrackIndex:         binary.BigEndian.Uint16(b[0x32:0x34]),
		USBLocalState:      b[0x6f],
		USBPresent:         b[0x6f] == 0,
		SDLocalState:       b[0x73],
		SDPresent:          b[0x73] == 0,
		LinkAvailable:      b[0x75] == 1,
		PlayState1:         b[0x7b],
		Firmware:           strings.TrimRight(string(b[0x7c:0x80]), "\x00 "),
		PlayState2:         b[0x8b],
		RawPitch:           binary.BigEndian.Uint32(b[0x8c:0x90]),
		MasterValidBPM:     binary.BigEndian.Uint16(b[0x90:0x92]) == 0x8000,
		RawBPM:             binary.BigEndian.Uint16(b[0x92:0x94]),
		PlayState3:         b[0x9d],
		MasterHandoff:      b[0x9f],
		Beat:               binary.BigEndian.Uint32(b[0xa0:0xa4]),
		BeatWithinBar:      b[0xa6],
	}

	st.Pitch = WordToPitch(st.RawPitch)
	if st.RawBPM > 0 && st.RawBPM != 0xffff {
		st.BPM = float64(st.RawBPM) / 100.0
	}

	if len(b) >= 0xd4 {
		flags := b[0x89]
		st.Playing = flags&FlagPlay != 0
		st.Master = flags&FlagMaster != 0
		st.Sync = flags&FlagSync != 0
		st.OnAir = flags&FlagOnAir != 0
		st.BPMSync = flags&FlagBPMSync != 0
	} else {
		// Pre-nexus fallback
		st.Playing = st.PlayState1 == PlayState1Playing || st.PlayState1 == PlayState1Looping ||
			(st.PlayState1 == PlayState1Searching && (st.PlayState2&0x0f) == PlayState2Moving)
	}

	if len(b) > 0xb7 {
		st.MediaPresence = b[0xb7]
	}
	if len(b) > 0xb8 {
		st.UnsafeEjectUSB = b[0xb8] == 1
	}
	if len(b) > 0xb9 {
		st.UnsafeEjectSD = b[0xb9] == 1
	}
	if len(b) > 0xba {
		st.EmergencyLoop = b[0xba] == 1
	}
	if len(b) > 0xcd {
		st.TouchAudio = b[0xcd]&0x20 != 0
	}

	// Settings block 1
	if len(b) >= 0xef && bytes.Equal(b[0xd0:0xd4], []byte{0x12, 0x34, 0x56, 0x78}) {
		st.WaveformColor = b[0xda]
	}

	// CDJ-3000 extensions
	if len(b) >= 0x200 {
		st.BufferFull = b[0x11f] == 1
		st.MasterTempo = b[0x158] == 1
		rawLs := binary.BigEndian.Uint32(b[0x1b6:0x1ba])
		if rawLs > 0 {
			st.LoopStartMS = uint32(uint64(rawLs) * 65536 / 1000)
		}
		rawLe := binary.BigEndian.Uint32(b[0x1be:0x1c2])
		if rawLe > 0 {
			st.LoopEndMS = uint32(uint64(rawLe) * 65536 / 1000)
		}
		st.LoopBeats = binary.BigEndian.Uint16(b[0x1c8:0x1ca])
	}

	return st, nil
}

// Beat packet (UDP 50001, 96 bytes / 0x60)
type Beat struct {
	DeviceName    string
	PlayerNumber  byte
	NextBeatMS    uint32
	SecondBeatMS  uint32
	NextBarMS     uint32
	FourthBeatMS  uint32
	SecondBarMS   uint32
	EighthBeatMS  uint32
	Pitch         float64
	RawPitch      uint32
	BPM           float64
	RawBPM        uint16
	BeatWithinBar byte
}

// NewBeatPacket constructs a 96-byte (0x60) beat announcement packet.
func NewBeatPacket(b Beat) []byte {
	pkt := make([]byte, 0x60)
	copy(pkt, MagicHeader)
	pkt[10] = TypeBeat

	name := b.DeviceName
	if name == "" {
		name = "CDJ-3000"
	}
	copy(pkt[11:31], name)
	pkt[31] = 0x01
	pkt[0x20] = 0x00
	pkt[0x21] = b.PlayerNumber
	binary.BigEndian.PutUint16(pkt[0x22:], 0x003c) // 0x60 - 0x24

	binary.BigEndian.PutUint32(pkt[0x24:], b.NextBeatMS)
	binary.BigEndian.PutUint32(pkt[0x28:], b.SecondBeatMS)
	binary.BigEndian.PutUint32(pkt[0x2c:], b.NextBarMS)
	binary.BigEndian.PutUint32(pkt[0x30:], b.FourthBeatMS)
	binary.BigEndian.PutUint32(pkt[0x34:], b.SecondBarMS)
	binary.BigEndian.PutUint32(pkt[0x38:], b.EighthBeatMS)

	pw := b.RawPitch
	if pw == 0 {
		pw = PitchToWord(b.Pitch)
	}
	binary.BigEndian.PutUint32(pkt[0x54:], pw)

	rawBPM := b.RawBPM
	if rawBPM == 0 && b.BPM > 0 {
		rawBPM = uint16(math.Round(b.BPM * 100))
	}
	binary.BigEndian.PutUint16(pkt[0x5a:], rawBPM)

	pkt[0x5c] = b.BeatWithinBar
	return pkt
}

// ParseBeatPacket parses a UDP 50001 beat packet.
func ParseBeatPacket(b []byte) (*Beat, error) {
	if len(b) < 0x60 {
		return nil, fmt.Errorf("cdj: beat packet too short (%d bytes, expected 0x60)", len(b))
	}
	if !bytes.Equal(b[:10], MagicHeader) || b[10] != TypeBeat {
		return nil, fmt.Errorf("cdj: not a valid beat packet")
	}

	nameEnd := 31
	for i := 11; i < 31; i++ {
		if b[i] == 0 {
			nameEnd = i
			break
		}
	}
	name := string(b[11:nameEnd])

	rawPitch := binary.BigEndian.Uint32(b[0x54:0x58])
	rawBPM := binary.BigEndian.Uint16(b[0x5a:0x5c])

	return &Beat{
		DeviceName:    name,
		PlayerNumber:  b[0x21],
		NextBeatMS:    binary.BigEndian.Uint32(b[0x24:0x28]),
		SecondBeatMS:  binary.BigEndian.Uint32(b[0x28:0x2c]),
		NextBarMS:     binary.BigEndian.Uint32(b[0x2c:0x30]),
		FourthBeatMS:  binary.BigEndian.Uint32(b[0x30:0x34]),
		SecondBarMS:   binary.BigEndian.Uint32(b[0x34:0x38]),
		EighthBeatMS:  binary.BigEndian.Uint32(b[0x38:0x3c]),
		RawPitch:      rawPitch,
		Pitch:         WordToPitch(rawPitch),
		RawBPM:        rawBPM,
		BPM:           float64(rawBPM) / 100.0,
		BeatWithinBar: b[0x5c],
	}, nil
}

// AbsolutePosition packet (UDP 50001, 60 bytes / 0x3c, CDJ-3000)
type AbsolutePosition struct {
	DeviceName     string
	PlayerNumber   byte
	TrackLengthSec uint32
	PlayheadMS     uint32
	HasPlayhead    bool
	Pitch          float64 // percentage, e.g. +3.26%
	EffectiveBPM   float64
}

// NewAbsolutePositionPacket constructs a 60-byte UDP 50001 Absolute Position packet.
func NewAbsolutePositionPacket(ap AbsolutePosition) []byte {
	b := make([]byte, 0x3c)
	copy(b, MagicHeader)
	b[10] = TypeAbsolutePosition

	name := ap.DeviceName
	if name == "" {
		name = "CDJ-3000"
	}
	copy(b[11:31], name)
	b[31] = 0x01
	b[0x20] = 0x00
	b[0x21] = ap.PlayerNumber
	binary.BigEndian.PutUint16(b[0x22:], 0x0018) // 0x3c - 0x24 = 24 bytes

	if ap.TrackLengthSec == 0 && !ap.HasPlayhead {
		binary.BigEndian.PutUint32(b[0x24:], 0xffffffff)
	} else {
		binary.BigEndian.PutUint32(b[0x24:], ap.TrackLengthSec)
	}

	if ap.HasPlayhead {
		binary.BigEndian.PutUint32(b[0x28:], ap.PlayheadMS)
	} else {
		binary.BigEndian.PutUint32(b[0x28:], 0xffffffff)
	}

	rawPitch := int32(math.Round(ap.Pitch * 100))
	binary.BigEndian.PutUint32(b[0x2c:], uint32(rawPitch))

	// 0x30..0x37: 8 zero bytes

	if ap.EffectiveBPM > 0 {
		rawBPM := uint32(math.Round(ap.EffectiveBPM * 10))
		binary.BigEndian.PutUint32(b[0x38:], rawBPM)
	} else {
		binary.BigEndian.PutUint32(b[0x38:], 0xffffffff)
	}

	return b
}

// ParseAbsolutePositionPacket parses a UDP 50001 Absolute Position packet.
func ParseAbsolutePositionPacket(b []byte) (*AbsolutePosition, error) {
	if len(b) < 0x3c {
		return nil, fmt.Errorf("cdj: absolute position packet too short (%d bytes, expected 60)", len(b))
	}
	if !bytes.Equal(b[:10], MagicHeader) || b[10] != TypeAbsolutePosition {
		return nil, fmt.Errorf("cdj: not a valid absolute position packet")
	}

	nameEnd := 31
	for i := 11; i < 31; i++ {
		if b[i] == 0 {
			nameEnd = i
			break
		}
	}
	name := string(b[11:nameEnd])

	trackLen := binary.BigEndian.Uint32(b[0x24:0x28])
	rawPlayhead := binary.BigEndian.Uint32(b[0x28:0x2c])
	hasPlayhead := rawPlayhead != 0xffffffff
	playheadMS := uint32(0)
	if hasPlayhead {
		playheadMS = rawPlayhead
	}

	rawPitch := int32(binary.BigEndian.Uint32(b[0x2c:0x30]))
	pitch := float64(rawPitch) / 100.0

	rawBPM := binary.BigEndian.Uint32(b[0x38:0x3c])
	effBPM := float64(0)
	if rawBPM != 0xffffffff {
		effBPM = float64(rawBPM) / 10.0
	}

	return &AbsolutePosition{
		DeviceName:     name,
		PlayerNumber:   b[0x21],
		TrackLengthSec: trackLen,
		PlayheadMS:     playheadMS,
		HasPlayhead:    hasPlayhead,
		Pitch:          pitch,
		EffectiveBPM:   effBPM,
	}, nil
}

// Media Query packet (UDP 50002, 48 bytes / 0x30)
type MediaQuery struct {
	DeviceName   string
	PlayerNumber byte
	ReplyIP      net.IP
	TargetPlayer byte
	TargetSlot   byte // 0x02 SD, 0x03 USB
}

func NewMediaQueryPacket(q MediaQuery) []byte {
	b := make([]byte, 48)
	copy(b, MagicHeader)
	b[10] = TypeMediaQuery

	name := q.DeviceName
	if name == "" {
		name = "CDJ-3000"
	}
	copy(b[11:31], name)
	b[31] = 0x01
	b[0x20] = 0x00
	b[0x21] = q.PlayerNumber
	binary.BigEndian.PutUint16(b[0x22:], 0x000c)

	if ip4 := q.ReplyIP.To4(); ip4 != nil {
		copy(b[0x24:0x28], ip4)
	}

	b[0x2b] = q.TargetPlayer
	b[0x2f] = q.TargetSlot
	return b
}

func ParseMediaQueryPacket(b []byte) (*MediaQuery, error) {
	if len(b) < 48 {
		return nil, fmt.Errorf("cdj: media query too short (%d bytes, expected 48)", len(b))
	}
	if !bytes.Equal(b[:10], MagicHeader) || b[10] != TypeMediaQuery {
		return nil, fmt.Errorf("cdj: not a valid media query")
	}

	nameEnd := 31
	for i := 11; i < 31; i++ {
		if b[i] == 0 {
			nameEnd = i
			break
		}
	}

	return &MediaQuery{
		DeviceName:   string(b[11:nameEnd]),
		PlayerNumber: b[0x21],
		ReplyIP:      net.IPv4(b[0x24], b[0x25], b[0x26], b[0x27]),
		TargetPlayer: b[0x2b],
		TargetSlot:   b[0x2f],
	}, nil
}

// Media Response packet (UDP 50002, 192 bytes / 0xc0)
type MediaResponse struct {
	DeviceName    string
	PlayerNumber  byte
	TargetPlayer  byte
	TargetSlot    byte
	MediaName     string
	CreatedDate   string
	TrackCount    uint16
	UIColor       byte
	TrackType     byte
	HasSettings   bool
	PlaylistCount uint16
	TotalBytes    uint64
	FreeBytes     uint64
}

func encodeUTF16BE(s string, maxBytes int) []byte {
	runes := []rune(s)
	u16 := utf16.Encode(runes)
	b := make([]byte, maxBytes)
	maxChars := (maxBytes / 2) - 1
	if len(u16) > maxChars {
		u16 = u16[:maxChars]
	}
	for i, c := range u16 {
		binary.BigEndian.PutUint16(b[2*i:], c)
	}
	return b
}

func decodeUTF16BE(b []byte) string {
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = binary.BigEndian.Uint16(b[2*i:])
		if u16[i] == 0 {
			u16 = u16[:i]
			break
		}
	}
	return string(utf16.Decode(u16))
}

func NewMediaResponsePacket(m MediaResponse) []byte {
	b := make([]byte, 192)
	copy(b, MagicHeader)
	b[10] = TypeMediaResponse

	name := m.DeviceName
	if name == "" {
		name = "CDJ-3000"
	}
	copy(b[11:31], name)
	b[31] = 0x01
	b[0x20] = 0x00
	b[0x21] = m.PlayerNumber
	binary.BigEndian.PutUint16(b[0x22:], 0x009c) // 156 bytes

	b[0x27] = m.TargetPlayer
	b[0x2b] = m.TargetSlot

	copy(b[0x2c:0x6c], encodeUTF16BE(m.MediaName, 64))
	copy(b[0x6c:0x88], encodeUTF16BE(m.CreatedDate, 28))
	copy(b[0x88:0xa6], encodeUTF16BE("1000", 30))

	binary.BigEndian.PutUint16(b[0xa6:], m.TrackCount)
	b[0xa8] = m.UIColor
	b[0xaa] = m.TrackType
	if m.HasSettings {
		b[0xab] = 0x01
	}
	binary.BigEndian.PutUint16(b[0xae:], m.PlaylistCount)
	binary.BigEndian.PutUint64(b[0xb0:], m.TotalBytes)
	binary.BigEndian.PutUint64(b[0xb8:], m.FreeBytes)

	return b
}

func ParseMediaResponsePacket(b []byte) (*MediaResponse, error) {
	if len(b) < 192 {
		return nil, fmt.Errorf("cdj: media response too short (%d bytes, expected 192)", len(b))
	}
	if !bytes.Equal(b[:10], MagicHeader) || b[10] != TypeMediaResponse {
		return nil, fmt.Errorf("cdj: not a valid media response")
	}

	nameEnd := 31
	for i := 11; i < 31; i++ {
		if b[i] == 0 {
			nameEnd = i
			break
		}
	}

	return &MediaResponse{
		DeviceName:    string(b[11:nameEnd]),
		PlayerNumber:  b[0x21],
		TargetPlayer:  b[0x27],
		TargetSlot:    b[0x2b],
		MediaName:     decodeUTF16BE(b[0x2c:0x6c]),
		CreatedDate:   decodeUTF16BE(b[0x6c:0x88]),
		TrackCount:    binary.BigEndian.Uint16(b[0xa6:0xa8]),
		UIColor:       b[0xa8],
		TrackType:     b[0xaa],
		HasSettings:   b[0xab] != 0,
		PlaylistCount: binary.BigEndian.Uint16(b[0xae:0xb0]),
		TotalBytes:    binary.BigEndian.Uint64(b[0xb0:0xb8]),
		FreeBytes:     binary.BigEndian.Uint64(b[0xb8:0xc0]),
	}, nil
}
