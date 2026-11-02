// Package worker manages the Python ML sidecar (PDF parsing, embeddings, Chroma).
//
// The sidecar speaks newline-delimited JSON-RPC over stdin/stdout. It is started
// lazily on the first call and shut down after an idle period, so the embedding
// model only occupies RAM while documents are being ingested or searched.
package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

type Options struct {
	Python  string   // interpreter path
	Dir     string   // directory containing the imm_worker package
	Env     []string // extra KEY=VALUE pairs
	IdleFor time.Duration
}

// proc is one running sidecar process with its own in-flight requests.
type proc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan response
	done    chan struct{}
}

type Manager struct {
	opts Options

	mu       sync.Mutex
	cur      *proc
	nextID   int64
	inflight int
	idle     *time.Timer
}

type request struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *string         `json:"error"`
}

var ErrNotConfigured = errors.New("không tìm thấy Python worker (thư mục worker/ hoặc .venv)")

func New(opts Options) *Manager {
	if opts.IdleFor <= 0 {
		opts.IdleFor = 5 * time.Minute
	}
	return &Manager{opts: opts}
}

// Reconfigure applies new options; the running process (if any) is stopped
// and the next call starts one with the new settings.
func (m *Manager) Reconfigure(opts Options) {
	if opts.IdleFor <= 0 {
		opts.IdleFor = 5 * time.Minute
	}
	m.mu.Lock()
	m.opts = opts
	m.mu.Unlock()
	m.Stop()
}

// Running reports whether the sidecar process is alive.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur != nil
}

func (m *Manager) startLocked() (*proc, error) {
	if m.cur != nil {
		return m.cur, nil
	}
	if m.opts.Dir == "" {
		return nil, ErrNotConfigured
	}
	cmd := exec.Command(m.opts.Python, "-m", "imm_worker")
	cmd.Dir = m.opts.Dir
	cmd.Env = append(os.Environ(), m.opts.Env...)
	cmd.Env = append(cmd.Env, "PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8")
	hideWindow(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start worker: %w", err)
	}
	p := &proc{cmd: cmd, stdin: stdin, pending: map[int64]chan response{}, done: make(chan struct{})}
	m.cur = p
	log.Printf("worker: started pid %d", cmd.Process.Pid)

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("worker: %s", sc.Text())
		}
	}()
	go m.readLoop(p, stdout)
	return p, nil
}

func (m *Manager) readLoop(p *proc, stdout io.Reader) {
	rd := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			var resp response
			if json.Unmarshal(line, &resp) == nil {
				p.mu.Lock()
				ch := p.pending[resp.ID]
				delete(p.pending, resp.ID)
				p.mu.Unlock()
				if ch != nil {
					ch <- resp
				}
			}
		}
		if err != nil {
			break
		}
	}
	p.cmd.Wait()

	m.mu.Lock()
	if m.cur == p {
		m.cur = nil
	}
	m.mu.Unlock()

	msg := "worker exited unexpectedly (xem log)"
	p.mu.Lock()
	for id, ch := range p.pending {
		ch <- response{ID: id, Error: &msg}
		delete(p.pending, id)
	}
	p.mu.Unlock()
	close(p.done)
	log.Printf("worker: exited")
}

// Call invokes a worker method and decodes the result into out (may be nil).
func (m *Manager) Call(ctx context.Context, method string, params any, out any) error {
	m.mu.Lock()
	p, err := m.startLocked()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if m.idle != nil {
		m.idle.Stop()
	}
	m.nextID++
	id := m.nextID
	m.inflight++
	m.mu.Unlock()
	defer m.release()

	ch := make(chan response, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()

	b, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	_, err = p.stdin.Write(append(b, '\n'))
	p.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("worker write: %w", err)
	}

	select {
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("worker %s: %s", method, *resp.Error)
		}
		if out != nil {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}

func (m *Manager) release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflight--
	if m.inflight == 0 && m.cur != nil {
		m.idle = time.AfterFunc(m.opts.IdleFor, m.stopIfIdle)
	}
}

func (m *Manager) stopIfIdle() {
	m.mu.Lock()
	if m.inflight > 0 || m.cur == nil {
		m.mu.Unlock()
		return
	}
	p := m.cur
	m.cur = nil // the next Call starts a fresh process
	m.mu.Unlock()
	log.Printf("worker: idle, shutting down to free memory")
	p.shutdown()
}

// Stop terminates the sidecar (used on app exit).
func (m *Manager) Stop() {
	m.mu.Lock()
	p := m.cur
	m.cur = nil
	if m.idle != nil {
		m.idle.Stop()
	}
	m.mu.Unlock()
	if p != nil {
		p.shutdown()
	}
}

// shutdown closes stdin (the worker finishes queued work and exits on EOF)
// and kills it if it lingers.
func (p *proc) shutdown() {
	p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		p.cmd.Process.Kill()
	}
}
