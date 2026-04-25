//go:build ignore

package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	frontend "github.com/isonami/studious-octo-succotash/frontend"
	"github.com/maxence-charriere/go-app/v10/pkg/app"
)

const (
	header = `

import "embed"

//go:embed `

	footer = `
var content embed.FS

var frontendBuildUnix int64 = `
)

func main() {
	if len(os.Args) != 4 {
		log.Fatalln("Usage: generate go_package ./ui_path ./output_gen.go")
	}

	outputDir := filepath.Join(os.Args[2], "build")
	if err := os.RemoveAll(outputDir); err != nil {
		log.Fatalln(err)
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "web"), 0o755); err != nil {
		log.Fatalln(err)
	}

	frontend.RegisterRoutes()
	handler := frontend.NewHandler()

	if err := app.GenerateStaticWebsite(outputDir, handler); err != nil {
		log.Fatalln(err)
	}

	cmd := exec.Command("go", "build", "-o", filepath.Join(outputDir, "web", "app.wasm"), "./frontend/cmd/wasm")
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalln(err)
	}

	if err := copyDir(filepath.Join(os.Args[2], "web"), filepath.Join(outputDir, "web")); err != nil {
		log.Fatalln(err)
	}

	data := "package " + os.Args[1] + header + filepath.Join(os.Args[2], "build/*") + footer + strconv.FormatInt(time.Now().UTC().Unix(), 10) + "\n"
	if err := os.WriteFile(os.Args[3], []byte(data), 0o644); err != nil {
		log.Fatalln(err)
	}
}

func copyDir(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(srcDir, entry.Name())
		dstPath := filepath.Join(dstDir, entry.Name())

		if entry.IsDir() {
			if err := os.MkdirAll(dstPath, 0o755); err != nil {
				return err
			}
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}

		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}

	return nil
}

func copyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err
}
