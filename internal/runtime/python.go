package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
)

// PythonRunner executes original BMad skill scripts in an isolated temp
// directory materialized from the conversation's VFS. No shell is involved,
// the environment is minimal (no secrets), and runs are time-boxed.
type PythonRunner struct {
	Bin       string        // python executable; empty disables the runner
	Timeout   time.Duration // per run
	MaxOutput int           // bytes of stdout/stderr returned

	once    sync.Once
	checked bool
}

// NewPythonRunner detects a Python 3.11+ interpreter when bin is "auto".
func NewPythonRunner(bin string) *PythonRunner {
	r := &PythonRunner{Bin: bin, Timeout: 20 * time.Second, MaxOutput: 64 * 1024}
	if bin == "auto" {
		r.Bin = ""
		for _, c := range []string{"python3", "python"} {
			if p, err := exec.LookPath(c); err == nil {
				out, err := exec.Command(p, "-c", "import sys,tomllib;print(sys.version_info[0])").Output()
				if err == nil && strings.TrimSpace(string(out)) == "3" {
					r.Bin = p
					break
				}
			}
		}
	}
	return r
}

// Enabled reports whether scripts can run.
func (r *PythonRunner) Enabled() bool { return r != nil && r.Bin != "" }

// Run executes script (absolute virtual path) with args.
func (r *PythonRunner) Run(ctx context.Context, tc *TurnContext, script string, args []string) (string, error) {
	if !r.Enabled() {
		return "", errors.New("python indisponível")
	}
	tmp, err := os.MkdirTemp("", "alicerce-py-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	root := filepath.Join(tmp, "project")

	// Materialize the project view (bundle + overlay + workspace + docs).
	files, err := tc.FS.AllFiles()
	if err != nil {
		return "", err
	}
	original := map[string][]byte{}
	for _, abs := range files {
		data, err := tc.FS.Read(abs)
		if err != nil {
			continue
		}
		rel := strings.TrimPrefix(abs, vfs.Root+"/")
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "", err
		}
		if OutputRel(abs) != "" {
			original[abs] = data
		}
	}
	if err := os.MkdirAll(filepath.Join(root, OutputDir), 0o755); err != nil {
		return "", err
	}

	hostRoot := filepath.ToSlash(root)
	rewrite := func(s string) string { return strings.ReplaceAll(s, vfs.Root, hostRoot) }
	argv := []string{filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(script, vfs.Root+"/")))}
	for _, a := range args {
		argv = append(argv, rewrite(a))
	}

	cctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.Bin, argv...)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + tmp,
		"TMPDIR=" + tmp,
		"PYTHONIOENCODING=utf-8",
		"PYTHONUTF8=1",
		"PYTHONDONTWRITEBYTECODE=1",
		"LANG=C.UTF-8",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"), // required by Python on Windows
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{buf: &stdout, max: r.MaxOutput}
	cmd.Stderr = &limitedWriter{buf: &stderr, max: r.MaxOutput}
	runErr := cmd.Run()

	// Sync workspace changes back.
	outRoot := filepath.Join(root, OutputDir)
	seen := map[string]bool{}
	_ = filepath.WalkDir(outRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		abs := vfs.Root + "/" + filepath.ToSlash(rel)
		seen[abs] = true
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if old, ok := original[abs]; !ok || !bytes.Equal(old, data) {
			_, _ = tc.FS.Write(abs, data)
		}
		return nil
	})
	for abs := range original {
		if !seen[abs] {
			_ = tc.FS.Delete(abs)
		}
	}

	back := func(s string) string {
		s = strings.ReplaceAll(s, hostRoot, vfs.Root)
		return strings.ReplaceAll(s, root, vfs.Root)
	}
	out := back(stdout.String())
	if cctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("o script excedeu o tempo limite de %s", r.Timeout)
	}
	if runErr != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		}
		msg := strings.TrimSpace(back(stderr.String()))
		if msg == "" {
			msg = runErr.Error()
		}
		return "", fmt.Errorf("script terminou com código %d: %s", code, Truncate(msg, 4000))
	}
	if errOut := strings.TrimSpace(back(stderr.String())); errOut != "" {
		out = strings.TrimRight(out, "\n") + "\n[stderr]\n" + Truncate(errOut, 4000)
	}
	if strings.TrimSpace(out) == "" {
		return "(ok, sem saída)", nil
	}
	return Truncate(out, r.MaxOutput), nil
}

type limitedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}
