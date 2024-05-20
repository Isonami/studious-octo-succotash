package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	header = `

import "embed"

//go:embed `

	footer = `
var content embed.FS
`
)

func main() {
	if len(os.Args) != 4 {
		log.Fatalln("Usage: generate go_package ./ui_path ./output_gen.go")
	}

	cmd := exec.Command("npm", "run", "build")
	cmd.Dir = os.Args[2]

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if _, ok := os.LookupEnv("SKIP_NPM"); !ok {
		err := cmd.Run()
		if err != nil {
			log.Fatalln(err)
		}
	}

	data := "package " + os.Args[1] + header + filepath.Join(os.Args[2], "build/*") + footer

	err := os.WriteFile(os.Args[3], []byte(data), 0644)
	if err != nil {
		log.Fatalln(err)
	}
}
