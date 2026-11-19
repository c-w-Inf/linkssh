package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

var errRemoteClosed = errors.New("ssh connection closed")

type Remote struct {
	addr string
	root string
	cfg  *ssh.ClientConfig

	mu      sync.Mutex
	cli     *ssh.Client
	sf      *sftp.Client
	absRoot string
	closed  bool
}

func NewRemote(addr, root string, cfg *ssh.ClientConfig) *Remote {
	return &Remote{addr: addr, root: root, cfg: cfg}
}

func (r *Remote) Connect() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRemoteClosed
	}
	return r.dialLocked()
}

func (r *Remote) Root() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.absRoot
}

func (r *Remote) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.closeLocked()
}

func (r *Remote) closeLocked() {
	if r.cli != nil {
		r.cli.Close()
	}
	r.cli, r.sf = nil, nil
}

func (r *Remote) Acquire() (*sftp.Client, func(error), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, errRemoteClosed
	}
	if r.sf == nil {
		if err := r.dialLocked(); err != nil {
			return nil, nil, err
		}
	}
	sf := r.sf
	return sf, func(err error) { r.release(sf, err) }, nil
}

func (r *Remote) release(sf *sftp.Client, err error) {
	if !isConnError(err) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sf == sf {
		r.closeLocked()
	}
}

func (r *Remote) dialLocked() error {
	r.closeLocked()

	conn, err := net.DialTimeout("tcp", r.addr, 15*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", r.addr, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, r.addr, r.cfg)
	if err != nil {
		conn.Close()
		return fmt.Errorf("ssh handshake with %s: %w", r.addr, err)
	}
	cli := ssh.NewClient(sshConn, chans, reqs)

	sf, err := sftp.NewClient(cli)
	if err != nil {
		cli.Close()
		return fmt.Errorf("sftp subsystem on %s: %w", r.addr, err)
	}
	if err := r.resolveRootLocked(sf); err != nil {
		sf.Close()
		cli.Close()
		return err
	}

	r.cli, r.sf = cli, sf
	go r.keepalive(cli)
	return nil
}

func (r *Remote) keepalive(cli *ssh.Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if _, _, err := cli.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			r.mu.Lock()
			if r.cli == cli {
				r.closeLocked()
			}
			r.mu.Unlock()
			return
		}
	}
}

func (r *Remote) resolveRootLocked(sf *sftp.Client) error {
	if r.absRoot != "" {
		return nil
	}
	root := r.root
	if root == "" {
		wd, err := sf.Getwd()
		if err != nil {
			return fmt.Errorf("resolve remote home: %w", err)
		}
		root = wd
	}
	abs, err := sf.RealPath(root)
	if err != nil {
		return fmt.Errorf("resolve remote root %q: %w", root, err)
	}
	r.absRoot = abs
	return nil
}

func isConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, io.EOF) {
		return false
	}
	var se *sftp.StatusError
	if errors.As(err, &se) {
		switch se.FxCode() {
		case sftp.ErrSSHFxConnectionLost, sftp.ErrSSHFxNoConnection, sftp.ErrSSHFxBadMessage:
			return true
		}
		return false
	}
	return true
}
