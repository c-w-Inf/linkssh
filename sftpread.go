package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	fxpInit    = 1
	fxpVersion = 2
	fxpClose   = 4
	fxpOpenDir = 11
	fxpReadDir = 12
	fxpStatus  = 101
	fxpHandle  = 102
	fxpName    = 104

	fxEOF = 1
)

type dirEntry struct {
	name string
	mode os.FileMode
	size int64
	uid  uint32
	gid  uint32
	mod  time.Time
}

func (e dirEntry) Name() string       { return e.name }
func (e dirEntry) Size() int64        { return e.size }
func (e dirEntry) Mode() os.FileMode  { return e.mode }
func (e dirEntry) ModTime() time.Time { return e.mod }
func (e dirEntry) IsDir() bool        { return e.mode.IsDir() }

type dirClient struct {
	ch    ssh.Channel
	r     *bufio.Reader
	mu    sync.Mutex
	id    uint32
	alive atomic.Bool
}

func newDirClient(cli *ssh.Client) (*dirClient, error) {
	ch, reqs, err := cli.OpenChannel("session", nil)
	if err != nil {
		return nil, err
	}
	go ssh.DiscardRequests(reqs)
	ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{"sftp"}))
	if err != nil || !ok {
		ch.Close()
		return nil, fmt.Errorf("sftp subsystem refused")
	}
	c := &dirClient{ch: ch, r: bufio.NewReaderSize(ch, 64<<10)}
	c.alive.Store(true)
	if err := c.init(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *dirClient) Close() {
	c.alive.Store(false)
	c.ch.Close()
}

func (c *dirClient) fail(err error) error {
	c.Close()
	return err
}

func (c *dirClient) init() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.frame(fxpInit, []byte{0, 0, 0, 3}); err != nil {
		return err
	}
	typ, _, _, err := c.recv()
	if err != nil {
		return err
	}
	if typ != fxpVersion {
		return fmt.Errorf("sftp: version packet %d", typ)
	}
	return nil
}

func (c *dirClient) frame(typ byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(1+len(payload)))
	buf[4] = typ
	copy(buf[5:], payload)
	if _, err := c.ch.Write(buf); err != nil {
		return c.fail(err)
	}
	return nil
}

func (c *dirClient) send(typ byte, body []byte) (uint32, error) {
	c.id++
	payload := binary.BigEndian.AppendUint32(nil, c.id)
	if err := c.frame(typ, append(payload, body...)); err != nil {
		return 0, err
	}
	return c.id, nil
}

func (c *dirClient) recv() (byte, uint32, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, 0, nil, c.fail(err)
	}
	buf := make([]byte, binary.BigEndian.Uint32(hdr[:]))
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return 0, 0, nil, c.fail(err)
	}
	return buf[0], binary.BigEndian.Uint32(buf[1:5]), buf[5:], nil
}

func (c *dirClient) OpenDir(path string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, err := c.send(fxpOpenDir, encodeString(path))
	if err != nil {
		return "", err
	}
	typ, got, body, err := c.recv()
	if err != nil {
		return "", err
	}
	if got != id || typ != fxpHandle {
		return "", c.fail(fmt.Errorf("sftp: opendir gave packet %d", typ))
	}
	h := &buffer{b: body}
	return h.str(), h.err
}

func (c *dirClient) CloseDir(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, err := c.send(fxpClose, encodeString(handle))
	if err != nil {
		return
	}
	c.recv()
	_ = id
}

func (c *dirClient) ReadDir(handle string, n int) ([][]dirEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ids := make([]uint32, n)
	for i := range ids {
		id, err := c.send(fxpReadDir, encodeString(handle))
		if err != nil {
			return nil, false, err
		}
		ids[i] = id
	}

	var out [][]dirEntry
	eof := false
	for _, id := range ids {
		typ, got, body, err := c.recv()
		if err != nil {
			return nil, false, err
		}
		if got != id {
			return nil, false, c.fail(fmt.Errorf("sftp: response id %d, want %d", got, id))
		}
		if eof {
			continue
		}
		switch typ {
		case fxpName:
			entries, err := parseName(body)
			if err != nil {
				return nil, false, err
			}
			out = append(out, entries)
		case fxpStatus:
			if b := (&buffer{b: body}); b.u32() == fxEOF {
				eof = true
				continue
			}
			return nil, false, fmt.Errorf("sftp: readdir status")
		default:
			return nil, false, c.fail(fmt.Errorf("sftp: readdir packet %d", typ))
		}
	}
	return out, eof, nil
}

func encodeString(s string) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(s))), s...)
}

type buffer struct {
	b   []byte
	err error
}

func (b *buffer) take(n int) []byte {
	if b.err != nil {
		return nil
	}
	if len(b.b) < n {
		b.err = io.ErrUnexpectedEOF
		return nil
	}
	v := b.b[:n]
	b.b = b.b[n:]
	return v
}

func (b *buffer) u32() uint32 {
	if v := b.take(4); v != nil {
		return binary.BigEndian.Uint32(v)
	}
	return 0
}

func (b *buffer) u64() uint64 {
	if v := b.take(8); v != nil {
		return binary.BigEndian.Uint64(v)
	}
	return 0
}

func (b *buffer) str() string {
	return string(b.take(int(b.u32())))
}

func (b *buffer) skip(n int) { b.take(n) }

func parseName(body []byte) ([]dirEntry, error) {
	b := &buffer{b: body}
	n := b.u32()
	out := make([]dirEntry, 0, n)
	for i := uint32(0); i < n && b.err == nil; i++ {
		var e dirEntry
		e.name = b.str()
		b.str()
		flags := b.u32()
		if flags&1 != 0 {
			e.size = int64(b.u64())
		}
		if flags&2 != 0 {
			e.uid = b.u32()
			e.gid = b.u32()
		}
		if flags&4 != 0 {
			e.mode = fileMode(b.u32())
		}
		if flags&8 != 0 {
			b.skip(4)
			e.mod = time.Unix(int64(b.u32()), 0)
		}
		if flags&0x80000000 != 0 {
			for j, k := b.u32(), uint32(0); k < j; k++ {
				b.str()
				b.str()
			}
		}
		if e.name == "." || e.name == ".." {
			continue
		}
		out = append(out, e)
	}
	return out, b.err
}

func fileMode(perm uint32) os.FileMode {
	m := os.FileMode(perm & 0777)
	switch perm & 0170000 {
	case 0010000:
		m |= os.ModeNamedPipe
	case 0020000:
		m |= os.ModeDevice | os.ModeCharDevice
	case 0040000:
		m |= os.ModeDir
	case 0060000:
		m |= os.ModeDevice
	case 0120000:
		m |= os.ModeSymlink
	case 0140000:
		m |= os.ModeSocket
	}
	if perm&04000 != 0 {
		m |= os.ModeSetuid
	}
	if perm&02000 != 0 {
		m |= os.ModeSetgid
	}
	if perm&01000 != 0 {
		m |= os.ModeSticky
	}
	return m
}
