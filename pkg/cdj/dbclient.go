package cdj

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// Magic byte sequence that prefixes all dbserver messages.
var DBServerMagic = []byte{0x11, 0x87, 0x23, 0x49, 0xae}

// Known dbserver transaction / message kinds
const (
	KindSetup        uint16 = 0x0000
	KindRootMenu     uint16 = 0x1000
	KindAllTracks    uint16 = 0x1004
	KindPlaylist     uint16 = 0x1105
	KindTrackInfo    uint16 = 0x2102
	KindRender       uint16 = 0x3000
	KindSetupAck     uint16 = 0x4000
	KindRenderHeader uint16 = 0x4001
	KindRenderItem   uint16 = 0x4101
	KindRenderFooter uint16 = 0x4201
	KindUnavailable  uint16 = 0x4003
)

// DBServer argument tag types
const (
	TagNone   byte = 0x00
	TagString byte = 0x02
	TagBlob   byte = 0x03
	TagNumber byte = 0x06
)

// BuildDMST encodes the 4-byte DMST routing parameter:
// (player << 24) | (menuLocation << 16) | (slot << 8) | trackType
func BuildDMST(player byte, menuLoc byte, slot byte, trackType byte) uint32 {
	return uint32(player)<<24 | uint32(menuLoc)<<16 | uint32(slot)<<8 | uint32(trackType)
}

// DBField represents a single typed parameter in a dbserver message.
type DBField struct {
	Tag  byte
	Num  uint32
	Str  string
	Blob []byte
}

// DBMessage represents a decoded or encodable dbserver packet.
type DBMessage struct {
	Tx   uint32
	Kind uint16
	Args []DBField
}

// EncodeDBRequest constructs a standard numeric-argument dbserver request.
func EncodeDBRequest(tx uint32, kind uint16, args ...uint32) []byte {
	b := make([]byte, 0, 32+5*len(args))
	b = append(b, DBServerMagic...)
	b = append(b, 0x11)
	b = binary.BigEndian.AppendUint32(b, tx)
	b = append(b, 0x10, byte(kind>>8), byte(kind))
	b = append(b, 0x0f, byte(len(args)))
	b = append(b, 0x14, 0, 0, 0, 12)

	tags := make([]byte, 12)
	for i := range args {
		if i < 12 {
			tags[i] = TagNumber
		}
	}
	b = append(b, tags...)

	for _, a := range args {
		b = append(b, 0x11)
		b = binary.BigEndian.AppendUint32(b, a)
	}
	return b
}

// EncodeDBMessage serializes a full DBMessage into wire bytes.
func EncodeDBMessage(msg DBMessage) []byte {
	b := make([]byte, 0, 64)
	b = append(b, DBServerMagic...)
	b = append(b, 0x11)
	b = binary.BigEndian.AppendUint32(b, msg.Tx)
	b = append(b, 0x10, byte(msg.Kind>>8), byte(msg.Kind))
	b = append(b, 0x0f, byte(len(msg.Args)))
	b = append(b, 0x14, 0, 0, 0, 12)

	tags := make([]byte, 12)
	for i, f := range msg.Args {
		if i < 12 {
			tags[i] = f.Tag
		}
	}
	b = append(b, tags...)

	for _, f := range msg.Args {
		switch f.Tag {
		case TagNumber:
			b = append(b, 0x11)
			b = binary.BigEndian.AppendUint32(b, f.Num)
		case TagString:
			runes := []rune(f.Str)
			u16 := utf16.Encode(runes)
			codeUnits := uint32(len(u16) + 1) // include NUL
			b = append(b, 0x26)
			b = binary.BigEndian.AppendUint32(b, codeUnits)
			for _, cu := range u16 {
				b = binary.BigEndian.AppendUint16(b, cu)
			}
			b = binary.BigEndian.AppendUint16(b, 0x0000) // NUL
		case TagBlob:
			b = append(b, 0x14)
			b = binary.BigEndian.AppendUint32(b, uint32(len(f.Blob)))
			b = append(b, f.Blob...)
		default:
			b = append(b, 0x11)
			b = binary.BigEndian.AppendUint32(b, f.Num)
		}
	}
	return b
}

// DecodeDBMessage parses a single dbserver message from an io.Reader.
func DecodeDBMessage(r io.Reader) (*DBMessage, error) {
	hdr := make([]byte, 32)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	return DecodeDBMessageFromBytes(hdr, r)
}

// DecodeDBMessageFromBytes parses a message from a slice, reading remaining field bodies from reader if needed.
func DecodeDBMessageFromBytes(b []byte, r ...io.Reader) (*DBMessage, error) {
	if len(b) < 32 {
		return nil, fmt.Errorf("dbserver: packet too short (%d bytes, min 32)", len(b))
	}
	if !bytes.Equal(b[:5], DBServerMagic) {
		return nil, fmt.Errorf("dbserver: invalid magic header %x", b[:5])
	}
	if b[5] != 0x11 {
		return nil, fmt.Errorf("dbserver: unexpected tx marker %x", b[5])
	}

	tx := binary.BigEndian.Uint32(b[6:10])
	if b[10] != 0x10 {
		return nil, fmt.Errorf("dbserver: unexpected kind marker %x", b[10])
	}
	kind := binary.BigEndian.Uint16(b[11:13])

	if b[13] != 0x0f {
		return nil, fmt.Errorf("dbserver: unexpected argc marker %x", b[13])
	}
	argc := int(b[14])
	if argc > 12 {
		return nil, fmt.Errorf("dbserver: invalid argument count %d > 12", argc)
	}

	// Tags descriptor at 15: 0x14 00 00 00 0c + 12 tag bytes
	tags := make([]byte, 12)
	if len(b) >= 32 {
		copy(tags, b[20:32])
	}

	msg := &DBMessage{
		Tx:   tx,
		Kind: kind,
		Args: make([]DBField, 0, argc),
	}

	offset := 32
	var reader io.Reader
	if len(r) > 0 && r[0] != nil {
		reader = r[0]
	} else {
		reader = bytes.NewReader(b[offset:])
		offset = len(b)
	}

	for i := 0; i < argc; i++ {
		tag := tags[i]
		markerBuf := make([]byte, 5)
		if _, err := io.ReadFull(reader, markerBuf); err != nil {
			return nil, fmt.Errorf("dbserver: failed to read field %d marker: %w", i, err)
		}

		fieldTag := markerBuf[0]
		fieldVal := binary.BigEndian.Uint32(markerBuf[1:5])

		field := DBField{Tag: tag, Num: fieldVal}
		switch fieldTag {
		case 0x11: // Number
			field.Tag = TagNumber
		case 0x26: // UTF-16BE String
			field.Tag = TagString
			codeUnits := fieldVal
			if codeUnits > 65536 {
				return nil, fmt.Errorf("dbserver: unreasonable string length %d", codeUnits)
			}
			if codeUnits > 0 {
				raw := make([]byte, 2*codeUnits)
				if _, err := io.ReadFull(reader, raw); err != nil {
					return nil, fmt.Errorf("dbserver: failed to read string body: %w", err)
				}
				u16 := make([]uint16, codeUnits)
				for j := range u16 {
					u16[j] = binary.BigEndian.Uint16(raw[2*j : 2*j+2])
				}
				// Strip trailing NUL
				if len(u16) > 0 && u16[len(u16)-1] == 0 {
					u16 = u16[:len(u16)-1]
				}
				field.Str = string(utf16.Decode(u16))
			}
		case 0x14: // Blob
			field.Tag = TagBlob
			blobLen := fieldVal
			if blobLen > 1<<24 {
				return nil, fmt.Errorf("dbserver: unreasonable blob length %d", blobLen)
			}
			blob := make([]byte, blobLen)
			if _, err := io.ReadFull(reader, blob); err != nil {
				return nil, fmt.Errorf("dbserver: failed to read blob body: %w", err)
			}
			field.Blob = blob
		default:
			// Treat as number or raw
		}
		msg.Args = append(msg.Args, field)
	}

	return msg, nil
}

// MenuItem represents a rendered browse item from a 0x4101 response.
type MenuItem struct {
	Parent    uint32
	ID        uint32
	Label     string
	Secondary string
	Kind      uint32
	Position  uint32
}

// ParseMenuItem extracts a MenuItem from a 0x4101 render response.
func ParseMenuItem(msg *DBMessage) (*MenuItem, error) {
	if msg.Kind != KindRenderItem {
		return nil, fmt.Errorf("dbserver: expected 0x4101 item, got 0x%04x", msg.Kind)
	}
	if len(msg.Args) < 7 {
		return nil, fmt.Errorf("dbserver: 0x4101 item has only %d arguments (expected >= 7)", len(msg.Args))
	}

	label := msg.Args[3].Str
	// Strip root item wrapping markers U+FFFA and U+FFFB
	label = strings.TrimPrefix(label, "￺")
	label = strings.TrimSuffix(label, "￻")

	sec := ""
	if len(msg.Args) > 5 {
		sec = msg.Args[5].Str
		sec = strings.TrimPrefix(sec, "￺")
		sec = strings.TrimSuffix(sec, "￻")
	}

	pos := uint32(0)
	if len(msg.Args) > 9 {
		pos = msg.Args[9].Num
	}

	return &MenuItem{
		Parent:    msg.Args[0].Num,
		ID:        msg.Args[1].Num,
		Label:     label,
		Secondary: sec,
		Kind:      msg.Args[6].Num,
		Position:  pos,
	}, nil
}
