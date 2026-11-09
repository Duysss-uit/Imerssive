// Package config resolves on-disk locations used by the app.
package config

import (
	"os"
	"path/filepath"
	"runtime"
)

type Paths struct {
	DataDir   string // %APPDATA%/Immersive
	DB        string // SQLite file
	Sources   string // copies of uploaded documents
	Pages     string // rendered comic pages
	Chroma    string // vector store
	ModelsDir string // embedding model cache
	Logs      string // activity and model logs
	Images    string // illustrations drawn in chats
}

func Resolve() (Paths, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, err
	}
	if d := os.Getenv("IMMERSIVE_DATA_DIR"); d != "" {
		base = d
	} else {
		base = filepath.Join(base, "Immersive")
	}
	p := Paths{
		DataDir:   base,
		DB:        filepath.Join(base, "app.db"),
		Sources:   filepath.Join(base, "sources"),
		Pages:     filepath.Join(base, "pages"),
		Chroma:    filepath.Join(base, "chroma"),
		ModelsDir: filepath.Join(base, "models"),
		Logs:      filepath.Join(base, "logs"),
		Images:    filepath.Join(base, "images"),
	}
	for _, d := range []string{p.DataDir, p.Sources, p.Pages, p.Chroma, p.ModelsDir, p.Logs, p.Images} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Paths{}, err
		}
	}
	return p, nil
}

// FindWorker locates the Python worker directory (the one containing imm_worker/)
// and its virtualenv interpreter, searching next to the executable and the cwd.
func FindWorker() (dir, python string) {
	var roots []string
	if d := os.Getenv("IMM_WORKER_DIR"); d != "" {
		roots = append(roots, filepath.Dir(filepath.Clean(d)))
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		roots = append(roots, d, filepath.Join(d, ".."), filepath.Join(d, "..", ".."), filepath.Join(d, "..", "..", ".."))
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd, filepath.Join(wd, ".."))
	}
	for _, r := range roots {
		cand := filepath.Join(r, "worker")
		if _, err := os.Stat(filepath.Join(cand, "imm_worker", "__main__.py")); err == nil {
			dir, _ = filepath.Abs(cand)
			break
		}
	}
	python = "python"
	if dir != "" {
		venv := filepath.Join(dir, ".venv", "bin", "python")
		if runtime.GOOS == "windows" {
			venv = filepath.Join(dir, ".venv", "Scripts", "python.exe")
		}
		if _, err := os.Stat(venv); err == nil {
			python = venv
		}
	}
	return dir, python
}
