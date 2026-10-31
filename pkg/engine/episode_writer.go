package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── append-only 落盘 ────────────────────────────────────────────

// EpisodeWriter 是 Episode 的 append-only JSONL 落盘器（同日一文件、单条一行）。
// 只追加、不覆盖、不重写历史；写失败不阻断在线动作。
type EpisodeWriter struct {
	dir string

	mu        sync.Mutex
	file      *os.File
	path      string
	day       string
	seq       atomic.Uint64
	appendErr atomic.Uint64
}

// OpenEpisodeWriter 打开（并创建）Episode 目录；目录不可写即返回错误。
func OpenEpisodeWriter(dir string) (*EpisodeWriter, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("episode dir required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &EpisodeWriter{dir: dir, path: filepath.Join(dir, episodeFileName(time.Now()))}, nil
}

// episodeFileName 返回某日的 Episode 文件名（按日切分，避免单文件无限增长）。
func episodeFileName(now time.Time) string {
	return "episodes-" + now.UTC().Format("20060102") + ".jsonl"
}

// Path 返回当前（或今日）落盘文件路径。
func (w *EpisodeWriter) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path
}

// Dropped 返回写失败次数（观测用）。
func (w *EpisodeWriter) Dropped() uint64 { return w.appendErr.Load() }

// Append 追加一条 Episode（单行 JSON）。调用方无需处理错误语义：
// 失败只是丢一条样本，绝不阻断在线动作。
func (w *EpisodeWriter) Append(ep *Episode) error {
	if ep == nil {
		return nil
	}
	blob, err := json.Marshal(ep)
	if err != nil {
		w.appendErr.Add(1)
		return err
	}
	blob = append(blob, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rotateLocked(time.Now()); err != nil {
		w.appendErr.Add(1)
		return err
	}
	if _, err := w.file.Write(blob); err != nil {
		w.appendErr.Add(1)
		return err
	}
	w.seq.Add(1)
	return nil
}

// rotateLocked 按日切换文件（O_APPEND 保证重启后仍是追加而非覆盖）。
func (w *EpisodeWriter) rotateLocked(now time.Time) error {
	name := episodeFileName(now)
	if w.file != nil && w.day == name {
		return nil
	}
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	path := filepath.Join(w.dir, name)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.file = f
	w.path = path
	w.day = name
	return nil
}

// Close 关闭当前文件（引擎退出/测试清理用）。
func (w *EpisodeWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
