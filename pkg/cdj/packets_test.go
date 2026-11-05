package cdj

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"testing"
)

// Captured hex fixtures from captured-wire.json (CDJ-2000nexus v1.44)
const (
	hexKeepalive = "5173707431576d4a4f4c060043444a2d323030306e6578757300000000000000010200360202745e1c56ca54a9feca54020000000100"
	hexStatus    = "5173707431576d4a4f4c0a43444a2d323030306e657875730000000000000001040200f802000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000100060400000000000000040101000000000000312e343400000000000000020084fffe000fbc6a7fffffff7fffffff000fbc6a000000ffffffffff01ff0000000000000000000000000000000001000000000000000000000fbc6a000fbc6a000027940f01000012345678000000010101010102010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000050000000000000000"
	hexMedia     = "5173707431576d4a4f4c0643444a2d323030306e6578757300000000000000010002009c000000020000000300530079006d006d00650074007200790000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000032003000310034002d00300036002d003200310000000000310030003000300000000000000000000000000000000000000000000000000000030a00000101000000210000000e66ff00000000000bf7a60000"
	hex0x0       = "11872349ae11fffffffe1000000f01140000000c0600000000000000000000001100000001"
	hex0x1000    = "11872349ae11038001a81010000f03140000000c060606000000000000000000110101030111000000001100ffffff"
	hex0x1105    = "11872349ae11038001aa1011050f04140000000c0606060600000000000000001101020301110000000011000000001100000001"
	hex0x2102    = "11872349ae110380036a1021020f02140000000c060600000000000000000000110108030111000000c8"
	hex0x4000    = "11872349ae11fffffffe1040000f02140000000c06060000000000000000000011000000001100000002"
	hex0x4001    = "11872349ae11038001a91040010f02140000000c06060000000000000000000011000000011100000000"
	hex0x4101    = "11872349ae11038001a91041010f0c140000000c060606020602060606060606110000000011000000051100000016260000000bfffa0050004c00410059004c004900530054fffb0000110000000226000000010000110000008411000000001100000000110000000011000000001100000000"
	hex0x4201    = "11872349ae11038001a91042010f00140000000c000000000000000000000000"
	hexPath      = "11872349ae110380036b1041010f0c140000000c0606060206020606060606061100747a7b11000000c811000000862600000043002f0043006f006e00740065006e00740073002f0054006f006d00630072006100660074002f004c006f006e0065006c0069006e006500730073002f0054006f006d006300720061006600740020002d0020004c006f006e0065006c0069006e0065007300730020002d0020004b006c007500620020004300750074002e006d007000330000110000000226000000010000110000000011000000001100000000110000000011000000001100000000"
)

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex.DecodeString failed: %v", err)
	}
	return b
}

func TestKeepaliveCapturedWire(t *testing.T) {
	raw := mustDecodeHex(t, hexKeepalive)
	k, err := ParseKeepalivePacket(raw)
	if err != nil {
		t.Fatalf("ParseKeepalivePacket failed: %v", err)
	}

	if k.DeviceName != "CDJ-2000nexus" {
		t.Errorf("DeviceName = %q, want CDJ-2000nexus", k.DeviceName)
	}
	if k.PlayerNumber != 2 {
		t.Errorf("PlayerNumber = %d, want 2", k.PlayerNumber)
	}
	if k.FirstDeviceFlag != 0x02 || !k.FirstDevice {
		t.Errorf("FirstDeviceFlag = %d, want 2", k.FirstDeviceFlag)
	}
	if k.MAC.String() != "74:5e:1c:56:ca:54" {
		t.Errorf("MAC = %s, want 74:5e:1c:56:ca:54", k.MAC)
	}
	if k.IP.String() != "169.254.202.84" {
		t.Errorf("IP = %s, want 169.254.202.84", k.IP)
	}
	if k.PeerCount != 2 {
		t.Errorf("PeerCount = %d, want 2", k.PeerCount)
	}
	if k.DeviceClass != 1 {
		t.Errorf("DeviceClass = %d, want 1 (CDJ)", k.DeviceClass)
	}
	if k.ModelCapability != 0 {
		t.Errorf("ModelCapability = %d, want 0", k.ModelCapability)
	}

	// Rebuild and verify equality
	rebuilt := NewKeepalivePacket(*k)
	if hex.EncodeToString(rebuilt) != hexKeepalive {
		t.Errorf("Rebuilt keepalive != captured wire:\ngot  %s\nwant %s", hex.EncodeToString(rebuilt), hexKeepalive)
	}
}

func TestStatusCapturedWire(t *testing.T) {
	raw := mustDecodeHex(t, hexStatus)
	st, err := ParseStatusPacket(raw)
	if err != nil {
		t.Fatalf("ParseStatusPacket failed: %v", err)
	}

	if st.DeviceName != "CDJ-2000nexus" {
		t.Errorf("DeviceName = %q, want CDJ-2000nexus", st.DeviceName)
	}
	if st.PlayerNumber != 2 {
		t.Errorf("PlayerNumber = %d, want 2", st.PlayerNumber)
	}
	if st.PacketVersion != 4 {
		t.Errorf("PacketVersion = %d, want 4", st.PacketVersion)
	}
	if st.Firmware != "1.44" {
		t.Errorf("Firmware = %q, want 1.44", st.Firmware)
	}
	if !st.USBPresent || st.USBLocalState != 0 {
		t.Errorf("USBPresent = %v, USBLocalState = %d, want loaded (0)", st.USBPresent, st.USBLocalState)
	}
	if st.SDPresent || st.SDLocalState != 4 {
		t.Errorf("SDPresent = %v, SDLocalState = %d, want empty (4)", st.SDPresent, st.SDLocalState)
	}
	if !st.LinkAvailable {
		t.Errorf("LinkAvailable = false, want true")
	}
	if st.Playing {
		t.Errorf("Playing = true, want false (idle player)")
	}
	if st.BPMSync {
		t.Errorf("BPMSync = true, want false (idle player)")
	}
}

func TestMediaCapturedWire(t *testing.T) {
	raw := mustDecodeHex(t, hexMedia)
	m, err := ParseMediaResponsePacket(raw)
	if err != nil {
		t.Fatalf("ParseMediaResponsePacket failed: %v", err)
	}

	if m.DeviceName != "CDJ-2000nexus" {
		t.Errorf("DeviceName = %q, want CDJ-2000nexus", m.DeviceName)
	}
	if m.PlayerNumber != 2 {
		t.Errorf("PlayerNumber = %d, want 2", m.PlayerNumber)
	}
	if m.TargetPlayer != 2 {
		t.Errorf("TargetPlayer = %d, want 2", m.TargetPlayer)
	}
	if m.TargetSlot != SlotUSB {
		t.Errorf("TargetSlot = %d, want USB (3)", m.TargetSlot)
	}
	if m.MediaName != "Symmetry" {
		t.Errorf("MediaName = %q, want Symmetry", m.MediaName)
	}
	if m.CreatedDate != "2014-06-21" {
		t.Errorf("CreatedDate = %q, want 2014-06-21", m.CreatedDate)
	}
	if m.TrackCount != 778 {
		t.Errorf("TrackCount = %d, want 778", m.TrackCount)
	}
	if m.PlaylistCount != 33 {
		t.Errorf("PlaylistCount = %d, want 33", m.PlaylistCount)
	}
	if m.TrackType != TrackTypeRekordbox {
		t.Errorf("TrackType = %d, want rekordbox (1)", m.TrackType)
	}
	if !m.HasSettings {
		t.Errorf("HasSettings = false, want true")
	}
	if m.TotalBytes != 61857529856 {
		t.Errorf("TotalBytes = %d, want 61857529856", m.TotalBytes)
	}
	if m.FreeBytes != 51399491584 {
		t.Errorf("FreeBytes = %d, want 51399491584", m.FreeBytes)
	}
}

func TestDBServerCapturedWire(t *testing.T) {
	// 0x0 setup query
	msg0, err := DecodeDBMessageFromBytes(mustDecodeHex(t, hex0x0))
	if err != nil {
		t.Fatalf("DecodeDBMessage 0x0 failed: %v", err)
	}
	if msg0.Tx != 0xfffffffe || msg0.Kind != KindSetup || len(msg0.Args) != 1 || msg0.Args[0].Num != 1 {
		t.Errorf("0x0 mismatch: %+v", msg0)
	}

	// 0x4000 setup ack
	msg4000, err := DecodeDBMessageFromBytes(mustDecodeHex(t, hex0x4000))
	if err != nil {
		t.Fatalf("DecodeDBMessage 0x4000 failed: %v", err)
	}
	if msg4000.Tx != 0xfffffffe || msg4000.Kind != KindSetupAck || len(msg4000.Args) != 2 || msg4000.Args[1].Num != 2 {
		t.Errorf("0x4000 mismatch: %+v", msg4000)
	}

	// 0x1000 root menu query
	msg1000, err := DecodeDBMessageFromBytes(mustDecodeHex(t, hex0x1000))
	if err != nil {
		t.Fatalf("DecodeDBMessage 0x1000 failed: %v", err)
	}
	if msg1000.Kind != KindRootMenu || len(msg1000.Args) != 3 || msg1000.Args[0].Num != 0x01010301 {
		t.Errorf("0x1000 mismatch: %+v", msg1000)
	}

	// 0x1105 playlist query
	msg1105, err := DecodeDBMessageFromBytes(mustDecodeHex(t, hex0x1105))
	if err != nil {
		t.Fatalf("DecodeDBMessage 0x1105 failed: %v", err)
	}
	if msg1105.Kind != KindPlaylist || len(msg1105.Args) != 4 || msg1105.Args[3].Num != 1 {
		t.Errorf("0x1105 mismatch: %+v", msg1105)
	}

	// 0x2102 track info query
	msg2102, err := DecodeDBMessageFromBytes(mustDecodeHex(t, hex0x2102))
	if err != nil {
		t.Fatalf("DecodeDBMessage 0x2102 failed: %v", err)
	}
	if msg2102.Kind != KindTrackInfo || len(msg2102.Args) != 2 || msg2102.Args[1].Num != 200 {
		t.Errorf("0x2102 mismatch: %+v", msg2102)
	}

	// 0x4001 render header
	msg4001, err := DecodeDBMessageFromBytes(mustDecodeHex(t, hex0x4001))
	if err != nil {
		t.Fatalf("DecodeDBMessage 0x4001 failed: %v", err)
	}
	if msg4001.Kind != KindRenderHeader || len(msg4001.Args) != 2 || msg4001.Args[0].Num != 1 {
		t.Errorf("0x4001 mismatch: %+v", msg4001)
	}

	// 0x4101 playlist item
	msg4101, err := DecodeDBMessageFromBytes(mustDecodeHex(t, hex0x4101))
	if err != nil {
		t.Fatalf("DecodeDBMessage 0x4101 failed: %v", err)
	}
	item, err := ParseMenuItem(msg4101)
	if err != nil {
		t.Fatalf("ParseMenuItem 0x4101 failed: %v", err)
	}
	if item.ID != 5 || item.Label != "PLAYLIST" || item.Kind != 132 {
		t.Errorf("0x4101 item parsed = %+v, want ID=5, Label=PLAYLIST, Kind=132", item)
	}

	// 0x4201 render footer
	msg4201, err := DecodeDBMessageFromBytes(mustDecodeHex(t, hex0x4201))
	if err != nil {
		t.Fatalf("DecodeDBMessage 0x4201 failed: %v", err)
	}
	if msg4201.Kind != KindRenderFooter || len(msg4201.Args) != 0 {
		t.Errorf("0x4201 mismatch: %+v", msg4201)
	}

	// path item
	msgPath, err := DecodeDBMessageFromBytes(mustDecodeHex(t, hexPath))
	if err != nil {
		t.Fatalf("DecodeDBMessage path failed: %v", err)
	}
	pathItem, err := ParseMenuItem(msgPath)
	if err != nil {
		t.Fatalf("ParseMenuItem path failed: %v", err)
	}
	wantPath := "/Contents/Tomcraft/Loneliness/Tomcraft - Loneliness - Klub Cut.mp3"
	if pathItem.ID != 200 || pathItem.Label != wantPath {
		t.Errorf("path item parsed = %+v, want ID=200, Label=%q", pathItem, wantPath)
	}
}

func TestCDJ3000StatusRoundtrip(t *testing.T) {
	orig := Status{
		DeviceName:        "CDJ-3000",
		PlayerNumber:      1,
		PacketVersion:     0x06,
		Active:            true,
		TrackSourcePlayer: 1,
		TrackSourceSlot:   SlotUSB,
		TrackType:         TrackTypeRekordbox,
		TrackID:           12345,
		TrackIndex:        3,
		USBPresent:        true,
		SDPresent:         false,
		USBLocalState:     0,
		SDLocalState:      4,
		LinkAvailable:     true,
		PlayState1:        PlayState1Playing,
		Firmware:          "3.20",
		Playing:           true,
		Master:            true,
		Sync:              true,
		OnAir:             true,
		BPMSync:           false,
		Pitch:             3.125,
		BPM:               126.0,
		Beat:              17,
		BeatWithinBar:     1,
		WaveformColor:     4, // 3-band
		MasterTempo:       true,
		BufferFull:        true,
	}

	pkt := NewCDJ3000StatusPacket(orig)
	if len(pkt) != 512 {
		t.Fatalf("CDJ-3000 status packet size = %d, want 512", len(pkt))
	}

	parsed, err := ParseStatusPacket(pkt)
	if err != nil {
		t.Fatalf("ParseStatusPacket failed: %v", err)
	}

	if parsed.PlayerNumber != orig.PlayerNumber {
		t.Errorf("PlayerNumber = %d, want %d", parsed.PlayerNumber, orig.PlayerNumber)
	}
	if parsed.TrackID != orig.TrackID {
		t.Errorf("TrackID = %d, want %d", parsed.TrackID, orig.TrackID)
	}
	if !parsed.Playing || !parsed.Master || !parsed.Sync || !parsed.OnAir {
		t.Errorf("Flags mismatch: Playing=%v, Master=%v, Sync=%v, OnAir=%v", parsed.Playing, parsed.Master, parsed.Sync, parsed.OnAir)
	}
	if !parsed.USBPresent || parsed.USBLocalState != 0 {
		t.Errorf("USBPresent=%v, USBLocalState=%d, want 0", parsed.USBPresent, parsed.USBLocalState)
	}
	if parsed.SDPresent || parsed.SDLocalState != 4 {
		t.Errorf("SDPresent=%v, SDLocalState=%d, want 4", parsed.SDPresent, parsed.SDLocalState)
	}
	if parsed.MediaPresence != 0x02 { // bit 1 USB
		t.Errorf("MediaPresence = 0x%02x, want 0x02", parsed.MediaPresence)
	}
	if parsed.Firmware != "3.20" {
		t.Errorf("Firmware = %q, want 3.20", parsed.Firmware)
	}
	if parsed.BPM != 126.0 {
		t.Errorf("BPM = %f, want 126.0", parsed.BPM)
	}
	if parsed.WaveformColor != 4 {
		t.Errorf("WaveformColor = %d, want 4", parsed.WaveformColor)
	}
	if !parsed.MasterTempo {
		t.Errorf("MasterTempo = false, want true")
	}
	if !parsed.BufferFull {
		t.Errorf("BufferFull = false, want true")
	}
}

func TestCDJ2000NXS2StatusRoundtrip(t *testing.T) {
	orig := Status{
		DeviceName:        "CDJ-2000NXS2",
		PlayerNumber:      3,
		PacketVersion:     0x04,
		TrackSourcePlayer: 3,
		TrackSourceSlot:   SlotUSB,
		TrackType:         TrackTypeRekordbox,
		TrackID:           999,
		USBPresent:        true,
		SDPresent:         true,
		USBLocalState:     0,
		SDLocalState:      0,
		LinkAvailable:     true,
		PlayState1:        PlayState1Paused,
		Firmware:          "1.85",
		Playing:           false,
		Master:            false,
		BPM:               120.0,
		Beat:              1,
		BeatWithinBar:     1,
		WaveformColor:     3, // RGB
	}

	pkt := NewCDJ2000NXS2StatusPacket(orig)
	if len(pkt) != 284 {
		t.Fatalf("CDJ-2000NXS2 packet size = %d, want 284", len(pkt))
	}

	parsed, err := ParseStatusPacket(pkt)
	if err != nil {
		t.Fatalf("ParseStatusPacket failed: %v", err)
	}
	if parsed.PlayerNumber != 3 || parsed.TrackID != 999 || parsed.USBPresent != true || parsed.SDPresent != true {
		t.Errorf("CDJ-2000NXS2 parsed mismatch: %+v", parsed)
	}
	if parsed.WaveformColor != 3 {
		t.Errorf("WaveformColor = %d, want 3", parsed.WaveformColor)
	}
}

func TestBeatPacketRoundtrip(t *testing.T) {
	orig := Beat{
		DeviceName:    "CDJ-3000",
		PlayerNumber:  2,
		NextBeatMS:    500,
		SecondBeatMS:  1000,
		NextBarMS:     1500,
		FourthBeatMS:  2000,
		SecondBarMS:   3500,
		EighthBeatMS:  4000,
		Pitch:         0.0,
		BPM:           120.0,
		BeatWithinBar: 2,
	}

	pkt := NewBeatPacket(orig)
	if len(pkt) != 96 {
		t.Fatalf("Beat packet size = %d, want 96", len(pkt))
	}

	parsed, err := ParseBeatPacket(pkt)
	if err != nil {
		t.Fatalf("ParseBeatPacket failed: %v", err)
	}
	if parsed.PlayerNumber != 2 || parsed.NextBeatMS != 500 || parsed.BPM != 120.0 || parsed.BeatWithinBar != 2 {
		t.Errorf("Beat packet parsed mismatch: %+v", parsed)
	}
}

func TestMediaQueryResponseRoundtrip(t *testing.T) {
	q := MediaQuery{
		DeviceName:   "CDJ-3000",
		PlayerNumber: 4,
		ReplyIP:      net.IPv4(192, 168, 1, 50),
		TargetPlayer: 1,
		TargetSlot:   SlotUSB,
	}
	qPkt := NewMediaQueryPacket(q)
	parsedQ, err := ParseMediaQueryPacket(qPkt)
	if err != nil {
		t.Fatalf("ParseMediaQueryPacket failed: %v", err)
	}
	if parsedQ.PlayerNumber != 4 || parsedQ.ReplyIP.String() != "192.168.1.50" || parsedQ.TargetPlayer != 1 || parsedQ.TargetSlot != SlotUSB {
		t.Errorf("MediaQuery mismatch: %+v", parsedQ)
	}

	resp := MediaResponse{
		DeviceName:    "CDJ-3000",
		PlayerNumber:  1,
		TargetPlayer:  1,
		TargetSlot:    SlotUSB,
		MediaName:     "MY_USB_STICK",
		CreatedDate:   "2026-11-05",
		TrackCount:    150,
		PlaylistCount: 12,
		UIColor:       6, // Aqua
		TrackType:     TrackTypeRekordbox,
		HasSettings:   true,
		TotalBytes:    32000000000,
		FreeBytes:     16000000000,
	}
	rPkt := NewMediaResponsePacket(resp)
	parsedR, err := ParseMediaResponsePacket(rPkt)
	if err != nil {
		t.Fatalf("ParseMediaResponsePacket failed: %v", err)
	}
	if parsedR.MediaName != "MY_USB_STICK" || parsedR.CreatedDate != "2026-11-05" || parsedR.TrackCount != 150 || parsedR.UIColor != 6 || parsedR.TotalBytes != 32000000000 {
		t.Errorf("MediaResponse mismatch: %+v", parsedR)
	}
}

func TestANLZParsing(t *testing.T) {
	// Construct minimal PMAI with PQTZ, PWV7, and PPTH
	buf := make([]byte, 28) // Header: PMAI + len_hdr(28) + len_file(total)
	copy(buf[:4], "PMAI")

	// PQTZ section (24-byte header + 2 * 8-byte entries = 40 bytes)
	pqtz := make([]byte, 40)
	copy(pqtz[:4], "PQTZ")
	binary.BigEndian.PutUint32(pqtz[4:8], 24)        // hdrLen
	binary.BigEndian.PutUint32(pqtz[8:12], 40)       // tagLen
	binary.BigEndian.PutUint32(pqtz[12:16], 0)       // padding
	binary.BigEndian.PutUint32(pqtz[16:20], 0x80000) // u2
	binary.BigEndian.PutUint32(pqtz[20:24], 2)       // num_beats = 2
	// Beat 1: beat=1, tempo=12000 (120.0 BPM), time=0
	binary.BigEndian.PutUint16(pqtz[24:26], 1)
	binary.BigEndian.PutUint16(pqtz[26:28], 12000)
	binary.BigEndian.PutUint32(pqtz[28:32], 0)
	// Beat 2: beat=2, tempo=12000, time=500
	binary.BigEndian.PutUint16(pqtz[32:34], 2)
	binary.BigEndian.PutUint16(pqtz[34:36], 12000)
	binary.BigEndian.PutUint32(pqtz[36:40], 500)

	// PWV7 section (24-byte header + 2 * 3-byte entries = 30 bytes)
	pwv7 := make([]byte, 30)
	copy(pwv7[:4], "PWV7")
	binary.BigEndian.PutUint32(pwv7[4:8], 24)
	binary.BigEndian.PutUint32(pwv7[8:12], 30)
	binary.BigEndian.PutUint32(pwv7[12:16], 3) // entry_bytes
	binary.BigEndian.PutUint32(pwv7[16:20], 2) // len_entries
	binary.BigEndian.PutUint32(pwv7[20:24], 0x960000)
	copy(pwv7[24:27], []byte{10, 20, 30})
	copy(pwv7[27:30], []byte{15, 25, 5})

	total := append(buf, pqtz...)
	total = append(total, pwv7...)

	binary.BigEndian.PutUint32(total[4:8], 28)
	binary.BigEndian.PutUint32(total[8:12], uint32(len(total)))

	anlz, err := ParseANLZ(total)
	if err != nil {
		t.Fatalf("ParseANLZ failed: %v", err)
	}

	if len(anlz.Beats) != 2 {
		t.Fatalf("Beats count = %d, want 2", len(anlz.Beats))
	}
	if anlz.Beats[0].Beat != 1 || anlz.Beats[0].BPM != 120.0 || anlz.Beats[1].TimeMS != 500 {
		t.Errorf("Beat points mismatch: %+v", anlz.Beats)
	}

	if len(anlz.WaveformCols) != 2 {
		t.Fatalf("Waveform cols count = %d, want 2", len(anlz.WaveformCols))
	}
	if anlz.WaveformCols[0] != [3]byte{10, 20, 30} || anlz.WaveformCols[1] != [3]byte{15, 25, 5} {
		t.Errorf("Waveform cols mismatch: %+v", anlz.WaveformCols)
	}
}

// TestANLZRealFiles verifies against real Rekordbox samples on the remote build machine if available.
func TestANLZRealFiles(t *testing.T) {
	sampleDir := "/root/build/sc5000-lab/deep-symmetry-archive/pyrekordbox/.testdata/export/PIONEER/USBANLZ"
	if _, err := os.Stat(sampleDir); err != nil {
		t.Skip("sample ANLZ directory not available locally")
	}

	// Track with analyzed beat grid
	datPath := sampleDir + "/P053/0001D21F/ANLZ0000.DAT"
	datBytes, err := os.ReadFile(datPath)
	if err == nil {
		anlz, err := ParseANLZ(datBytes)
		if err != nil {
			t.Errorf("ParseANLZ on %s failed: %v", datPath, err)
		} else {
			if len(anlz.Beats) != 257 {
				t.Errorf("Expected 257 beats in %s, got %d", datPath, len(anlz.Beats))
			}
			t.Logf("Successfully parsed real DAT file %s: %d beats, path: %q", datPath, len(anlz.Beats), anlz.Path)
		}
	}

	twoExPath := sampleDir + "/P017/00009B77/ANLZ0000.2EX"
	twoExBytes, err := os.ReadFile(twoExPath)
	if err == nil {
		anlz, err := ParseANLZ(twoExBytes)
		if err != nil {
			t.Errorf("ParseANLZ on %s failed: %v", twoExPath, err)
		} else {
			if len(anlz.WaveformCols) == 0 {
				t.Errorf("Expected 3-band waveform in %s, got 0 columns", twoExPath)
			}
			t.Logf("Successfully parsed real 2EX file %s: %d waveform columns", twoExPath, len(anlz.WaveformCols))
		}
	}
}

func TestAbsolutePositionRoundtrip(t *testing.T) {
	// Case 1: Active track with positive pitch
	ap1 := AbsolutePosition{
		DeviceName:     "CDJ-3000",
		PlayerNumber:   1,
		TrackLengthSec: 240,
		PlayheadMS:     45120,
		HasPlayhead:    true,
		Pitch:          3.26,
		EffectiveBPM:   123.9,
	}

	pkt1 := NewAbsolutePositionPacket(ap1)
	if len(pkt1) != 60 {
		t.Fatalf("AbsolutePosition packet length = %d, want 60", len(pkt1))
	}

	parsed1, err := ParseAbsolutePositionPacket(pkt1)
	if err != nil {
		t.Fatalf("ParseAbsolutePositionPacket failed: %v", err)
	}
	if parsed1.PlayerNumber != 1 {
		t.Errorf("PlayerNumber = %d, want 1", parsed1.PlayerNumber)
	}
	if parsed1.TrackLengthSec != 240 {
		t.Errorf("TrackLengthSec = %d, want 240", parsed1.TrackLengthSec)
	}
	if !parsed1.HasPlayhead || parsed1.PlayheadMS != 45120 {
		t.Errorf("PlayheadMS = %d, HasPlayhead = %v, want 45120 / true", parsed1.PlayheadMS, parsed1.HasPlayhead)
	}
	if parsed1.Pitch != 3.26 {
		t.Errorf("Pitch = %f, want 3.26", parsed1.Pitch)
	}
	if parsed1.EffectiveBPM != 123.9 {
		t.Errorf("EffectiveBPM = %f, want 123.9", parsed1.EffectiveBPM)
	}

	// Case 2: Active track with negative pitch
	ap2 := AbsolutePosition{
		DeviceName:     "CDJ-3000",
		PlayerNumber:   2,
		TrackLengthSec: 180,
		PlayheadMS:     12500,
		HasPlayhead:    true,
		Pitch:          -2.50,
		EffectiveBPM:   117.0,
	}
	pkt2 := NewAbsolutePositionPacket(ap2)
	parsed2, err := ParseAbsolutePositionPacket(pkt2)
	if err != nil {
		t.Fatalf("ParseAbsolutePositionPacket (neg pitch) failed: %v", err)
	}
	if parsed2.Pitch != -2.50 {
		t.Errorf("Negative Pitch = %f, want -2.50", parsed2.Pitch)
	}
	if parsed2.EffectiveBPM != 117.0 {
		t.Errorf("EffectiveBPM = %f, want 117.0", parsed2.EffectiveBPM)
	}

	// Case 3: No track loaded / invalid playhead
	ap3 := AbsolutePosition{
		DeviceName:     "CDJ-3000",
		PlayerNumber:   3,
		TrackLengthSec: 0,
		PlayheadMS:     0,
		HasPlayhead:    false,
		Pitch:          0.0,
		EffectiveBPM:   0.0,
	}
	pkt3 := NewAbsolutePositionPacket(ap3)
	// Wire checks: 0x24..0x27, 0x28..0x2b, 0x38..0x3b should be 0xffffffff
	if binary.BigEndian.Uint32(pkt3[0x24:0x28]) != 0xffffffff {
		t.Errorf("TrackLength on wire = 0x%08x, want 0xffffffff", binary.BigEndian.Uint32(pkt3[0x24:0x28]))
	}
	if binary.BigEndian.Uint32(pkt3[0x28:0x2c]) != 0xffffffff {
		t.Errorf("Playhead on wire = 0x%08x, want 0xffffffff", binary.BigEndian.Uint32(pkt3[0x28:0x2c]))
	}
	if binary.BigEndian.Uint32(pkt3[0x38:0x3c]) != 0xffffffff {
		t.Errorf("EffectiveBPM on wire = 0x%08x, want 0xffffffff", binary.BigEndian.Uint32(pkt3[0x38:0x3c]))
	}

	parsed3, err := ParseAbsolutePositionPacket(pkt3)
	if err != nil {
		t.Fatalf("ParseAbsolutePositionPacket (no track) failed: %v", err)
	}
	if parsed3.HasPlayhead {
		t.Errorf("HasPlayhead = true, want false")
	}
	if parsed3.EffectiveBPM != 0 {
		t.Errorf("EffectiveBPM = %f, want 0", parsed3.EffectiveBPM)
	}
}
