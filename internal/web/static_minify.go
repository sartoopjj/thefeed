package web

import (
	"bytes"
	"io/fs"
	"strings"
	"time"

	"github.com/tdewolff/minify/v2"
	mcss "github.com/tdewolff/minify/v2/css"
	mhtml "github.com/tdewolff/minify/v2/html"
	mjs "github.com/tdewolff/minify/v2/js"
)

// staticMinFS serves the embedded static assets with .js/.css/.html minified
// (for index.html that includes its inline <script> blocks — the HTML
// minifier delegates those to the JS minifier). The embedded assets never
// change at runtime, so minification happens once at startup and the results
// are kept in memory rather than re-run per request.
var staticMinFS = func() fs.FS {
	sub, _ := fs.Sub(staticFS, "static")
	return newMinifiedStaticFS(sub)
}()

func newMinifiedStaticFS(base fs.FS) fs.FS {
	m := minify.New()
	m.AddFunc("text/css", mcss.Minify)
	m.AddFunc("application/javascript", mjs.Minify)
	m.AddFunc("text/html", mhtml.Minify)

	startup := time.Now()
	files := make(map[string]*minifiedFile)
	fs.WalkDir(base, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		var mediatype string
		switch {
		case strings.HasSuffix(path, ".css"):
			mediatype = "text/css"
		case strings.HasSuffix(path, ".js"):
			mediatype = "application/javascript"
		case strings.HasSuffix(path, ".html"):
			mediatype = "text/html"
		default:
			return nil
		}
		raw, err := fs.ReadFile(base, path)
		if err != nil {
			return nil
		}
		out, err := m.Bytes(mediatype, raw)
		if err != nil {
			// Fall back to the unminified asset rather than fail the build.
			out = raw
		}
		files[path] = &minifiedFile{data: out, modTime: startup}
		return nil
	})

	return &minifiedStaticFS{base: base, files: files}
}

type minifiedFile struct {
	data    []byte
	modTime time.Time
}

type minifiedStaticFS struct {
	base  fs.FS
	files map[string]*minifiedFile
}

func (mfs *minifiedStaticFS) Open(name string) (fs.File, error) {
	if mf, ok := mfs.files[name]; ok {
		return &openMinifiedFile{
			Reader: bytes.NewReader(mf.data),
			info:   minifiedFileInfo{name: name, size: int64(len(mf.data)), modTime: mf.modTime},
		}, nil
	}
	return mfs.base.Open(name)
}

// openMinifiedFile satisfies fs.File (and io.Seeker, for HTTP Range support)
// over the in-memory minified bytes.
type openMinifiedFile struct {
	*bytes.Reader
	info minifiedFileInfo
}

func (f *openMinifiedFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *openMinifiedFile) Close() error               { return nil }

type minifiedFileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (i minifiedFileInfo) Name() string       { return i.name }
func (i minifiedFileInfo) Size() int64        { return i.size }
func (i minifiedFileInfo) Mode() fs.FileMode  { return 0o444 }
func (i minifiedFileInfo) ModTime() time.Time { return i.modTime }
func (i minifiedFileInfo) IsDir() bool        { return false }
func (i minifiedFileInfo) Sys() any           { return nil }
