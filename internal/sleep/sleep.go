package sleep

import (
	"strconv"
	"sync"

	"os/exec"

	"github.com/mnhkahn/gogogo/logger"
)

type Manager struct {
	log  *logger.Logger
	mu   sync.Mutex
	pids map[int]*exec.Cmd // activePID -> caffeinate process
}

func New(log *logger.Logger) *Manager {
	return &Manager{log: log, pids: make(map[int]*exec.Cmd)}
}

// SyncPids 同步当前需要阻止睡眠的 AI 进程 PID 列表。
// 新增的 PID 会启动对应的 caffeinate，已停止的 PID 会清理掉对应的 caffeinate。
func (m *Manager) SyncPids(activePids []int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	activeSet := make(map[int]struct{}, len(activePids))
	for _, pid := range activePids {
		activeSet[pid] = struct{}{}
	}

	// 停掉已经不在 activePids 里的
	for pid, cmd := range m.pids {
		if _, ok := activeSet[pid]; !ok {
			m.stopLocked(pid, cmd)
		}
	}

	// 为新增的 PID 启动 caffeinate
	for _, pid := range activePids {
		if _, ok := m.pids[pid]; !ok {
			m.startLocked(pid)
		}
	}
}

func (m *Manager) startLocked(pid int) {
	if pid <= 1 {
		return
	}
	cmd := exec.Command("caffeinate", "-dims", "-w", strconv.Itoa(pid))
	if err := cmd.Start(); err != nil {
		m.log.Warn("Failed to start caffeinate for pid %d: %v", pid, err)
		return
	}
	m.pids[pid] = cmd
	m.log.Info("Sleep prevention enabled for pid %d (caffeinate -dims -w %d)", pid, pid)
}

func (m *Manager) stopLocked(pid int, cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil {
			m.log.Warn("Failed to kill caffeinate for pid %d: %v", pid, err)
		}
		_, _ = cmd.Process.Wait()
	}
	delete(m.pids, pid)
	m.log.Info("Sleep prevention disabled for pid %d", pid)
}

// AllowSleep 停止所有 caffeinate。
func (m *Manager) AllowSleep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for pid, cmd := range m.pids {
		m.stopLocked(pid, cmd)
	}
}

func (m *Manager) IsActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pids) > 0
}
