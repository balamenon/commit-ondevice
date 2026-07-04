package localmodel

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/msfoundry/commit/store"
)

type ModelStatus struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Cached      bool   `json:"cached"`
	Downloading bool   `json:"downloading"`
	Bytes       int64  `json:"bytes"`
	TotalBytes  int64  `json:"total_bytes"`
}

type Status struct {
	Phase         string        `json:"phase"`
	Ready         bool          `json:"ready"`
	Detail        string        `json:"detail"`
	Error         string        `json:"error"`
	ServerRunning bool          `json:"server_running"`
	ServerPID     int           `json:"server_pid"`
	Models        []ModelStatus `json:"models"`
}

type Manager struct {
	mu           sync.RWMutex
	once         sync.Once
	status       Status
	models       []ModelStatus
	serverCmd    *exec.Cmd
	serverExited bool
	lastOutput   string
}

func NewManager() *Manager {
	models := []ModelStatus{
		{ID: envOrDefault("COMMIT_LLM_MODEL", store.DefaultModel), Label: "Gemma 4 12B", TotalBytes: 11 * 1024 * 1024 * 1024},
		{ID: envOrDefault("COMMIT_LLM_DRAFT_MODEL", store.DefaultDraftModel), Label: "MTP draft model", TotalBytes: 270 * 1024 * 1024},
		{ID: envOrDefault("COMMIT_EMBEDDING_MODEL", store.DefaultEmbeddingModel), Label: "EmbeddingGemma", TotalBytes: 213 * 1024 * 1024},
	}
	m := &Manager{models: models}
	m.setStatus(func(s *Status) {
		s.Phase = "idle"
		s.Detail = "Preparing local models"
		s.Models = m.snapshotModels()
	})
	return m
}

func (m *Manager) Start(ctx context.Context) {
	m.once.Do(func() {
		go m.run(ctx)
	})
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.models {
		m.models[i].Cached = isModelCached(m.models[i].ID)
		m.models[i].Bytes = dirSize(modelCachePath(m.models[i].ID))
	}
	m.status.Models = append([]ModelStatus(nil), m.models...)
	m.status.ServerRunning = m.serverRunningLocked()
	if m.serverCmd != nil && m.serverCmd.Process != nil {
		m.status.ServerPID = m.serverCmd.Process.Pid
	}
	return m.status
}

func (m *Manager) run(ctx context.Context) {
	m.setPhase("installing", "Checking local AI runtime", "")
	if err := m.ensureRuntime(ctx); err != nil {
		m.setPhase("error", "Local AI runtime setup failed", err.Error())
		return
	}

	m.setPhase("checking", "Checking local model cache", "")
	downloader, argsPrefix, err := findHuggingFaceDownloader()
	if err != nil {
		m.setPhase("error", "Hugging Face downloader is not installed", err.Error())
		return
	}

	for i := range m.models {
		model := m.models[i].ID
		if model == "" || model == "none" {
			continue
		}
		if isModelCached(model) {
			m.updateModel(i, false)
			continue
		}
		if err := m.waitForExternalDownload(ctx, i); err != nil {
			m.setPhase("error", "Model download was interrupted", err.Error())
			return
		}
		if isModelCached(model) {
			m.updateModel(i, false)
			continue
		}
		if err := m.downloadModel(ctx, downloader, argsPrefix, i); err != nil {
			m.setPhase("error", "Model download failed", err.Error())
			return
		}
	}

	m.setPhase("starting_server", "Starting local Gemma server", "")
	if err := m.startServer(ctx); err != nil {
		m.setPhase("error", "Local Gemma server failed to start", err.Error())
		return
	}
	m.setPhase("ready", "Local Gemma is ready", "")
	m.setStatus(func(s *Status) {
		s.Ready = true
	})
}

func (m *Manager) ensureRuntime(ctx context.Context) error {
	pipxPath, err := m.ensurePipx(ctx)
	if err != nil {
		return err
	}
	if err := m.ensureHuggingFaceCLI(ctx, pipxPath); err != nil {
		return err
	}
	if err := m.ensureMLXVLM(ctx, pipxPath); err != nil {
		return err
	}
	return nil
}

func (m *Manager) ensurePipx(ctx context.Context) (string, error) {
	if path := findExecutable("pipx"); path != "" {
		return path, nil
	}
	brew := findExecutable("brew")
	if brew == "" {
		return "", fmt.Errorf("pipx is required to install the local AI runtime, and Homebrew is not installed")
	}
	m.setPhase("installing", "Installing pipx", "")
	if err := m.runStatusCommand(ctx, "Installing pipx", brew, "install", "pipx"); err != nil {
		return "", err
	}
	if path := findExecutable("pipx"); path != "" {
		return path, nil
	}
	return "", fmt.Errorf("pipx installation finished, but pipx was not found")
}

func (m *Manager) ensureHuggingFaceCLI(ctx context.Context, pipxPath string) error {
	if findExecutable("hf") != "" {
		return nil
	}
	m.setPhase("installing", "Installing Hugging Face downloader", "")
	return m.runStatusCommand(ctx, "Installing Hugging Face downloader", pipxPath, "install", "huggingface-hub[hf_xet]")
}

func (m *Manager) ensureMLXVLM(ctx context.Context, pipxPath string) error {
	if m.mlxVLMHealthy(ctx) {
		return nil
	}
	m.setPhase("installing", "Installing Gemma 4 MLX runtime", "")
	if err := m.runStatusCommand(ctx, "Installing Gemma 4 MLX runtime", pipxPath, "install", "--force", "mlx-vlm"); err != nil {
		return err
	}
	m.setPhase("installing", "Repairing Gemma 4 runtime dependencies", "")
	if err := m.runStatusCommand(ctx, "Repairing Gemma 4 runtime dependencies", pipxPath,
		"inject", "--force", "mlx-vlm", "transformers>=5.5,<5.13", "huggingface_hub>=1.0"); err != nil {
		return err
	}
	if !m.mlxVLMHealthy(ctx) {
		return fmt.Errorf("mlx_vlm.server is installed but failed its startup check: %s", m.lastOutput)
	}
	return nil
}

func (m *Manager) mlxVLMHealthy(ctx context.Context) bool {
	serverPath := findExecutable("mlx_vlm.server")
	if serverPath == "" {
		return false
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, serverPath, "--help")
	out, err := cmd.CombinedOutput()
	m.rememberOutput(string(out))
	return err == nil
}

func (m *Manager) runStatusCommand(ctx context.Context, detail, name string, args ...string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, name, args...)
	cmd.Env = append(os.Environ(), "PIPX_HOME="+filepath.Join(homeDir(), ".local", "pipx"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go m.scanOutput(stdout)
	go m.scanOutput(stderr)
	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("%s failed: %w: %s", detail, err, m.lastOutput)
	}
	return nil
}

func (m *Manager) waitForExternalDownload(ctx context.Context, idx int) error {
	model := m.models[idx].ID
	for isModelDownloadRunning(model) {
		m.setPhase("downloading", "Another process is downloading "+m.models[idx].Label, "")
		m.updateModel(idx, true)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
		if isModelCached(model) {
			m.updateModel(idx, false)
			return nil
		}
	}
	return nil
}

func (m *Manager) downloadModel(ctx context.Context, downloader string, argsPrefix []string, idx int) error {
	model := m.models[idx].ID
	m.setPhase("downloading", "Downloading "+m.models[idx].Label, "")
	m.updateModel(idx, true)

	args := append(append([]string{}, argsPrefix...), "download", model)
	cmdCtx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, downloader, args...)
	cmd.Env = append(os.Environ(), "HF_HUB_ENABLE_HF_TRANSFER=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	go m.scanOutput(stdout)
	go m.scanOutput(stderr)

	ticker := time.NewTicker(2 * time.Second)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.updateModel(idx, true)
		case err := <-done:
			m.updateModel(idx, false)
			if err != nil {
				return err
			}
			if !isModelCached(model) {
				return fmt.Errorf("%s did not appear in cache after download", model)
			}
			return nil
		}
	}
}

func (m *Manager) scanOutput(pipe any) {
	reader, ok := pipe.(interface {
		Read([]byte) (int, error)
	})
	if !ok {
		return
	}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		m.setStatus(func(s *Status) {
			s.Detail = line
		})
		m.rememberOutput(line)
	}
}

func (m *Manager) startServer(ctx context.Context) error {
	if tcpReachable("127.0.0.1:8080") {
		return nil
	}
	serverPath := findExecutable("mlx_vlm.server")
	if serverPath == "" {
		return fmt.Errorf("Gemma 4 MLX runtime is not installed")
	}

	cmd := exec.CommandContext(ctx, serverPath,
		"--model", envOrDefault("COMMIT_LLM_MODEL", store.DefaultModel),
		"--draft-model", envOrDefault("COMMIT_LLM_DRAFT_MODEL", store.DefaultDraftModel),
		"--draft-kind", "mtp",
		"--host", "127.0.0.1",
		"--port", "8080",
	)
	cmd.Env = os.Environ()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return err
	}

	m.mu.Lock()
	m.serverCmd = cmd
	m.serverExited = false
	m.mu.Unlock()

	go m.logPipe("mlx-vlm", stdout)
	go m.logPipe("mlx-vlm", stderr)

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		m.serverExited = true
		m.status.ServerRunning = false
		if err != nil && m.status.Phase != "ready" {
			m.status.Error = strings.TrimSpace(err.Error() + ": " + m.lastOutput)
		}
		m.mu.Unlock()
		done <- err
	}()

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		if tcpReachable("127.0.0.1:8080") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			if err != nil {
				return fmt.Errorf("%w: %s", err, m.lastOutput)
			}
			return fmt.Errorf("mlx_vlm.server exited before becoming reachable")
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("timed out waiting for mlx_vlm.server on 127.0.0.1:8080")
}

func (m *Manager) logPipe(prefix string, pipe any) {
	reader, ok := pipe.(interface {
		Read([]byte) (int, error)
	})
	if !ok {
		return
	}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		log.Printf("%s: %s", prefix, line)
		m.rememberOutput(line)
		m.setStatus(func(s *Status) {
			if s.Phase == "starting_server" {
				s.Detail = line
			}
		})
	}
}

func (m *Manager) rememberOutput(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len(text) > 1200 {
		text = text[len(text)-1200:]
	}
	m.mu.Lock()
	m.lastOutput = text
	m.mu.Unlock()
}

func (m *Manager) setPhase(phase, detail, errText string) {
	m.setStatus(func(s *Status) {
		s.Phase = phase
		s.Detail = detail
		s.Error = errText
		s.Ready = phase == "ready"
	})
}

func (m *Manager) updateModel(idx int, downloading bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models[idx].Downloading = downloading
	m.models[idx].Cached = isModelCached(m.models[idx].ID)
	m.models[idx].Bytes = dirSize(modelCachePath(m.models[idx].ID))
	m.status.Models = append([]ModelStatus(nil), m.models...)
}

func (m *Manager) setStatus(fn func(*Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.status)
	m.status.Models = append([]ModelStatus(nil), m.models...)
	m.status.ServerRunning = m.serverRunningLocked()
}

func (m *Manager) serverRunningLocked() bool {
	if tcpReachable("127.0.0.1:8080") {
		return true
	}
	return m.serverCmd != nil && !m.serverExited
}

func (m *Manager) snapshotModels() []ModelStatus {
	out := append([]ModelStatus(nil), m.models...)
	for i := range out {
		out[i].Cached = isModelCached(out[i].ID)
		out[i].Bytes = dirSize(modelCachePath(out[i].ID))
	}
	return out
}

func findHuggingFaceDownloader() (string, []string, error) {
	if path := findExecutable("hf"); path != "" {
		return path, nil, nil
	}
	if path := findExecutable("huggingface-cli"); path != "" {
		return path, nil, nil
	}
	return "", nil, fmt.Errorf("install Hugging Face Hub CLI first: pipx install 'huggingface-hub[hf_xet]'")
}

func findExecutable(name string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join("/opt/homebrew/bin", name),
		filepath.Join("/usr/local/bin", name),
		filepath.Join(home, ".local", "bin", name),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return candidate
		}
	}
	return ""
}

func isModelCached(repo string) bool {
	pattern := filepath.Join(modelCachePath(repo), "snapshots", "*")
	matches, err := filepath.Glob(pattern)
	return err == nil && len(matches) > 0
}

func modelCachePath(repo string) string {
	return filepath.Join(huggingFaceHubCacheDir(), "models--"+strings.ReplaceAll(repo, "/", "--"))
}

func huggingFaceHubCacheDir() string {
	if cache := os.Getenv("HUGGINGFACE_HUB_CACHE"); cache != "" {
		return cache
	}
	if hfHome := os.Getenv("HF_HOME"); hfHome != "" {
		return filepath.Join(hfHome, "hub")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "huggingface", "hub")
}

func isModelDownloadRunning(model string) bool {
	patterns := []string{
		"hf download " + model,
		"huggingface-cli download " + model,
	}
	for _, pattern := range patterns {
		if err := exec.Command("pgrep", "-f", pattern).Run(); err == nil {
			return true
		}
	}
	return false
}

func dirSize(path string) int64 {
	var total int64
	filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func tcpReachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
