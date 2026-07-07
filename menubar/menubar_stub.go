//go:build !darwin || !cgo

package menubar

import (
	"context"

	"github.com/msfoundry/commit/localmodel"
	"github.com/msfoundry/commit/store"
)

func Enabled() bool {
	return false
}

func Quit() {}

func Run(ctx context.Context, cancel context.CancelFunc, dashboardURL string, db *store.DB, models *localmodel.Manager) {
}
