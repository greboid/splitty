package receipt

// HTTP handlers: POST /receipts (upload + image cleanup for scan.js),
// POST /receipts/{file}/analyze (vision LLM extraction) and
// GET /receipts/{file} (authorised image serving for expense pages).
// Uploading and analysing are separate steps so the client can reflect
// both stages in its status text; both act on the one stored file, which
// is the cleaned-up image.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/greboid/splitpayments/internal/auth"
	"github.com/greboid/splitpayments/internal/expense"
)

type Handlers struct {
	Store    *Store
	Client   *Client // nil when receipt scanning is disabled
	Expenses *expense.Store
}

type scanResponse struct {
	OK       bool    `json:"ok"`
	File     string  `json:"file,omitempty"`
	Merchant string  `json:"merchant,omitempty"`
	Date     string  `json:"date,omitempty"`
	Total    float64 `json:"total,omitempty"`
	Tax      float64 `json:"tax,omitempty"`
	Tip      float64 `json:"tip,omitempty"`
	Items    []Item  `json:"items,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// Upload handles the receipt image upload/camera capture: it validates the
// image, applies the cleanup pipeline (EXIF orientation, trim, deskew) and
// stores the result. Extraction is a separate Analyze call on the stored
// file.
func (h *Handlers) Upload(w http.ResponseWriter, r *http.Request) {
	if h.Client == nil {
		writeJSON(w, http.StatusServiceUnavailable, scanResponse{Error: "Receipt scanning is not configured on this instance."})
		return
	}
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, scanResponse{Error: "Could not read the upload."})
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, scanResponse{Error: "Attach a receipt photo in the image field."})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, MaxImageSize+1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, scanResponse{Error: "Could not read the upload."})
		return
	}

	ctype := header.Header.Get("Content-Type")
	if ctype == "" || ctype == "application/octet-stream" {
		ctype = http.DetectContentType(data)
	}

	// Clean up the upload before storing it: bake in the camera's EXIF
	// rotation, crop away the background and straighten skew, so the stored
	// file and the vision model both see an upright, receipt-only image.
	// Best-effort: unsuitable images pass through untouched.
	data = prepareImage(data)

	name, err := h.Store.Save(ctype, bytes.NewReader(data))
	if err != nil {
		switch {
		case errors.Is(err, ErrTooLarge):
			writeJSON(w, http.StatusRequestEntityTooLarge, scanResponse{Error: "Images must be 10 MB or smaller."})
		case errors.Is(err, ErrBadType):
			writeJSON(w, http.StatusUnsupportedMediaType, scanResponse{Error: "Only JPEG, PNG and WebP images are supported."})
		default:
			slog.Error("save receipt", "error", err)
			writeJSON(w, http.StatusInternalServerError, scanResponse{Error: "Could not store the image."})
		}
		return
	}

	// Attach the fresh scan to the sender's draft when one is given, so the
	// image is authorised to the draft's owner before the expense exists and
	// the preview still resolves after a reload. Best-effort: the form's
	// receipt_file field re-attaches it on the next autosave regardless.
	if u := auth.UserFrom(r); u != nil {
		if draftID := strings.TrimSpace(r.FormValue("draft")); draftID != "" {
			if err := h.Expenses.AttachReceipt(draftID, u.ID, name); err != nil {
				slog.Error("attach receipt to draft", "error", err)
			}
		}
	}

	writeJSON(w, http.StatusOK, scanResponse{OK: true, File: name})
}

// Analyze sends a stored receipt to the vision LLM and answers with the
// extracted itemization, ready for the itemization editor.
func (h *Handlers) Analyze(w http.ResponseWriter, r *http.Request) {
	if auth.UserFrom(r) == nil {
		http.NotFound(w, r)
		return
	}
	if h.Client == nil {
		writeJSON(w, http.StatusServiceUnavailable, scanResponse{Error: "Receipt scanning is not configured on this instance."})
		return
	}
	f, ctype, err := h.Store.Open(r.PathValue("file"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		slog.Error("read receipt", "error", err)
		writeJSON(w, http.StatusInternalServerError, scanResponse{Error: "Could not read the stored image."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	result, err := h.Client.Scan(ctx, ctype, data)
	if err != nil {
		slog.Error("receipt scan", "error", err)
		writeJSON(w, http.StatusBadGateway, scanResponse{Error: "The receipt service could not read this image."})
		return
	}

	writeJSON(w, http.StatusOK, scanResponse{
		OK:       true,
		Merchant: result.Merchant,
		Date:     result.Date,
		Total:    result.Total,
		Tax:      result.Tax,
		Tip:      result.Tip,
		Items:    result.Items,
	})
}

// Serve handles GET /receipts/{file} for embedded receipt images, in
// decreasing specificity: receipts attached to an expense are visible to
// that expense's group members only; receipts attached to a draft are
// visible to the draft's owner only (a scan survives reloading the draft
// without becoming world-readable); anything else — a scan on the edit
// page, not yet saved anywhere — is served to any signed-in user, the name
// being an unguessable random UUID.
func (h *Handlers) Serve(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r)
	if u == nil {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("file")
	groupID, found, err := h.Expenses.GroupIDByReceiptFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case found:
		ok, err := h.Expenses.IsMember(groupID, u.ID)
		if err != nil || !ok {
			http.NotFound(w, r)
			return
		}
	default:
		ownerID, owned, err := h.Expenses.DraftOwnerByReceiptFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if owned && ownerID != u.ID {
			http.NotFound(w, r)
			return
		}
		if !owned && !h.Store.FileExists(name) {
			http.NotFound(w, r)
			return
		}
	}
	f, ctype, err := h.Store.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "could not read receipt", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
