# linkssh

Serve a remote machine's filesystem over HTTP, with SSH/SFTP as the transport.

`http://localhost:8080/path` makes the server read `/path` on the SSH host: files
are returned as-is (Range, Last-Modified and HEAD are supported) and directories
render as a minimal interactive index with filtering and keyboard navigation. Each
row has a download button that forces `Content-Disposition: attachment` instead of
rendering the file in the browser. Each
row shows name, size, mode, owner:group and mtime; owner names are resolved from
the remote `/etc/passwd` and `/etc/group`, falling back to numeric IDs.

HTTP paths map 1:1 onto remote paths. The service is read-only and stateless.
The optional `-s` flag gates everything behind a token derived from the path and
the modification times of every folder above it.

## Layout

```
main.go      flags, SSH auth, HTTP server, graceful shutdown
remote.go    SSH/SFTP connection (keepalive, automatic reconnect)
handler.go   path mapping, file/dir responses, token and password checks
render.go    the tiny placeholder engine used by the HTML pages
assets.go    go:embed of web/*.html
web/         dir.html (index), login.html (password dialog), README.md (templates)
Makefile     build / run / vet / fmt / clean
```

The pages are real `.html` files compiled into the binary with `go:embed`: no HTML
strings in Go source and no code generation step. Run `make build` after editing.

The template syntax has only two constructs: `{{NAME}}` is replaced with a value,
and `<!--block-->` … `<!--/block-->` marks an optional or repeated fragment. See
[web/README.md](web/README.md) for the placeholder list. You can restyle the pages
or replace them with your own frontend without touching Go code; unknown
`{{MARKER}}`s are left untouched.

## Usage

```bash
make build            # same as go build -trimpath -o linkssh .
./linkssh -l 8080 me@example.com
```

```
linkssh: serving ssh://me@example.com:22/home/me
linkssh: listening on http://127.0.0.1:8080
```

```
GET /                -> index of the remote home directory
GET /logs/app.txt    -> contents of /logs/app.txt
GET /logs            -> 301 to /logs/
```

Names starting with `.` are hidden in the index by default; the `hidden` option in
the page header toggles them and the choice is kept in the `hidden` cookie for 30
days (the header link carries `?hidden=1` / `?hidden=0`).

The target is always `user@host` (for IPv6, `user@[::1]`); the SSH port comes from
`-p` only.

## Flags

| Flag | Description |
| --- | --- |
| `-l` | HTTP listen port, default `8080` |
| `-p` | SSH port, default `22` |
| `-b` | HTTP listen address, default `127.0.0.1` |
| `-r` | Remote directory to serve as `/`, default the login home |
| `-s` | Access secret: requires a `?token=` per path (see below) |

The host key is always checked against `~/.ssh/known_hosts` (there is no
insecure mode), so connect to the host once with `ssh` to record it.

Authentication uses private keys from `~/.ssh` only: `id_ed25519`, `id_ecdsa`,
`id_rsa` and any other `id_*` key (`.pub` files are ignored). Passwords,
keyboard-interactive and ssh-agent are not supported, so the key must not be
encrypted:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519 -N ''
```

## Access secret (-s)

```bash
./linkssh -l 8080 -s 'mysecret' me@example.com
```

Once enabled, files and directories behave the same way:

1. A request with the right `?token=` is served:

   ```bash
   # on the SSH host, with -r /var/log so GET /app.log reads /var/log/app.log
   printf '%s' "/app.log$(stat -c %Y /var/log)mysecret" | sha256sum
   # 6f0d... ->  GET /app.log?token=6f0d...

   # with -r / and the deeper path /var/log/app.log:
   printf '%s' "/var/log/app.log$(stat -c %Y /)$(stat -c %Y /var)$(stat -c %Y /var/log)mysecret" | sha256sum
   ```

   The token is the hex `sha256(path + m1 + m2 + ... + mn + secret)`: `path` is the
   URL path (the part after `-r`), and `m1..mn` are the modification times of the
   folders from the served root down to the entry's folder, as Unix seconds in
   decimal and without separators. For a directory the chain ends with the
   directory itself; for a file it ends with its parent. `/dir` and `/dir/` share
   one token; the root path is hashed as `/`.

   Only folder timestamps are used (SFTP has no birth time, so `mtime` is the
   available value): a file's own mtime does not matter, but adding, removing or
   renaming anything in a folder updates that folder's mtime and invalidates the
   tokens of everything below it.

2. Without a valid `?token=` the `pass` cookie is checked. If it matches, the
   server answers `302` with the same path plus the correct token (other query
   parameters are preserved). Every link in the index already carries the token
   for its own path.

   A visitor authenticated **only** by a token does not get the `..` row: its link
   (and the parent's token) is never rendered, so a token handed out for a
   subtree cannot be used to walk up. With a valid `pass` cookie the `..` row is
   shown again.

3. If neither works, a password dialog page is returned (missing token -> `401`,
   wrong token -> `403`, both with `Cache-Control: no-store`). The page sets the
   `pass` cookie itself (`Path=/`, `SameSite=Lax`, 30 days) and reloads; the
   reload hits step 2 and shows the content. A wrong secret re-opens the dialog
   with `wrong password`. The server only handles `GET` and `HEAD`, so the page
   never sends anything else.

   You can also set the cookie yourself, e.g. `curl -b 'pass=mysecret'` or through
   DevTools.

Without `-s` the checks are disabled and the server is a plain read-only proxy.

## Status codes

| Situation | Status |
| --- | --- |
| Normal response | `200` |
| Directory without a trailing slash / file with one | `301` |
| Cookie accepted, redirecting to the token URL | `302` |
| Missing token and no valid cookie (password dialog) | `401` |
| Token mismatch (same dialog) | `403` |
| Remote path does not exist (`SSH_FX_NO_SUCH_FILE`, `os.ErrNotExist`) | `404` |
| Remote permission denied (`SSH_FX_PERMISSION_DENIED`, `os.ErrPermission`) | `403` |
| Operation not supported by the remote | `501` |
| Method other than `GET`/`HEAD` | `405` |
| SSH connection failed / lost, other SFTP errors | `502` |
| Server shutting down | `503` |

Failures render `web/error.html` with only the status, a short title and the
requested path; the underlying SSH/SFTP error goes to the server log and is never
sent to the client.

`..` is cleaned away, so a request cannot escape the `-r` root. A broken SSH
connection is re-established on the next request.

## Security

By default the HTTP side is unauthenticated (a bare proxy when `-s` is not used)
and only listens on `127.0.0.1`. With `-s`, the token is a hash of the path and
its folder timestamps: it appears in URLs and may leak through browser history,
`Referer` headers or access logs. It is path-level access control, not a session
mechanism. Tokens are verified only after the folder chain is `stat`ed, so a
request carrying a token can distinguish an existing path (`403`) from a missing
one (`404`) even when the token is wrong; requests without any token or cookie
are rejected before touching the remote filesystem.

The secret itself lives in the `pass` cookie. When the dialog page writes it from
JavaScript it cannot be `HttpOnly`, so any script running on the page could read
it. If that matters, create the cookie manually in
DevTools/curl, or only use `?token=`.

To expose the service, add TLS and authentication in a reverse proxy, or just use
SSH port forwarding:

```bash
ssh -L 8080:127.0.0.1:8080 me@example.com
```
