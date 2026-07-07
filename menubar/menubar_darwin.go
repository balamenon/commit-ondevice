//go:build darwin && cgo

package menubar

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"
	"github.com/msfoundry/commit/localmodel"
	"github.com/msfoundry/commit/store"
)

const iconPNG = "iVBORw0KGgoAAAANSUhEUgAAABIAAAASCAYAAABWzo5XAAAAKklEQVR42mNgGIrgPxSPGoTfEIoM+48Dk62REKadQVTzGs0CezRlD1YAAErPP8G6acT3AAAAAElFTkSuQmCC"

func Enabled() bool {
	return true
}

func Quit() {
	systray.Quit()
}

func Run(ctx context.Context, cancel context.CancelFunc, dashboardURL string, db *store.DB, models *localmodel.Manager) {
	var quitOnce sync.Once
	requestQuit := func() {
		quitOnce.Do(func() {
			if models != nil {
				models.Stop()
			}
			cancel()
			systray.Quit()
		})
	}

	systray.Run(func() {
		onReady(ctx, requestQuit, dashboardURL, db, models)
	}, func() {
		if models != nil {
			models.Stop()
		}
		cancel()
	})
}

func onReady(ctx context.Context, requestQuit func(), dashboardURL string, db *store.DB, models *localmodel.Manager) {
	if icon, err := base64.StdEncoding.DecodeString(iconPNG); err == nil {
		systray.SetTemplateIcon(icon, icon)
	}
	systray.SetTitle("Commit")
	systray.SetTooltip("Commit is starting")

	statusItem := systray.AddMenuItem("Status: starting", "Current local model status")
	statusItem.Disable()
	modelItem := systray.AddMenuItem("Model: checking", "Current local generation model")
	modelItem.Disable()
	downloadItem := systray.AddMenuItem("Models: checking cache", "Download/cache status")
	downloadItem.Disable()
	systray.AddSeparator()

	openItem := systray.AddMenuItem("Open Dashboard", dashboardURL)
	modelMenu := systray.AddMenuItem("Switch Model", "Choose the local generation model")
	modelItems := map[string]*systray.MenuItem{}
	for _, option := range localmodel.ModelOptions() {
		item := modelMenu.AddSubMenuItem(option.Label+" - "+option.Description, option.ID)
		modelItems[option.ID] = item
	}
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quit Commit", "Stop Commit and unload the local model")

	type menuAction struct {
		kind  string
		model string
	}
	actions := make(chan menuAction, 8)
	go func() {
		for range openItem.ClickedCh {
			actions <- menuAction{kind: "open"}
		}
	}()
	for id, item := range modelItems {
		id := id
		item := item
		go func() {
			for range item.ClickedCh {
				actions <- menuAction{kind: "model", model: id}
			}
		}()
	}
	go func() {
		for range quitItem.ClickedCh {
			actions <- menuAction{kind: "quit"}
			return
		}
	}()

	update := func() {
		if models == nil {
			systray.SetTitle("Commit: no model")
			statusItem.SetTitle("Status: model manager unavailable")
			return
		}
		status := models.Status()
		title := "Commit: " + shortStatus(status)
		systray.SetTitle(title)
		systray.SetTooltip(statusTooltip(status))
		statusItem.SetTitle("Status: " + statusText(status))
		modelItem.SetTitle("Model: " + currentModelLabel(status, db))
		downloadItem.SetTitle(downloadText(status))
		for id, item := range modelItems {
			if id == currentModelID(status, db) {
				item.Check()
			} else {
				item.Uncheck()
			}
		}
	}

	update()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				systray.Quit()
				return
			case <-ticker.C:
				update()
			case action := <-actions:
				switch action.kind {
				case "open":
					if err := exec.Command("open", dashboardURL).Start(); err != nil {
						log.Printf("open dashboard from menubar: %v", err)
					}
				case "model":
					if models != nil {
						if err := models.SwitchModel(ctx, action.model); err != nil {
							log.Printf("switch model from menubar: %v", err)
						}
					}
					update()
				case "quit":
					systray.SetTitle("Commit: quitting")
					statusItem.SetTitle("Status: quitting")
					requestQuit()
					return
				}
			}
		}
	}()
}

func shortStatus(status localmodel.Status) string {
	switch status.Phase {
	case "ready":
		return "ready"
	case "downloading":
		for _, model := range status.Models {
			if model.Downloading {
				if model.TotalBytes > 0 {
					return fmt.Sprintf("%d%%", int(float64(model.Bytes)/float64(model.TotalBytes)*100))
				}
				return "downloading"
			}
		}
		return "downloading"
	case "installing":
		return "installing"
	case "starting_server":
		return "starting"
	case "error":
		return "error"
	case "stopped":
		return "stopped"
	default:
		return "preparing"
	}
}

func statusText(status localmodel.Status) string {
	if status.Error != "" {
		return status.Error
	}
	if status.Detail != "" {
		return status.Detail
	}
	return shortStatus(status)
}

func statusTooltip(status localmodel.Status) string {
	parts := []string{"Commit"}
	if label := strings.TrimSpace(status.CurrentLabel); label != "" {
		parts = append(parts, label)
	}
	if detail := strings.TrimSpace(statusText(status)); detail != "" {
		parts = append(parts, detail)
	}
	return strings.Join(parts, " - ")
}

func currentModelID(status localmodel.Status, db *store.DB) string {
	if status.CurrentModel != "" {
		return status.CurrentModel
	}
	if db != nil {
		return db.GetModel()
	}
	return ""
}

func currentModelLabel(status localmodel.Status, db *store.DB) string {
	if status.CurrentLabel != "" {
		return status.CurrentLabel
	}
	id := currentModelID(status, db)
	for _, option := range localmodel.ModelOptions() {
		if option.ID == id {
			return option.Label
		}
	}
	return id
}

func downloadText(status localmodel.Status) string {
	if len(status.Models) == 0 {
		return "Models: no downloads"
	}
	for _, model := range status.Models {
		if model.Downloading {
			if model.TotalBytes > 0 {
				pct := int(float64(model.Bytes) / float64(model.TotalBytes) * 100)
				return fmt.Sprintf("Downloading %s: %d%% (%s / %s)", model.Label, pct, formatBytes(model.Bytes), formatBytes(model.TotalBytes))
			}
			return "Downloading " + model.Label
		}
	}
	for _, model := range status.Models {
		if !model.Cached {
			return "Waiting: " + model.Label
		}
	}
	return "Models: cached"
}

func formatBytes(bytes int64) string {
	const unit = 1000
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
