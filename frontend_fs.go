package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

type frontendFS struct {
	base      fs.FS
	buildTime time.Time
	etags     map[string]string
}

type frontendFile struct {
	fs.File
	seeker    io.ReadSeeker
	buildTime time.Time
}

type frontendFileInfo struct {
	fs.FileInfo
	buildTime time.Time
}

func newFrontendFS(source fs.FS, root string, buildUnix int64) (*frontendFS, error) {
	base, err := fs.Sub(source, root)
	if err != nil {
		return nil, fmt.Errorf("frontend sub fs: %w", err)
	}

	buildTime := time.Unix(buildUnix, 0).UTC()
	if buildUnix == 0 {
		buildTime = time.Now().UTC()
	}

	frontend := &frontendFS{
		base:      base,
		buildTime: buildTime,
		etags:     map[string]string{},
	}

	if err := fs.WalkDir(base, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}

		data, err := fs.ReadFile(base, path)
		if err != nil {
			return err
		}
		frontend.etags[cleanFrontendPath(path)] = makeETag(data)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("index frontend files: %w", err)
	}

	return frontend, nil
}

func (f *frontendFS) Open(name string) (fs.File, error) {
	file, err := f.base.Open(name)
	if err != nil {
		return nil, err
	}

	seeker, _ := file.(io.ReadSeeker)

	return &frontendFile{
		File:      file,
		seeker:    seeker,
		buildTime: f.buildTime,
	}, nil
}

func (f *frontendFS) ETag(name string) (string, bool) {
	name = cleanFrontendPath(name)
	if tag, ok := f.etags[name]; ok {
		return tag, true
	}

	indexName := cleanFrontendPath(filepath.ToSlash(filepath.Join(name, "index.html")))
	tag, ok := f.etags[indexName]
	return tag, ok
}

func (f *frontendFile) Read(p []byte) (int, error) {
	return f.File.Read(p)
}

func (f *frontendFile) Seek(offset int64, whence int) (int64, error) {
	if f.seeker == nil {
		return 0, fmt.Errorf("file does not implement io.ReadSeeker")
	}
	return f.seeker.Seek(offset, whence)
}

func (f *frontendFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return frontendFileInfo{FileInfo: info, buildTime: f.buildTime}, nil
}

func (f frontendFileInfo) ModTime() time.Time {
	return f.buildTime
}

func makeETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `W/"sha256-` + hex.EncodeToString(sum[:]) + `"`
}

func cleanFrontendPath(name string) string {
	name = filepath.ToSlash(filepath.Clean(strings.TrimPrefix(name, "/")))
	if name == "." {
		return ""
	}
	return name
}

func frontendETagMiddleware(frontend *frontendFS) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			path := c.Request().URL.Path
			if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/auth/") {
				if etag, ok := frontend.ETag(path); ok {
					c.Response().Header().Set("ETag", etag)
				}
			}
			return next(c)
		}
	}
}
