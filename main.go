package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("linkssh: ")

	var (
		listenPort int
		sshPort    int
		bind       string
		root       string
		secret     string
	)
	flag.IntVar(&listenPort, "l", 8080, "HTTP listen port")
	flag.IntVar(&sshPort, "p", 22, "SSH port")
	flag.StringVar(&bind, "b", "127.0.0.1", "HTTP listen address")
	flag.StringVar(&root, "r", "", "remote directory to serve as / (default: login home)")
	flag.StringVar(&secret, "s", "", "shared secret: only serve ?token=sha256(path+folder mtimes+secret); cookie \"pass\" holds the secret")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] user@host\n\nflags:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nexample:\n  %s -l 8080 -r /var/log me@example.com\n", filepath.Base(os.Args[0]))
	}
	flag.Parse()

	if listenPort < 1 || listenPort > 65535 {
		log.Fatalf("invalid http port %d", listenPort)
	}
	if sshPort < 1 || sshPort > 65535 {
		log.Fatalf("invalid ssh port %d", sshPort)
	}
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	login, host, err := parseTarget(flag.Arg(0))
	if err != nil {
		log.Fatalf("%v", err)
	}
	if secret == "" {
		log.Print("access secret disabled")
	} else {
		log.Print("access secret enabled")
	}
	addr := net.JoinHostPort(host, strconv.Itoa(sshPort))
	cfg, err := sshConfig(login)
	if err != nil {
		log.Fatalf("%v", err)
	}

	remote := NewRemote(addr, root, cfg)
	if err := remote.Connect(); err != nil {
		log.Fatalf("connect to %s@%s: %v", login, addr, err)
	}
	defer remote.Close()
	log.Printf("serving ssh://%s@%s%s", login, addr, remote.Root())

	srv, err := newFileServer(remote, secret)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(listenPort)))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("listening on http://%s", ln.Addr())

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	case <-ctx.Done():
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}

func parseTarget(s string) (login, host string, err error) {
	if strings.Contains(s, "://") {
		return "", "", fmt.Errorf("ssh target must be user@host, got %q", s)
	}
	i := strings.LastIndex(s, "@")
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("ssh target must be user@host, got %q", s)
	}
	login, host = s[:i], s[i+1:]
	switch {
	case strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]"):
		host = host[1 : len(host)-1]
	case strings.Contains(host, ":"):
		return "", "", fmt.Errorf("ssh target must be user@host without a port, got %q (use -p)", s)
	}
	return login, host, nil
}

func sshConfig(login string) (*ssh.ClientConfig, error) {
	var auth []ssh.AuthMethod

	for _, p := range defaultKeys() {
		signer, err := readKey(p)
		if err != nil {
			log.Printf("skipping key %s: %v", p, err)
			continue
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if len(auth) == 0 {
		return nil, fmt.Errorf("no usable private key in %s", sshDir())
	}

	hostKey, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User:            login,
		Auth:            auth,
		HostKeyCallback: hostKey,
		Timeout:         15 * time.Second,
	}, nil
}

func defaultKeys() []string {
	dir := sshDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "id_") || strings.HasSuffix(name, ".pub") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	return paths
}

func sshDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".ssh")
	}
	return ".ssh"
}

func readKey(path string) (ssh.Signer, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err == nil {
		return signer, nil
	}
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		return nil, fmt.Errorf("key %s is encrypted, use an unencrypted key", path)
	}
	return nil, fmt.Errorf("parse key %s: %w", path, err)
}

func hostKeyCallback() (ssh.HostKeyCallback, error) {
	var files []string
	for _, name := range []string{"known_hosts", "known_hosts2"} {
		p := filepath.Join(sshDir(), name)
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no known_hosts in %s: connect once with ssh to trust the host", sshDir())
	}
	check, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts: %w", err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := check(hostname, remote, key); err != nil {
			return fmt.Errorf("host key verification failed for %s: %w", hostname, err)
		}
		return nil
	}, nil
}
