package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

type fileServer struct {
	rm      *Remote
	dir     dirTemplate
	dialog  loginTemplate
	errpage errorTemplate
	secret  string

	nameMu  sync.Mutex
	users   map[int]string
	groups  map[int]string
	namesOK bool
}

func newFileServer(rm *Remote, secret string) (*fileServer, error) {
	dirRaw, err := webFS.ReadFile("web/dir.html")
	if err != nil {
		return nil, err
	}
	dir, err := parseDirTemplate(string(dirRaw))
	if err != nil {
		return nil, err
	}
	loginRaw, err := webFS.ReadFile("web/login.html")
	if err != nil {
		return nil, err
	}
	errorRaw, err := webFS.ReadFile("web/error.html")
	if err != nil {
		return nil, err
	}
	return &fileServer{
		rm:      rm,
		dir:     dir,
		dialog:  loginTemplate{shell: string(loginRaw)},
		errpage: errorTemplate{shell: string(errorRaw)},
		secret:  secret,
	}, nil
}

func (s *fileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &respWriter{ResponseWriter: w}
	start := time.Now()
	defer func() {
		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf("%s %s %d %dB %s", r.Method, r.URL.Path, status, rw.bytes,
			time.Since(start).Round(time.Millisecond))
	}()

	upath := path.Clean("/" + r.URL.Path)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		rw.Header().Set("Allow", "GET, HEAD")
		s.showError(rw, http.StatusMethodNotAllowed, "Method not allowed", upath)
		return
	}
	if s.secret != "" && r.URL.Query().Get("token") == "" && !s.hasCookie(r) {
		s.deny(rw, r, upath, nil)
		return
	}

	rpath := path.Join(s.rm.Root(), upath)
	var fi os.FileInfo
	err := s.with(func(c *sftp.Client) (err error) {
		fi, err = c.Stat(rpath)
		return err
	})
	if err != nil {
		s.fail(rw, upath, err)
		return
	}
	var times []time.Time
	if s.secret != "" {
		if fi.IsDir() {
			times, err = s.dirTimes(upath, fi.ModTime())
		} else {
			times, err = s.dirTimes(path.Dir(upath), time.Time{})
		}
		if err != nil {
			s.fail(rw, upath, err)
			return
		}
	}
	if !s.authorized(r, upath, times) {
		s.deny(rw, r, upath, times)
		return
	}
	if s.setHidden(rw, r) {
		return
	}

	switch {
	case fi.IsDir() && !strings.HasSuffix(r.URL.Path, "/"):
		redirect(rw, r, r.URL.Path+"/")
	case fi.IsDir():
		s.serveDir(rw, r, rpath, upath, fi, times)
	case strings.HasSuffix(r.URL.Path, "/"):
		redirect(rw, r, strings.TrimSuffix(r.URL.Path, "/"))
	default:
		s.serveFile(rw, r, rpath, fi)
	}
}

func (s *fileServer) with(fn func(*sftp.Client) error) error {
	c, done, err := s.rm.Acquire()
	if err != nil {
		return err
	}
	err = fn(c)
	done(err)
	return err
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	u := *r.URL
	u.Path = to
	http.Redirect(w, r, u.RequestURI(), http.StatusMovedPermanently)
}

func (s *fileServer) serveDir(w *respWriter, r *http.Request, rpath, upath string, fi os.FileInfo, times []time.Time) {
	show := s.showHidden(r)
	var (
		list    []os.FileInfo
		symMods = map[string]time.Time{}
		symDirs = map[string]bool{}
	)
	err := s.with(func(c *sftp.Client) error {
		var err error
		if list, err = c.ReadDir(rpath); err != nil {
			return err
		}

		for _, fi := range list {
			if fi.Mode()&os.ModeSymlink == 0 {
				continue
			}
			st, err := c.Stat(path.Join(rpath, fi.Name()))
			if err != nil {
				continue
			}
			symMods[fi.Name()] = st.ModTime()
			if st.IsDir() {
				symDirs[fi.Name()] = true
			}
		}
		return nil
	})
	if err != nil {
		s.fail(w, upath, err)
		return
	}

	if !show {
		visible := list[:0]
		for _, fi := range list {
			if !strings.HasPrefix(fi.Name(), ".") {
				visible = append(visible, fi)
			}
		}
		list = visible
	}

	isDir := func(fi os.FileInfo) bool { return fi.IsDir() || symDirs[fi.Name()] }
	sort.Slice(list, func(i, j int) bool {
		if di, dj := isDir(list[i]), isDir(list[j]); di != dj {
			return di
		}
		return strings.ToLower(list[i].Name()) < strings.ToLower(list[j].Name())
	})

	data := pageData{Path: upath, Entries: make([]pageEntry, 0, len(list))}
	data.HiddenHref = s.toggleHref(upath, !show, times)
	if show {
		data.HiddenMark = "✓"
	}
	data.Count = fmt.Sprintf("%d item", len(list))
	if len(list) != 1 {
		data.Count += "s"
	}
	if upath != "/" && (s.secret == "" || s.hasCookie(r)) {
		var parent []time.Time
		if len(times) > 1 {
			parent = times[:len(times)-1]
		}
		data.Parent = s.href(path.Dir(upath), true, parent)
	}
	users, groups := s.names()
	for _, fi := range list {
		mod := fi.ModTime()
		if target, ok := symMods[fi.Name()]; ok {
			mod = target
		}
		dir := isDir(fi)
		child := times
		if dir {
			child = append(append([]time.Time{}, times...), mod)
		}
		e := pageEntry{
			Name:  fi.Name(),
			Href:  s.href(path.Join(upath, fi.Name()), dir, child),
			IsDir: dir,
			Mode:  modeString(fi.Mode()),
			Owner: ownerString(fi, users, groups),
			Mod:   mod.Format("2006-01-02 15:04"),
		}
		if !e.IsDir {
			e.Size = humanSize(fi.Size())
			e.Download = withQuery(e.Href, "download=1")
		}
		data.Entries = append(data.Entries, e)
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	s.dir.execute(w, data)
}

func hrefFor(p string) string {
	return (&url.URL{Path: p}).EscapedPath()
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func (s *fileServer) serveFile(w *respWriter, r *http.Request, rpath string, fi os.FileInfo) {
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": fi.Name()}))
	}
	err := s.with(func(c *sftp.Client) error {
		f, err := c.Open(rpath)
		if err != nil {
			return err
		}
		defer f.Close()

		body := &tracked{rs: f}
		http.ServeContent(w, r, fi.Name(), fi.ModTime(), body)
		return body.err
	})
	if err != nil {
		s.fail(w, r.URL.Path, err)
	}
}

type tracked struct {
	rs  io.ReadSeeker
	err error
}

func (t *tracked) Read(p []byte) (int, error) {
	n, err := t.rs.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		t.err = err
	}
	return n, err
}

func (t *tracked) Seek(off int64, whence int) (int64, error) {
	n, err := t.rs.Seek(off, whence)
	if err != nil {
		t.err = err
	}
	return n, err
}

func (s *fileServer) fail(w *respWriter, upath string, err error) {
	code, title := classify(err)
	if code >= 500 {
		log.Printf("%s: %v", upath, err)
	}
	if w.status != 0 {
		return
	}
	s.showError(w, code, title, upath)
}

func (s *fileServer) showError(w *respWriter, status int, title, upath string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	s.errpage.execute(w, status, title, upath)
}

func classify(err error) (int, string) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return http.StatusNotFound, "Not found"
	case errors.Is(err, os.ErrPermission):
		return http.StatusForbidden, "Permission denied"
	case errors.Is(err, errRemoteClosed):
		return http.StatusServiceUnavailable, "Service unavailable"
	}
	var se *sftp.StatusError
	if errors.As(err, &se) && se.FxCode() == sftp.ErrSSHFxOpUnsupported {
		return http.StatusNotImplemented, "Not implemented"
	}
	return http.StatusBadGateway, "Bad gateway"
}

type writerOnly struct{ io.Writer }

var copyBufPool = sync.Pool{New: func() any {
	b := make([]byte, 1<<20)
	return &b
}}

func (w *respWriter) ReadFrom(src io.Reader) (int64, error) {
	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	return io.CopyBuffer(writerOnly{w}, src, *bufp)
}

type respWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *respWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func chainDirs(dir string) []string {
	var dirs []string
	for {
		dirs = append(dirs, dir)
		if dir == "/" {
			break
		}
		dir = path.Dir(dir)
	}
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	return dirs
}

func (s *fileServer) dirTimes(dir string, known time.Time) ([]time.Time, error) {
	dirs := chainDirs(dir)
	times := make([]time.Time, len(dirs))
	root := s.rm.Root()
	err := s.with(func(c *sftp.Client) error {
		for i, d := range dirs {
			if i == len(dirs)-1 && !known.IsZero() {
				times[i] = known
				continue
			}
			st, err := c.Stat(path.Join(root, d))
			if err != nil {
				return err
			}
			times[i] = st.ModTime()
		}
		return nil
	})
	return times, err
}

func (s *fileServer) token(upath string, times []time.Time) string {
	p := strings.TrimSuffix(upath, "/")
	if p == "" {
		p = "/"
	}
	var b strings.Builder
	b.WriteString(p)
	for _, t := range times {
		b.WriteString(strconv.FormatInt(t.Unix(), 10))
	}
	b.WriteString(s.secret)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func (s *fileServer) href(upath string, isDir bool, times []time.Time) string {
	p := hrefFor(upath)
	if isDir && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	if s.secret == "" {
		return p
	}
	return p + "?token=" + s.token(upath, times)
}

func (s *fileServer) hasCookie(r *http.Request) bool {
	c, err := r.Cookie("pass")
	if err != nil {
		return false
	}
	if sameSecret(c.Value, s.secret) {
		return true
	}
	u, err := url.PathUnescape(c.Value)
	return err == nil && sameSecret(u, s.secret)
}

func (s *fileServer) authorized(r *http.Request, upath string, times []time.Time) bool {
	if s.secret == "" {
		return true
	}
	tok := r.URL.Query().Get("token")
	return tok != "" && sameSecret(tok, s.token(upath, times))
}

func (s *fileServer) deny(w http.ResponseWriter, r *http.Request, upath string, times []time.Time) {
	if s.hasCookie(r) {
		s.redirectWithToken(w, r, upath, times, http.StatusFound)
		return
	}
	code := http.StatusUnauthorized
	if r.URL.Query().Get("token") != "" {
		code = http.StatusForbidden
	}
	if r.Method == http.MethodGet {
		s.showLogin(w, upath, code)
		return
	}
	w.WriteHeader(code)
}

func (s *fileServer) redirectWithToken(w http.ResponseWriter, r *http.Request, upath string, times []time.Time, code int) {
	u := *r.URL
	q := u.Query()
	q.Set("token", s.token(upath, times))
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.RequestURI(), code)
}

func (s *fileServer) names() (map[int]string, map[int]string) {
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	if s.namesOK {
		return s.users, s.groups
	}
	users, groups := map[int]string{}, map[int]string{}
	if err := s.with(func(c *sftp.Client) error {
		readIDMap(c, "/etc/passwd", users)
		readIDMap(c, "/etc/group", groups)
		return nil
	}); err != nil {
		return users, groups
	}
	s.users, s.groups, s.namesOK = users, groups, true
	return users, groups
}

func readIDMap(c *sftp.Client, path string, ids map[int]string) {
	f, err := c.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		id, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		ids[id] = fields[0]
	}
}

func modeString(m os.FileMode) string {
	b := []byte("----------")
	switch {
	case m&os.ModeDir != 0:
		b[0] = 'd'
	case m&os.ModeSymlink != 0:
		b[0] = 'l'
	case m&os.ModeNamedPipe != 0:
		b[0] = 'p'
	case m&os.ModeSocket != 0:
		b[0] = 's'
	case m&os.ModeDevice != 0:
		if m&os.ModeCharDevice != 0 {
			b[0] = 'c'
		} else {
			b[0] = 'b'
		}
	}
	const rwx = "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if m&(1<<uint(8-i)) != 0 {
			b[i+1] = rwx[i]
		}
	}
	setBit := func(i int, set bool, lower, upper byte) {
		if !set {
			return
		}
		if b[i] == 'x' {
			b[i] = lower
		} else {
			b[i] = upper
		}
	}
	setBit(3, m&os.ModeSetuid != 0, 's', 'S')
	setBit(6, m&os.ModeSetgid != 0, 's', 'S')
	setBit(9, m&os.ModeSticky != 0, 't', 'T')
	return string(b)
}

func ownerString(fi os.FileInfo, users, groups map[int]string) string {
	st, ok := fi.Sys().(*sftp.FileStat)
	if !ok {
		return ""
	}
	user := users[int(st.UID)]
	if user == "" {
		user = strconv.FormatUint(uint64(st.UID), 10)
	}
	group := groups[int(st.GID)]
	if group == "" {
		group = strconv.FormatUint(uint64(st.GID), 10)
	}
	return user + ":" + group
}

func (s *fileServer) showHidden(r *http.Request) bool {
	c, err := r.Cookie("hidden")
	return err == nil && c.Value == "1"
}

func (s *fileServer) toggleHref(upath string, on bool, times []time.Time) string {
	value := "0"
	if on {
		value = "1"
	}
	return withQuery(s.href(upath, true, times), "hidden="+value)
}

func withQuery(u, kv string) string {
	if strings.Contains(u, "?") {
		return u + "&" + kv
	}
	return u + "?" + kv
}

func (s *fileServer) setHidden(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := r.URL.Query()["hidden"]; !ok {
		return false
	}
	value := "0"
	if r.URL.Query().Get("hidden") == "1" {
		value = "1"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "hidden",
		Value:    value,
		Path:     "/",
		MaxAge:   30 * 24 * 60 * 60,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	u := *r.URL
	q := u.Query()
	q.Del("hidden")
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.RequestURI(), http.StatusFound)
	return true
}

func (s *fileServer) showLogin(w http.ResponseWriter, upath string, code int) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	s.dialog.execute(w, upath)
}

func sameSecret(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
