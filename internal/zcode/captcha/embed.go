package captcha

// embed.go — the happy-dom node solver ships INSIDE the binary: the whole
// captcha_node tree (solver.js + package.json + node_modules) is embedded
// and materialized to data/captcha_node on first use, so a release build
// is a single self-contained exe (node itself stays an external runtime).

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

//go:embed all:captcha_node
var solverAssets embed.FS

var (
	extractOnce sync.Once
	extractDir  string
	extractErr  error
)

// assetsSum fingerprints the embedded tree; extraction repeats only when a
// new build changes it.
func assetsSum() string {
	h := sha256.New()
	fs.WalkDir(solverAssets, "captcha_node", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		h.Write([]byte(p))
		f, err := solverAssets.Open(p)
		if err != nil {
			return nil
		}
		io.Copy(h, f)
		f.Close()
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

// ensureSolverDir materializes the embedded solver to data/captcha_node
// (skipped when the on-disk copy matches this build's fingerprint).
func ensureSolverDir() (string, error) {
	extractOnce.Do(func() {
		dir := filepath.Join("data", "captcha_node")
		marker := filepath.Join(dir, ".embedded-sha")
		sum := assetsSum()
		if b, err := os.ReadFile(marker); err == nil && string(b) == sum {
			if _, err := os.Stat(filepath.Join(dir, "solver.js")); err == nil {
				extractDir = dir
				return
			}
		}
		tmp := dir + ".tmp"
		os.RemoveAll(tmp)
		err := fs.WalkDir(solverAssets, "captcha_node", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel("captcha_node", p)
			dst := filepath.Join(tmp, rel)
			if d.IsDir() {
				return os.MkdirAll(dst, 0o755)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			src, err := solverAssets.Open(p)
			if err != nil {
				return err
			}
			defer src.Close()
			out, err := os.Create(dst)
			if err != nil {
				return err
			}
			defer out.Close()
			_, err = io.Copy(out, src)
			return err
		})
		if err != nil {
			extractErr = err
			return
		}
		if err := os.WriteFile(filepath.Join(tmp, ".embedded-sha"), []byte(sum), 0o644); err != nil {
			extractErr = err
			return
		}
		os.RemoveAll(dir)
		if err := os.Rename(tmp, dir); err != nil {
			extractErr = err
			return
		}
		extractDir = dir
	})
	return extractDir, extractErr
}
