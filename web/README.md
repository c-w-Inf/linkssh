# Page templates

`dir.html` and `login.html` are compiled into the binary by `go:embed` (see
`assets.go`), so run `make build` after editing them. The syntax is deliberately
trivial and needs no knowledge of Go templates:

- `{{NAME}}` is replaced by a value (single pass, so a value that itself contains
  `{{...}}` is never expanded again)
- `<!--name-->` … `<!--/name-->` delimits a section that the server repeats or
  drops
- every value is already HTML-escaped and safe in text or attributes
- unknown `{{MARKER}}`s are left untouched, so you can layer your own frontend
  templates on top

## dir.html

| Placeholder | Value |
| --- | --- |
| `{{PATH}}` | requested path, e.g. `/var/log` |
| `{{COUNT}}` | entry counter, e.g. `12 items` |
| `{{HIDDEN_HREF}}` | link toggling dot-files for this directory |
| `{{HIDDEN_MARK}}` | `✓` when dot-files are shown, empty otherwise |

The `parent` section is emitted only when a parent directory exists (it is
dropped entirely at the root) and the visitor holds the `pass` cookie; visitors
authenticated by token alone never receive it, so no parent token is exposed.

| Inside `parent` | Value |
| --- | --- |
| `{{PARENT}}` | link to the parent directory (URL-encoded, carries the token) |

The `entry` section is repeated once per directory entry.

| Inside `entry` | Value |
| --- | --- |
| `{{HREF}}` | link (URL-encoded, carries `?token=`; directories end with `/`) |
| `{{NAME}}` | entry name (escaped, without the trailing slash) |
| `{{SLASH}}` | `/` for directories, empty for files |
| `{{CLASS}}` | `dir` for directories, empty for files (ready for a `class` attribute) |
| `{{SIZE}}` | human readable size, empty for directories |
| `{{MODE}}` | permission string, e.g. `drwxr-xr-x` |
| `{{OWNER}}` | `user:group`, names resolved from `/etc/passwd` and `/etc/group` |
| `{{MOD}}` | modification time, `2006-01-02 15:04` |
| `{{DL}}` | download link for the entry (`?download=1`) |

The `download` section inside `entry` is emitted for files only, so directory rows
get no link at all.

The built-in script expects `#rows`, `#q`, `#count`, `#none`, `tr[data-name]` and
`tr.up`, and makes a click anywhere in a row follow that row's `<a href>`. If you
restructure the page, update the script or delete the `<script>` block.

## login.html

| Placeholder | Value |
| --- | --- |
| `{{PATH}}` | requested path |

The form is handled by the inline script: it writes the `pass` cookie and reloads.
The server only handles `GET` and `HEAD`. `#f`, `#p` and `#e` are the form, the input
and the error line; the error is shown again if the previous attempt failed
(tracked in `sessionStorage` for 15 seconds).

## error.html

Rendered for 4xx/5xx responses (404, 403 from the remote, 405, 501, 502, 503).

| Placeholder | Value |
| --- | --- |
| `{{STATUS}}` | numeric status, e.g. `404` |
| `{{TITLE}}` | short reason, e.g. `Not found` |
| `{{PATH}}` | requested path |

The underlying SSH/SFTP error is only written to the server log, never to the page.
