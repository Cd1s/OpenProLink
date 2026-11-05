package prolink

// Pioneer players export their SD and USB slots read-only over NFSv2 on UDP.
// This is the client side Deep Symmetry's crate-digger uses: portmapper (111)
// -> MOUNT v1 -> NFS v2 LOOKUP/READ, with path names in UTF-16LE. Replies come
// from any host on the LAN, so every field is bounds-checked and file sizes
// are capped. Read-only: no NFS procedure here changes anything on the player.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	rpcPortmap = 100000
	rpcMount   = 100005
	rpcNFS     = 100003
	nfsChunk   = 8192 // NFSv2's maximum READ size
	fhSize     = 32
)

// SlotExport is the NFS export of a status packet's media slot (2 = SD, 3 = USB).
func SlotExport(slot byte) (string, error) {
	switch slot {
	case 2:
		return "/B/", nil
	case 3:
		return "/C/", nil
	}
	return "", fmt.Errorf("prolink: slot %d has no NFS export", slot)
}

// NFSFile is what LOOKUP reports about a file.
type NFSFile struct {
	Size  uint32
	MTime uint32 // seconds
}

// NFSClient reads files from one player. The zero Portmap means UDP 111.
type NFSClient struct {
	IP      net.IP
	Portmap int
	Timeout time.Duration // per RPC attempt; default 1 s, three attempts
	ports   map[uint32]int
}

// pioneerName encodes a path the way the players expect it.
func pioneerName(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return b
}

type xdr struct{ bytes.Buffer }

func (x *xdr) u32(v uint32) *xdr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	x.Write(b[:])
	return x
}

func (x *xdr) opaque(b []byte) *xdr {
	x.u32(uint32(len(b)))
	x.Write(b)
	x.Write(make([]byte, (4-len(b)%4)%4))
	return x
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) u32() uint32 {
	if r.err != nil || len(r.b) < 4 {
		r.err = errors.New("prolink: short NFS reply")
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) fixed(n int) []byte {
	if r.err != nil || n < 0 || len(r.b) < n {
		r.err = errors.New("prolink: short NFS reply")
		return nil
	}
	v := r.b[:n]
	r.b = r.b[min((n+3)&^3, len(r.b)):]
	return v
}

func (r *reader) opaque(max int) []byte {
	n := r.u32()
	if r.err == nil && (max < 0 || uint64(n) > uint64(max) || uint64(n)+3 > uint64(^uint(0)>>1)) {
		r.err = errors.New("prolink: oversized NFS field")
	}
	if r.err != nil {
		return nil
	}
	pad := (int(n) + 3) &^ 3
	if len(r.b) < pad {
		if len(r.b) < int(n) {
			r.err = errors.New("prolink: short NFS reply")
			return nil
		}
		pad = int(n) // tolerate a missing pad on the last field
	}
	v := r.b[:n]
	r.b = r.b[pad:]
	return v
}

func (c *NFSClient) call(ctx context.Context, port int, prog, vers, proc uint32, args []byte) (*reader, error) {
	var id [4]byte
	_, _ = rand.Read(id[:])
	xid := binary.BigEndian.Uint32(id[:])
	msg := &xdr{}
	msg.u32(xid).u32(0).u32(2).u32(prog).u32(vers).u32(proc)
	cred := &xdr{} // AUTH_UNIX: stamp, empty machine name, uid 0, gid 0, no groups
	cred.u32(0).opaque(nil).u32(0).u32(0).u32(0)
	msg.u32(1).opaque(cred.Bytes()).u32(0).u32(0)
	msg.Write(args)
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: c.IP, Port: port})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCancel()
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = time.Second
	}
	buf := make([]byte, 64<<10)
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := conn.Write(msg.Bytes()); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(timeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = conn.SetReadDeadline(deadline)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break // resend
				}
				return nil, err
			}
			r := &reader{b: append([]byte(nil), buf[:n]...)}
			if r.u32() != xid || r.err != nil {
				continue // stale or foreign datagram
			}
			if r.u32() != 1 || r.u32() != 0 { // REPLY, MSG_ACCEPTED
				return nil, errors.New("prolink: NFS call rejected")
			}
			r.u32()
			r.opaque(400) // verifier
			if st := r.u32(); r.err != nil || st != 0 {
				return nil, fmt.Errorf("prolink: RPC program %d proc %d failed (%d)", prog, proc, st)
			}
			return r, nil
		}
	}
	return nil, fmt.Errorf("prolink: no reply from %s:%d", c.IP, port)
}

func (c *NFSClient) port(ctx context.Context, prog, vers uint32) (int, error) {
	if p, ok := c.ports[prog]; ok {
		return p, nil
	}
	pm := c.Portmap
	if pm == 0 {
		pm = 111
	}
	args := &xdr{}
	args.u32(prog).u32(vers).u32(17).u32(0) // GETPORT, UDP
	r, err := c.call(ctx, pm, rpcPortmap, 2, 3, args.Bytes())
	if err != nil {
		return 0, err
	}
	p := r.u32()
	if r.err != nil || p == 0 || p > 65535 {
		return 0, fmt.Errorf("prolink: program %d is not registered", prog)
	}
	if c.ports == nil {
		c.ports = map[uint32]int{}
	}
	c.ports[prog] = int(p)
	return int(p), nil
}

// Lookup mounts export ("/C/") and walks path ("/PIONEER/rekordbox/export.pdb").
func (c *NFSClient) Lookup(ctx context.Context, export, path string) ([]byte, NFSFile, error) {
	var file NFSFile
	mp, err := c.port(ctx, rpcMount, 1)
	if err != nil {
		return nil, file, err
	}
	np, err := c.port(ctx, rpcNFS, 2)
	if err != nil {
		return nil, file, err
	}
	r, err := c.call(ctx, mp, rpcMount, 1, 1, (&xdr{}).opaque(pioneerName(export)).Bytes())
	if err != nil {
		return nil, file, err
	}
	if st := r.u32(); r.err != nil || st != 0 {
		return nil, file, fmt.Errorf("prolink: mount %s failed (%d)", export, st)
	}
	fh := r.fixed(fhSize)
	if r.err != nil {
		return nil, file, r.err
	}
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
	if len(parts) == 0 {
		return nil, file, errors.New("prolink: empty NFS path")
	}
	for _, name := range parts {
		if name == "." || name == ".." {
			return nil, file, errors.New("prolink: unsafe NFS path")
		}
		args := &xdr{}
		args.Write(fh)
		args.opaque(pioneerName(name))
		r, err := c.call(ctx, np, rpcNFS, 2, 4, args.Bytes())
		if err != nil {
			return nil, file, err
		}
		if st := r.u32(); r.err != nil || st != 0 {
			return nil, file, fmt.Errorf("prolink: %s: NFS error %d", path, st)
		}
		fh = append([]byte(nil), r.fixed(fhSize)...)
		attr := r.fixed(68)
		if r.err != nil {
			return nil, file, r.err
		}
		file = NFSFile{Size: binary.BigEndian.Uint32(attr[20:]), MTime: binary.BigEndian.Uint32(attr[52:])}
	}
	return fh, file, nil
}

// Fetch reads a whole file of at most max bytes.
func (c *NFSClient) Fetch(ctx context.Context, export, path string, max uint32) ([]byte, NFSFile, error) {
	var b bytes.Buffer
	file, err := c.FetchTo(ctx, export, path, &b, max)
	return b.Bytes(), file, err
}

// FetchTo streams a file of at most max bytes to w, 8 KiB at a time.
func (c *NFSClient) FetchTo(ctx context.Context, export, path string, w io.Writer, max uint32) (NFSFile, error) {
	fh, file, err := c.Lookup(ctx, export, path)
	if err != nil {
		return file, err
	}
	if file.Size > max {
		return file, fmt.Errorf("prolink: %s is %d bytes, over the %d limit", path, file.Size, max)
	}
	np, err := c.port(ctx, rpcNFS, 2)
	if err != nil {
		return file, err
	}
	for done := uint32(0); done < file.Size; {
		args := &xdr{}
		args.Write(fh)
		args.u32(done).u32(nfsChunk).u32(0)
		r, err := c.call(ctx, np, rpcNFS, 2, 6, args.Bytes())
		if err != nil {
			return file, err
		}
		if st := r.u32(); r.err != nil || st != 0 {
			return file, fmt.Errorf("prolink: %s: NFS read error %d", path, st)
		}
		r.fixed(68)
		data := r.opaque(nfsChunk)
		if r.err != nil {
			return file, r.err
		}
		if len(data) == 0 {
			return file, fmt.Errorf("prolink: %s ended at %d of %d bytes", path, done, file.Size)
		}
		if rest := file.Size - done; uint32(len(data)) > rest {
			data = data[:rest]
		}
		if _, err := w.Write(data); err != nil {
			return file, err
		}
		done += uint32(len(data))
	}
	return file, nil
}
