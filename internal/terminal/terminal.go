package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Target represents a terminal injection target.
type Target struct {
	Type   string // "tmux", "pts"
	Target string // tmux pane (session:window.pane) or /dev/ttysNNN
}

func (t *Target) String() string {
	return fmt.Sprintf("%s:%s", t.Type, t.Target)
}

// Injector handles terminal input injection.
type Injector struct {
	log     Logger
	mu      sync.RWMutex
	tmuxBin string
	tmuxEnv string
}

// Logger is a minimal logging interface.
type Logger interface {
	Info(format string, args ...any)
	Debug(format string, args ...any)
	Error(format string, args ...any)
}

// New creates a new terminal injector.
func New(log Logger) *Injector {
	i := &Injector{log: log}
	i.resolveTmuxBin()
	i.resolveTmuxEnv()
	return i
}

func (i *Injector) resolveTmuxBin() {
	if path, err := exec.LookPath("tmux"); err == nil && path != "" {
		i.tmuxBin = path
		return
	}
	candidates := []string{
		"/opt/homebrew/bin/tmux",
		"/usr/local/bin/tmux",
		"/usr/bin/tmux",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			i.tmuxBin = p
			return
		}
	}
	i.tmuxBin = "tmux"
}

func (i *Injector) resolveTmuxEnv() {
	if v := os.Getenv("TMUX"); v != "" {
		i.tmuxEnv = v
		return
	}
	out, err := exec.Command("sh", "-c", "echo $TMUX").Output()
	if err == nil {
		i.tmuxEnv = strings.TrimSpace(string(out))
	}
}

// ResolveTarget finds the terminal target for a given PID.
// It walks up the process tree looking for a TTY, then tries to map it to a tmux pane.
func (i *Injector) ResolveTarget(startPid int) (*Target, error) {
	// Strategy 0: explicit env override.
	if explicit := os.Getenv("LARKER_TMUX_TARGET"); explicit != "" {
		return &Target{Type: "tmux", Target: explicit}, nil
	}

	// Strategy 1: walk process tree to find TTY.
	pid := startPid
	var ptsDevice string
	for range 10 {
		tty := i.getTtyByPid(pid)
		if tty != "" {
			ptsDevice = tty
			break
		}
		ppid, err := i.getPpid(pid)
		if err != nil || ppid <= 1 {
			break
		}
		pid = ppid
	}

	if ptsDevice == "" {
		return nil, fmt.Errorf("no terminal device found for pid %d", startPid)
	}

	// Strategy 2: try to find tmux pane matching this pts device.
	if pane := i.findTmuxPaneByPts(ptsDevice); pane != "" {
		return &Target{Type: "tmux", Target: pane}, nil
	}

	// Fallback: raw pts device.
	return &Target{Type: "pts", Target: ptsDevice}, nil
}

func (i *Injector) getTtyByPid(pid int) string {
	out, err := exec.Command("ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	tty := strings.TrimSpace(string(out))
	if tty == "" || tty == "?" || tty == "??" {
		return ""
	}
	devPath := tty
	if !strings.HasPrefix(tty, "/dev/") {
		devPath = "/dev/" + tty
	}
	if strings.HasPrefix(devPath, "/dev/ttys") || strings.HasPrefix(devPath, "/dev/pts/") {
		return devPath
	}
	return ""
}

func (i *Injector) getPpid(pid int) (int, error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	return ppid, nil
}

func (i *Injector) findTmuxPaneByPts(ptsDevice string) string {
	cmd := exec.Command(i.tmuxBin, "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}")
	cmd.Env = i.tmuxEnvEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 && parts[0] == ptsDevice {
			return parts[1]
		}
	}
	return ""
}

// InjectKeys sends a key sequence to the target terminal.
func (i *Injector) InjectKeys(target *Target, keys string) error {
	if target == nil {
		return fmt.Errorf("nil target")
	}
	switch target.Type {
	case "tmux":
		return i.injectViaTmux(target.Target, keys)
	case "pts":
		// On macOS, pts injection is unreliable; try tmux fallback.
		if pane := i.findTmuxPaneByPts(target.Target); pane != "" {
			return i.injectViaTmux(pane, keys)
		}
		return fmt.Errorf("pts injection not supported on this platform, please run inside tmux")
	default:
		return fmt.Errorf("unknown target type: %s", target.Type)
	}
}

// InjectText sends text followed by Enter.
func (i *Injector) InjectText(target *Target, text string) error {
	return i.InjectKeys(target, text+"\r")
}

func (i *Injector) injectViaTmux(target string, keys string) error {
	parts := i.mapKeysToTmux(keys)
	args := append([]string{"send-keys", "-t", target}, parts...)
	cmd := exec.Command(i.tmuxBin, args...)
	cmd.Env = i.tmuxEnvEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux send-keys failed: %w, output: %s", err, string(out))
	}
	if i.log != nil {
		i.log.Debug("tmux injected to %s: %q", target, keys)
	}
	return nil
}

// mapKeysToTmux converts a raw key string into tmux send-keys arguments.
func (i *Injector) mapKeysToTmux(keys string) []string {
	var parts []string
	j := 0
	for j < len(keys) {
		// ANSI 3-byte sequences.
		if j+3 <= len(keys) {
			seq3 := keys[j : j+3]
			switch seq3 {
			case "\x1b[A":
				parts = append(parts, "Up")
				j += 3
				continue
			case "\x1b[B":
				parts = append(parts, "Down")
				j += 3
				continue
			case "\x1b[C":
				parts = append(parts, "Right")
				j += 3
				continue
			case "\x1b[D":
				parts = append(parts, "Left")
				j += 3
				continue
			}
		}
		ch := keys[j]
		switch ch {
		case '\n', '\r':
			parts = append(parts, "Enter")
		case '\x1b':
			parts = append(parts, "Escape")
		case '\t':
			parts = append(parts, "Tab")
		case ' ':
			parts = append(parts, "Space")
		default:
			if ch >= 0x80 {
				// Multi-byte UTF-8 character: decode full rune as a single argument.
				r, size := utf8.DecodeRuneInString(keys[j:])
				parts = append(parts, string(r))
				j += size
				continue
			}
			parts = append(parts, string(ch))
		}
		j++
	}
	return parts
}

func (i *Injector) tmuxEnvEnv() []string {
	if i.tmuxEnv != "" {
		return append(os.Environ(), "TMUX="+i.tmuxEnv)
	}
	return os.Environ()
}

