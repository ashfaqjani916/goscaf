package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
)

var (
	devBuildFlag   string
	devRunFlag     string
	devWatchFlag   []string
	devExcludeFlag []string
	devDelayFlag   time.Duration
)

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Start development server with hot reload",
	Long: `Watch for .go file changes and automatically rebuild and restart your application.

Run from the root of a goscaf-generated project:
  goscaf dev

Customize the build or run command with flags:
  goscaf dev --build "go build -race -o /tmp/myapp ./cmd/main.go"
  goscaf dev --watch ./internal --watch ./cmd --delay 300ms`,
	RunE: runDev,
}

func init() {
	rootCmd.AddCommand(devCmd)
	devCmd.Flags().StringVar(&devBuildFlag, "build", "", "Custom build command (default: go build -o <tmp> ./cmd/main.go)")
	devCmd.Flags().StringVar(&devRunFlag, "run", "", "Custom run command (default: <tmp-binary>)")
	devCmd.Flags().StringSliceVar(&devWatchFlag, "watch", []string{"."}, "Directories to watch for .go file changes")
	devCmd.Flags().StringSliceVar(&devExcludeFlag, "exclude", []string{".git", "vendor", "node_modules", "bin", "tmp"}, "Directory names to exclude from watching")
	devCmd.Flags().DurationVar(&devDelayFlag, "delay", 500*time.Millisecond, "Debounce delay before triggering a rebuild")
}

// devProc wraps a running subprocess and a channel that closes when it exits.
type devProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func startProc(args []string) (*devProc, error) {
	c := exec.Command(args[0], args[1:]...) //nolint:gosec
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = os.Environ()
	if err := c.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		_ = c.Wait()
		close(done)
	}()
	return &devProc{cmd: c, done: done}, nil
}

// stop sends an interrupt to the process and waits up to 3s before force-killing.
func (p *devProc) stop() {
	if p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// reloader debounces file-change events and manages the running process.
type reloader struct {
	mu    sync.Mutex
	timer *time.Timer
	proc  *devProc
	delay time.Duration
}

// schedule debounces calls to fn; only the last call within r.delay fires.
func (r *reloader) schedule(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(r.delay, fn)
}

// swap replaces the current process pointer and returns the old one (caller stops it).
func (r *reloader) swap(p *devProc) *devProc {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.proc
	r.proc = p
	return old
}

// shutdown stops the timer and the running process.
func (r *reloader) shutdown() {
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
	}
	p := r.proc
	r.proc = nil
	r.mu.Unlock()
	if p != nil {
		p.stop()
	}
}

func runDev(_ *cobra.Command, _ []string) error {
	if _, err := os.Stat("cmd/main.go"); os.IsNotExist(err) {
		return fmt.Errorf("cmd/main.go not found — run goscaf dev from the root of a goscaf project")
	}

	binPath := filepath.Join(os.TempDir(), fmt.Sprintf("goscaf-dev-%d", os.Getpid()))
	defer os.Remove(binPath) //nolint:errcheck

	buildArgs := splitOrDefault(devBuildFlag, []string{"go", "build", "-o", binPath, "./cmd/main.go"})
	runArgs := splitOrDefault(devRunFlag, []string{binPath})

	doBuild := func() bool {
		c := exec.Command(buildArgs[0], buildArgs[1:]...) //nolint:gosec
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			color.HiRed("  ✗ Build failed")
			return false
		}
		return true
	}

	rl := &reloader{delay: devDelayFlag}

	rebuild := func() {
		color.HiCyan("  → Rebuilding...")
		if old := rl.swap(nil); old != nil {
			old.stop()
		}
		if !doBuild() {
			return
		}
		color.HiGreen("  ✓ Build succeeded, restarting...")
		p, err := startProc(runArgs)
		if err != nil {
			color.HiRed("  ✗ Failed to start: %v", err)
			return
		}
		if old := rl.swap(p); old != nil {
			old.stop()
		}
	}

	// Initial build and start
	color.HiCyan("  → Building...")
	if doBuild() {
		color.HiGreen("  ✓ Build succeeded, starting...")
		p, err := startProc(runArgs)
		if err != nil {
			color.HiRed("  ✗ Failed to start: %v", err)
		} else {
			rl.swap(p) //nolint:errcheck
		}
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}
	defer watcher.Close()

	for _, dir := range devWatchFlag {
		if err := addWatchRecursive(watcher, dir, devExcludeFlag); err != nil {
			return fmt.Errorf("failed to watch %s: %w", dir, err)
		}
	}

	fmt.Println()
	color.HiCyan("  → Watching for changes... (Ctrl+C to stop)")
	fmt.Println()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// Auto-watch newly created sub-directories
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					_ = addWatchRecursive(watcher, event.Name, devExcludeFlag)
				}
			}
			if !strings.HasSuffix(event.Name, ".go") {
				continue
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				rel, _ := filepath.Rel(".", event.Name)
				ts := time.Now().Format("15:04:05")
				color.HiYellow("  [%s] changed: %s", ts, rel)
				rl.schedule(rebuild)
			}

		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			color.HiRed("  ✗ Watcher error: %v", watchErr)

		case <-sigCh:
			fmt.Println()
			color.HiYellow("  → Shutting down...")
			rl.shutdown()
			return nil
		}
	}
}

func splitOrDefault(flag string, defaults []string) []string {
	if flag == "" {
		return defaults
	}
	return strings.Fields(flag)
}

func addWatchRecursive(watcher *fsnotify.Watcher, root string, exclude []string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible paths silently
		}
		if !info.IsDir() {
			return nil
		}
		for _, ex := range exclude {
			if filepath.Base(path) == ex {
				return filepath.SkipDir
			}
		}
		return watcher.Add(path)
	})
}
