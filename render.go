package main

import (
	"fmt"
	"html"
	"io"
	"strconv"
	"strings"
)

func fill(w io.Writer, tmpl string, kv map[string]string) {
	pairs := make([]string, 0, 2*len(kv))
	for k, v := range kv {
		pairs = append(pairs, "{{"+k+"}}", v)
	}
	io.WriteString(w, strings.NewReplacer(pairs...).Replace(tmpl))
}

func expand(tmpl string, kv map[string]string) string {
	var b strings.Builder
	fill(&b, tmpl, kv)
	return b.String()
}

func cutBlock(s, name, placeholder string) (string, string, error) {
	open, closing := "<!--"+name+"-->", "<!--/"+name+"-->"
	i := strings.Index(s, open)
	if i < 0 {
		return s, "", nil
	}
	j := strings.Index(s[i+len(open):], closing)
	if j < 0 {
		return s, "", fmt.Errorf("template: %s block is not closed with %s", open, closing)
	}
	block := s[i+len(open) : i+len(open)+j]
	rest := s[:i] + placeholder + s[i+len(open)+j+len(closing):]
	return rest, block, nil
}

type pageEntry struct {
	Name  string
	Href  string
	IsDir bool
	Size  string
	Mode  string
	Owner string
	Mod   string

	Download string
}

type pageData struct {
	Path       string
	Count      string
	Parent     string
	HiddenHref string
	HiddenMark string
	Entries    []pageEntry
}

type dirTemplate struct {
	shell    string
	entry    string
	download string
	parent   string
}

func parseDirTemplate(raw string) (dirTemplate, error) {
	var t dirTemplate
	shell, entry, err := cutBlock(raw, "entry", "{{ENTRIES}}")
	if err != nil {
		return t, err
	}
	entry, download, err := cutBlock(entry, "download", "{{DOWNLOAD}}")
	if err != nil {
		return t, err
	}
	shell, parent, err := cutBlock(shell, "parent", "{{PARENT_ROW}}")
	if err != nil {
		return t, err
	}
	t.shell, t.entry, t.download, t.parent = shell, entry, download, parent
	return t, nil
}

func (t dirTemplate) execute(w io.Writer, d pageData) {
	var rows strings.Builder
	for _, e := range d.Entries {
		var slash, class, download string
		if e.IsDir {
			slash, class = "/", "dir"
		} else if e.Download != "" {
			download = expand(t.download, map[string]string{"DL": html.EscapeString(e.Download)})
		}
		fill(&rows, t.entry, map[string]string{
			"HREF":     html.EscapeString(e.Href),
			"NAME":     html.EscapeString(e.Name),
			"SLASH":    slash,
			"CLASS":    class,
			"SIZE":     html.EscapeString(e.Size),
			"MODE":     html.EscapeString(e.Mode),
			"OWNER":    html.EscapeString(e.Owner),
			"MOD":      html.EscapeString(e.Mod),
			"DOWNLOAD": download,
		})
	}

	parentRow := ""
	if d.Parent != "" {
		parentRow = expand(t.parent, map[string]string{"PARENT": html.EscapeString(d.Parent)})
	}

	fill(w, t.shell, map[string]string{
		"PATH":        html.EscapeString(d.Path),
		"COUNT":       html.EscapeString(d.Count),
		"HIDDEN_HREF": html.EscapeString(d.HiddenHref),
		"HIDDEN_MARK": d.HiddenMark,
		"ENTRIES":     rows.String(),
		"PARENT_ROW":  parentRow,
	})
}

type errorTemplate struct{ shell string }

func (t errorTemplate) execute(w io.Writer, status int, title, upath string) {
	fill(w, t.shell, map[string]string{
		"STATUS": strconv.Itoa(status),
		"TITLE":  html.EscapeString(title),
		"PATH":   html.EscapeString(upath),
	})
}

type loginTemplate struct{ shell string }

func (t loginTemplate) execute(w io.Writer, upath string) {
	fill(w, t.shell, map[string]string{
		"PATH": html.EscapeString(upath),
	})
}
