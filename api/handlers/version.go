package handlers

import (
	"net/http"

	"github.com/datapointchris/ypl/api/wire"
)

// buildVersion is the release a server was built from, as its image's tag
// names it.
type buildVersion struct {
	Version string `json:"version"`
}

// showVersion answers the release this server was built from. A client
// refused for a field it sent reads it, to name both sides' versions.
func (h *Handlers) showVersion(w http.ResponseWriter, _ *http.Request) {
	wire.JSON(w, http.StatusOK, buildVersion{Version: h.version})
}
