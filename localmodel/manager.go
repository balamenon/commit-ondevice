package localmodel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	CurrentModel  string        `json:"current_model"`
	CurrentLabel  string        `json:"current_label"`
	Models        []ModelStatus `json:"models"`
}

type ModelOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type Manager struct {
	mu              sync.RWMutex
	once            sync.Once
	db              *store.DB
	cancel          context.CancelFunc
	runDone         chan struct{}
	status          Status
	models          []ModelStatus
	serverCmd       *exec.Cmd
	embeddingCmd    *exec.Cmd
	serverExited    bool
	embeddingExited bool
	lastOutput      string
}

func ModelOptions() []ModelOption {
	return []ModelOption{
		{ID: store.Gemma412BModel, Label: "Gemma 4 12B", Description: "Largest local mode; uses the MTP draft model when no draft override is set"},
		{ID: store.Qwen3VL2BModel, Label: "Qwen3-VL 2B", Description: "Smaller Qwen VLM option for lower memory use"},
		{ID: store.SmolVLM256MModel, Label: "SmolVLM 256M", Description: "Tiny VLM option for the lowest footprint"},
	}
}

func NewManager(db *store.DB) *Manager {
	model := envOrDefault("COMMIT_LLM_MODEL", defaultModel(db))
	m := &Manager{db: db, models: modelsFor(model)}
	m.setStatus(func(s *Status) {
		s.Phase = "idle"
		s.Detail = "Preparing local models"
		s.CurrentModel = model
		s.CurrentLabel = modelLabel(model)
	})
	return m
}

func defaultModel(db *store.DB) string {
	if db == nil {
		return store.DefaultModel
	}
	return db.GetModel()
}

func modelStatus(id string) ModelStatus {
	return ModelStatus{
		ID:         id,
		Label:      modelLabel(id),
		TotalBytes: estimatedModelBytes(id),
	}
}

func modelLabel(id string) string {
	lower := strings.ToLower(id)
	switch {
	case strings.Contains(lower, "embeddinggemma"):
		return "EmbeddingGemma"
	case strings.Contains(lower, "assistant") || strings.Contains(lower, "draft"):
		return "MTP draft model"
	case strings.Contains(lower, "gemma-4") && strings.Contains(lower, "e2b"):
		return "Gemma 4 E2B"
	case strings.Contains(lower, "gemma-4") && strings.Contains(lower, "e4b"):
		return "Gemma 4 E4B"
	case strings.Contains(lower, "gemma-3n") && strings.Contains(lower, "e2b"):
		return "Gemma 3n E2B"
	case strings.Contains(lower, "gemma-3n") && strings.Contains(lower, "e4b"):
		return "Gemma 3n E4B"
	case strings.Contains(lower, "qwen3-vl") && strings.Contains(lower, "2b"):
		return "Qwen3-VL 2B"
	case strings.Contains(lower, "smolvlm") && strings.Contains(lower, "256m"):
		return "SmolVLM 256M"
	case strings.Contains(lower, "12b"):
		return "Gemma 4 12B"
	}
	parts := strings.Split(id, "/")
	return parts[len(parts)-1]
}

func estimatedModelBytes(id string) int64 {
	lower := strings.ToLower(id)
	switch {
	case id == "" || id == "none":
		return 0
	case strings.Contains(lower, "embeddinggemma"):
		return mb(213)
	case strings.Contains(lower, "assistant") || strings.Contains(lower, "draft"):
		return mb(270)
	case strings.Contains(lower, "gemma-4") && strings.Contains(lower, "e2b"):
		return mb(3550)
	case strings.Contains(lower, "gemma-4") && strings.Contains(lower, "e4b"):
		return mb(5150)
	case strings.Contains(lower, "gemma-3n") && strings.Contains(lower, "e2b"):
		return mb(4000)
	case strings.Contains(lower, "gemma-3n") && strings.Contains(lower, "e4b"):
		return mb(5820)
	case strings.Contains(lower, "qwen3-vl") && strings.Contains(lower, "2b"):
		return mb(1500)
	case strings.Contains(lower, "smolvlm") && strings.Contains(lower, "256m"):
		return mb(300)
	case strings.Contains(lower, "12b"):
		return gb(11)
	}
	return 0
}

func modelsFor(model string) []ModelStatus {
	draftModel := selectedDraftModel(model)
	embeddingModel := envOrDefault("COMMIT_EMBEDDING_MODEL", store.DefaultEmbeddingModel)
	models := []ModelStatus{modelStatus(model)}
	if draftModel != "" && draftModel != "none" {
		models = append(models, modelStatus(draftModel))
	}
	return append(models, modelStatus(embeddingModel))
}

func selectedDraftModel(model string) string {
	if draftModel := os.Getenv("COMMIT_LLM_DRAFT_MODEL"); draftModel != "" {
		return draftModel
	}
	return store.DefaultDraftForModel(model)
}

func mb(n int64) int64 {
	return n * 1000 * 1000
}

func gb(n int64) int64 {
	return n * 1000 * 1000 * 1000
}

func (m *Manager) Start(ctx context.Context) {
	m.once.Do(func() {
		m.startRun(ctx)
	})
}

func (m *Manager) SwitchModel(ctx context.Context, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("model is required")
	}
	if !store.SupportedGenerationModel(model) {
		return fmt.Errorf("%s is currently disabled because upstream mlx-vlm fails to load its audio-tower weights", modelLabel(model))
	}
	if os.Getenv("COMMIT_LLM_MODEL") != "" {
		return fmt.Errorf("model is fixed by COMMIT_LLM_MODEL")
	}
	if m.db != nil {
		if err := m.db.SetModel(model); err != nil {
			return err
		}
	}
	m.Stop()
	m.mu.Lock()
	m.models = modelsFor(model)
	m.status = Status{
		Phase:        "idle",
		Detail:       "Preparing local models",
		CurrentModel: model,
		CurrentLabel: modelLabel(model),
	}
	m.status.Models = m.snapshotModelsLocked()
	m.mu.Unlock()
	m.startRun(ctx)
	return nil
}

func (m *Manager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	runDone := m.runDone
	m.runDone = nil
	cmd := m.serverCmd
	m.serverCmd = nil
	embeddingCmd := m.embeddingCmd
	m.embeddingCmd = nil
	m.serverExited = true
	m.embeddingExited = true
	m.status.ServerRunning = false
	m.status.ServerPID = 0
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if runDone != nil {
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
		}
	}
	if cmd != nil && cmd.Process != nil {
		go func() {
			time.Sleep(2 * time.Second)
			_ = cmd.Process.Kill()
		}()
	} else {
		stopExternalServerOnPort("8080")
	}
	if embeddingCmd != nil && embeddingCmd.Process != nil {
		go func() {
			time.Sleep(2 * time.Second)
			_ = embeddingCmd.Process.Kill()
		}()
	} else {
		stopExternalServerOnPort("8081")
	}
	m.setPhase("stopped", "Local model server is stopped", "")
}

func (m *Manager) Restart(ctx context.Context) {
	model := envOrDefault("COMMIT_LLM_MODEL", defaultModel(m.db))
	m.Stop()
	m.mu.Lock()
	m.models = modelsFor(model)
	m.status = Status{
		Phase:        "idle",
		Detail:       "Preparing local models",
		CurrentModel: model,
		CurrentLabel: modelLabel(model),
	}
	m.status.Models = m.snapshotModelsLocked()
	m.mu.Unlock()
	m.startRun(ctx)
}

func (m *Manager) startRun(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	runDone := make(chan struct{})
	m.cancel = cancel
	m.runDone = runDone
	m.status.Error = ""
	m.status.Ready = false
	m.mu.Unlock()
	go func() {
		defer close(runDone)
		m.run(runCtx)
	}()
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
	} else {
		m.status.ServerPID = 0
	}
	return m.status
}

func (m *Manager) run(ctx context.Context) {
	m.setPhase("installing", "Checking local AI runtime", "")
	if err := m.ensureRuntime(ctx); err != nil {
		if ctx.Err() != nil {
			m.setPhase("stopped", "Local model server is stopped", "")
			return
		}
		m.setPhase("error", "Local AI runtime setup failed", err.Error())
		return
	}

	m.setPhase("checking", "Checking local model cache", "")
	downloader, argsPrefix, err := findHuggingFaceDownloader()
	if err != nil {
		if ctx.Err() != nil {
			m.setPhase("stopped", "Local model server is stopped", "")
			return
		}
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
			if ctx.Err() != nil {
				m.setPhase("stopped", "Local model server is stopped", "")
				return
			}
			m.setPhase("error", "Model download was interrupted", err.Error())
			return
		}
		if isModelCached(model) {
			m.updateModel(i, false)
			continue
		}
		if err := m.downloadModel(ctx, downloader, argsPrefix, i); err != nil {
			if ctx.Err() != nil {
				m.setPhase("stopped", "Local model server is stopped", "")
				return
			}
			m.setPhase("error", "Model download failed", err.Error())
			return
		}
	}

	m.setPhase("starting_embeddings", "Starting local embedding server", "")
	if err := m.startEmbeddingServer(ctx); err != nil {
		if ctx.Err() != nil {
			m.setPhase("stopped", "Local model server is stopped", "")
			return
		}
		m.setPhase("error", "Local embedding server failed to start", err.Error())
		return
	}
	m.setPhase("starting_server", "Starting local Gemma server", "")
	if err := m.startServer(ctx); err != nil {
		if ctx.Err() != nil {
			m.setPhase("stopped", "Local model server is stopped", "")
			return
		}
		m.setPhase("error", "Local Gemma server failed to start", err.Error())
		return
	}
	m.setPhase("ready", "Local Gemma and embeddings are ready", "")
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
	if err := m.ensureEmbeddingRuntime(ctx, pipxPath); err != nil {
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
	return m.runStatusCommand(ctx, "Installing Hugging Face downloader", pipxPath, pipxInstallArgs("huggingface-hub[hf_xet]")...)
}

func (m *Manager) ensureMLXVLM(ctx context.Context, pipxPath string) error {
	if !m.mlxVLMHealthy(ctx) {
		m.setPhase("installing", "Installing local MLX runtime", "")
		if err := m.runStatusCommand(ctx, "Installing local MLX runtime", pipxPath, pipxInstallArgs("--force", "mlx-vlm")...); err != nil {
			return err
		}
	}
	if !m.mlxVLMDependenciesHealthy(ctx) {
		m.setPhase("installing", "Repairing local MLX runtime dependencies", "")
		if err := m.runStatusCommand(ctx, "Repairing local MLX runtime dependencies", pipxPath,
			"inject", "--force", "mlx-vlm", "transformers==5.5.0", "huggingface_hub>=1.0", "torch", "torchvision"); err != nil {
			return err
		}
	}
	if !m.mlxVLMHealthy(ctx) {
		return fmt.Errorf("mlx_vlm.server is installed but failed its startup check: %s", m.lastOutput)
	}
	return nil
}

func (m *Manager) ensureEmbeddingRuntime(ctx context.Context, pipxPath string) error {
	if m.embeddingRuntimeHealthy(ctx) {
		return nil
	}
	m.setPhase("installing", "Installing local embedding runtime", "")
	if err := m.runStatusCommand(ctx, "Installing local embedding runtime", pipxPath,
		"inject", "--force", "mlx-vlm", "mlx-embeddings", "fastapi", "uvicorn"); err != nil {
		return err
	}
	if !m.embeddingRuntimeHealthy(ctx) {
		return fmt.Errorf("local embedding runtime is installed but failed its startup check: %s", m.lastOutput)
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

func (m *Manager) mlxVLMDependenciesHealthy(ctx context.Context) bool {
	pythonPath := mlxVLMVenvPython()
	if pythonPath == "" {
		return false
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	script := `import importlib.metadata as md
import torch, torchvision
version = md.version("transformers")
raise SystemExit(0 if version == "5.5.0" else 1)
`
	cmd := exec.CommandContext(cmdCtx, pythonPath, "-c", script)
	out, err := cmd.CombinedOutput()
	m.rememberOutput(string(out))
	return err == nil
}

func (m *Manager) embeddingRuntimeHealthy(ctx context.Context) bool {
	pythonPath := mlxVLMVenvPython()
	if pythonPath == "" {
		return false
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, pythonPath, "-c", "import fastapi, uvicorn, mlx_embeddings")
	out, err := cmd.CombinedOutput()
	m.rememberOutput(string(out))
	return err == nil
}

func pipxInstallArgs(extra ...string) []string {
	args := []string{"install"}
	if python := preferredPipxPython(); python != "" {
		args = append(args, "--python", python, "--fetch-python", "missing")
	}
	return append(args, extra...)
}

func preferredPipxPython() string {
	candidates := []string{
		"python3.13",
		"python3.12",
		"python3.11",
		"/opt/homebrew/bin/python3.13",
		"/opt/homebrew/bin/python3.12",
		"/opt/homebrew/bin/python3.11",
		"/usr/local/bin/python3.13",
		"/usr/local/bin/python3.12",
		"/usr/local/bin/python3.11",
	}
	for _, candidate := range candidates {
		if path := findExecutable(candidate); path != "" {
			return path
		}
	}
	return "3.13"
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
		model := envOrDefault("COMMIT_LLM_MODEL", defaultModel(m.db))
		if err := localChatHealthCheck(ctx, model); err == nil {
			return nil
		} else {
			m.rememberOutput(err.Error())
			m.setPhase("starting_server", "Restarting unresponsive local Gemma server", err.Error())
			stopExternalServerOnPort("8080")
			deadline := time.Now().Add(10 * time.Second)
			for tcpReachable("127.0.0.1:8080") && time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(250 * time.Millisecond):
				}
			}
			if tcpReachable("127.0.0.1:8080") {
				return fmt.Errorf("local Gemma server is unhealthy and could not be restarted: %w", err)
			}
		}
	}
	serverPath := findExecutable("mlx_vlm.server")
	if serverPath == "" {
		return fmt.Errorf("local MLX runtime is not installed")
	}

	model := envOrDefault("COMMIT_LLM_MODEL", defaultModel(m.db))
	args := []string{"--model", model}
	if draftModel := selectedDraftModel(model); draftModel != "" && draftModel != "none" {
		args = append(args, "--draft-model", draftModel, "--draft-kind", envOrDefault("COMMIT_LLM_DRAFT_KIND", "mtp"))
	}
	args = append(args, "--host", "127.0.0.1", "--port", "8080")
	cmd := exec.CommandContext(ctx, serverPath, args...)
	cmd.Env = os.Environ()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return err
	}

	m.mu.Lock()
	m.serverCmd = cmd
	m.serverExited = false
	m.status.ServerPID = cmd.Process.Pid
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
	var lastHealthErr error
	for time.Now().Before(deadline) {
		if tcpReachable("127.0.0.1:8080") {
			if err := localChatHealthCheck(ctx, model); err == nil {
				return nil
			} else {
				lastHealthErr = err
				m.rememberOutput(err.Error())
				m.setStatus(func(s *Status) {
					s.Detail = "Waiting for local Gemma to finish loading"
					s.Error = err.Error()
				})
			}
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
	if lastHealthErr != nil {
		return fmt.Errorf("timed out waiting for local Gemma health check: %w", lastHealthErr)
	}
	return fmt.Errorf("timed out waiting for mlx_vlm.server on 127.0.0.1:8080")
}

func localChatHealthCheck(ctx context.Context, model string) error {
	body, err := json.Marshal(map[string]any{
		"model":       model,
		"max_tokens":  4,
		"temperature": 0,
		"stream":      false,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with ok."},
		},
	})
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST", "http://127.0.0.1:8080/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1200))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("local Gemma health check failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (m *Manager) startEmbeddingServer(ctx context.Context) error {
	embeddingModel := envOrDefault("COMMIT_EMBEDDING_MODEL", store.DefaultEmbeddingModel)
	if tcpReachable("127.0.0.1:8081") {
		if err := localEmbeddingHealthCheck(ctx, embeddingModel); err == nil {
			return nil
		} else {
			m.rememberOutput(err.Error())
			m.setPhase("starting_embeddings", "Restarting unresponsive local embedding server", err.Error())
			stopExternalServerOnPort("8081")
			deadline := time.Now().Add(10 * time.Second)
			for tcpReachable("127.0.0.1:8081") && time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(250 * time.Millisecond):
				}
			}
			if tcpReachable("127.0.0.1:8081") {
				return fmt.Errorf("local embedding server is unhealthy and could not be restarted: %w", err)
			}
		}
	}

	pythonPath := mlxVLMVenvPython()
	if pythonPath == "" {
		return fmt.Errorf("local embedding runtime is not installed")
	}
	scriptPath := embeddingServerScriptPath()
	if scriptPath == "" {
		return fmt.Errorf("embedding server script is missing")
	}

	args := []string{
		scriptPath,
		"--host", "127.0.0.1",
		"--port", "8081",
		"--model", embeddingModel,
	}
	cmd := exec.CommandContext(ctx, pythonPath, args...)
	cmd.Env = os.Environ()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return err
	}

	m.mu.Lock()
	m.embeddingCmd = cmd
	m.embeddingExited = false
	m.mu.Unlock()

	go m.logPipe("mlx-embeddings", stdout)
	go m.logPipe("mlx-embeddings", stderr)

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		m.embeddingExited = true
		if err != nil && m.status.Phase != "ready" {
			m.status.Error = strings.TrimSpace(err.Error() + ": " + m.lastOutput)
		}
		m.mu.Unlock()
		done <- err
	}()

	deadline := time.Now().Add(10 * time.Minute)
	var lastHealthErr error
	for time.Now().Before(deadline) {
		if tcpReachable("127.0.0.1:8081") {
			if err := localEmbeddingHealthCheck(ctx, embeddingModel); err == nil {
				return nil
			} else {
				lastHealthErr = err
				m.rememberOutput(err.Error())
				m.setStatus(func(s *Status) {
					s.Detail = "Waiting for EmbeddingGemma to finish loading"
					s.Error = err.Error()
				})
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			if err != nil {
				return fmt.Errorf("%w: %s", err, m.lastOutput)
			}
			return fmt.Errorf("embedding server exited before embeddings became reachable")
		case <-time.After(2 * time.Second):
		}
	}
	if lastHealthErr != nil {
		return fmt.Errorf("timed out waiting for local embedding health check: %w", lastHealthErr)
	}
	return fmt.Errorf("timed out waiting for embedding server on 127.0.0.1:8081")
}

func localEmbeddingHealthCheck(ctx context.Context, model string) error {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"input": []string{"embedding health check"},
	})
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST", "http://127.0.0.1:8081/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1200))
		return fmt.Errorf("local embedding health check failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read embedding health response: %w", err)
	}
	var parsed embeddingHealthResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return fmt.Errorf("parse embedding health response: %w", err)
	}
	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return fmt.Errorf("local embedding health check returned no vector")
	}
	return nil
}

type embeddingHealthResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

func stopExternalServerOnPort(port string) {
	pids, err := listeningPIDs(port)
	if err != nil {
		return
	}
	for _, pid := range pids {
		cmdline, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
		if err != nil {
			continue
		}
		command := string(cmdline)
		if !strings.Contains(command, "mlx_vlm.server") && !strings.Contains(command, "mlx_lm.server") && !strings.Contains(command, "embedding_server.py") {
			continue
		}
		if proc, err := os.FindProcess(pid); err == nil {
			log.Printf("stopping stale local MLX server pid %d", pid)
			_ = proc.Kill()
		}
	}
}

func listeningPIDs(port string) ([]int, error) {
	out, err := exec.Command("lsof", "-nP", "-t", "-iTCP:"+port, "-sTCP:LISTEN").Output()
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
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
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotModelsLocked()
}

func (m *Manager) snapshotModelsLocked() []ModelStatus {
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

func mlxVLMVenvPython() string {
	serverPath := findExecutable("mlx_vlm.server")
	if serverPath != "" {
		pythonPath := filepath.Join(filepath.Dir(serverPath), "python")
		if info, err := os.Stat(pythonPath); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return pythonPath
		}
	}
	home, _ := os.UserHomeDir()
	pythonPath := filepath.Join(home, ".local", "pipx", "venvs", "mlx-vlm", "bin", "python")
	if info, err := os.Stat(pythonPath); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
		return pythonPath
	}
	return findExecutable("python3")
}

func embeddingServerScriptPath() string {
	exe, err := os.Executable()
	if err == nil {
		candidates := []string{
			filepath.Join(filepath.Dir(exe), "..", "Resources", "scripts", "embedding_server.py"),
			filepath.Join(filepath.Dir(exe), "scripts", "embedding_server.py"),
		}
		for _, candidate := range candidates {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				if abs, err := filepath.Abs(candidate); err == nil {
					return abs
				}
				return candidate
			}
		}
	}
	candidates := []string{
		filepath.Join("scripts", "embedding_server.py"),
		filepath.Join(".", "scripts", "embedding_server.py"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			if abs, err := filepath.Abs(candidate); err == nil {
				return abs
			}
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
