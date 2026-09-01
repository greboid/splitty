package activity

// The Entry type and feed queries live in store.go; this file holds the
// feed page handler.

import (
	"net/http"

	"github.com/greboid/splitpayments/internal/auth"
	"github.com/greboid/splitpayments/internal/render"
)

// Handler serves GET /activity, the global feed for the logged-in user.
type Handler struct {
	Store  *Store
	Render *render.Renderer
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r)
	entries, err := h.Store.ForUser(u.ID, 100)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	h.Render.Render(w, http.StatusOK, "activity.html", struct {
		render.Common
		Entries []Entry
	}{
		Common:  h.Render.CommonFrom(r, &render.User{Name: u.Name, IsGuest: u.IsGuest}),
		Entries: entries,
	})
}
