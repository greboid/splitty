package receipt

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/greboid/splitpayments/internal/auth"
	"github.com/greboid/splitpayments/internal/database"
	"github.com/greboid/splitpayments/internal/user"
)

// fakeLLM asserts the OpenAI-compatible request shape and returns a canned
// extraction.
func fakeLLM(t *testing.T, reply string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL *struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Model != "vision-test" {
			t.Errorf("model = %q", body.Model)
		}
		if len(body.Messages) != 1 || len(body.Messages[0].Content) != 2 {
			t.Fatalf("unexpected messages shape: %+v", body.Messages)
		}
		img := body.Messages[0].Content[1]
		if img.ImageURL == nil || !bytes.HasPrefix([]byte(img.ImageURL.URL), []byte("data:image/png;base64,")) {
			t.Errorf("image part missing data URI: %+v", img)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": reply},
			}},
		})
	}))
	return srv, &calls
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.White)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestScanEndToEnd(t *testing.T) {
	reply := "Sure! Here is the data:\n```json\n" +
		`{"merchant":"Corner Shop","date":"2026-08-30","total":13.45,"tax":0.45,"tip":0,
		  "items":[{"description":"Milk","amount":2.0},{"description":"Bread","amount":3.0}]}` +
		"\n```"
	srv, calls := fakeLLM(t, reply)
	defer srv.Close()

	store := mustStore(t)
	h := &Handlers{
		Store:  store,
		Client: NewClient(srv.URL+"/v1", "test-key", "vision-test", ""),
	}

	rec, file := postScan(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp scanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Merchant != "Corner Shop" || len(resp.Items) != 2 {
		t.Errorf("unexpected response: %+v", resp)
	}
	if resp.Items[0].Description != "Milk" || resp.Items[0].Amount != 2.0 {
		t.Errorf("items[0] = %+v", resp.Items[0])
	}
	if *calls != 1 {
		t.Errorf("LLM called %d times", *calls)
	}
	// The stored image — the cleaned-up upload — must exist under the
	// returned name.
	if !store.FileExists(file) {
		t.Errorf("stored receipt %q missing", file)
	}
}

func TestScanRejectsBadType(t *testing.T) {
	h := &Handlers{Store: mustStore(t), Client: NewClient("http://unused", "k", "m", "")}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("image", "receipt.txt")
	fw.Write([]byte("not an image"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/receipts", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.Upload(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", rec.Code)
	}
}

func TestScanDisabled(t *testing.T) {
	h := &Handlers{Store: mustStore(t)}
	req := httptest.NewRequest(http.MethodPost, "/receipts", bytes.NewReader([]byte("x")))
	rec := httptest.NewRecorder()
	h.Upload(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec := analyzeFile(t, h, "whatever.jpg"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("analyze status = %d, want 503", rec.Code)
	}
}

func TestAnalyzeGuards(t *testing.T) {
	h := &Handlers{Store: mustStore(t), Client: NewClient("http://unused", "k", "m", "")}

	// Analyze needs a signed-in user.
	req := httptest.NewRequest(http.MethodPost, "/receipts/x.jpg/analyze", nil)
	rec := httptest.NewRecorder()
	h.Analyze(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unauthenticated analyze = %d, want 404", rec.Code)
	}

	// And a name that is not a stored receipt is not served.
	if rec := analyzeFile(t, h, "not-a-receipt.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown file analyze = %d, want 404", rec.Code)
	}
}

// mustStore returns a Store backed by a fresh, migrated in-memory
// database. One connection keeps that private database alive.
func mustStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return NewStore(db)
}

func TestScanAnthropic(t *testing.T) {
	reply := "Here is the data:\n```json\n" +
		`{"merchant":"Cafe","date":"2026-08-31","total":9.2,"tax":1.2,"tip":0,
		  "items":[{"description":"Coffee","amount":3.4}]}` +
		"\n```"
	srv := fakeAnthropic(t, reply)
	defer srv.Close()

	for name, baseURL := range map[string]string{
		"plain base":     srv.URL,
		"base with /v1":  srv.URL + "/v1", // must not produce /v1/v1/messages
		"trailing slash": srv.URL + "/",
	} {
		t.Run(name, func(t *testing.T) {
			h := &Handlers{Store: mustStore(t), Client: NewClient(baseURL, "test-key", "claude-vision", "anthropic")}
			rec, _ := postScan(t, h)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var resp scanResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.OK || resp.Merchant != "Cafe" || len(resp.Items) != 1 || resp.Items[0].Description != "Coffee" {
				t.Errorf("unexpected response: %+v", resp)
			}
		})
	}
}

// fakeAnthropic asserts the Anthropic Messages API request shape and returns
// a canned text reply.
func fakeAnthropic(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version header")
		}
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Role    string `json:"role"`
				Content []struct {
					Type   string `json:"type"`
					Text   string `json:"text"`
					Source *struct {
						Type      string `json:"type"`
						MediaType string `json:"media_type"`
						Data      string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Model != "claude-vision" || body.MaxTokens == 0 {
			t.Errorf("model = %q, max_tokens = %d", body.Model, body.MaxTokens)
		}
		if len(body.Messages) != 1 || len(body.Messages[0].Content) != 2 {
			t.Fatalf("unexpected messages shape: %+v", body.Messages)
		}
		img := body.Messages[0].Content[0]
		if img.Type != "image" || img.Source == nil || img.Source.Type != "base64" ||
			img.Source.MediaType != "image/png" || img.Source.Data == "" {
			t.Errorf("bad image block: %+v", img)
		}
		if txt := body.Messages[0].Content[1]; txt.Type != "text" || txt.Text == "" {
			t.Errorf("bad text block: %+v", txt)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"content": []any{map[string]any{"type": "text", "text": reply}},
		})
	}))
	return srv
}

// postScan runs the upload+analyze flow against the handlers, as the client
// UI does, returning the analyze response and the stored file name.
func postScan(t *testing.T, h *Handlers) (*httptest.ResponseRecorder, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("image", "receipt.png")
	fw.Write(testPNG(t))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/receipts", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.Upload(rec, req)
	if rec.Code != http.StatusOK {
		return rec, ""
	}
	var stored scanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.File == "" {
		t.Fatal("upload response is missing the file name")
	}
	return analyzeFile(t, h, stored.File), stored.File
}

// analyzeFile calls the analyze endpoint for a stored receipt as an
// authenticated user.
func analyzeFile(t *testing.T, h *Handlers, file string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/receipts/"+file+"/analyze", nil)
	req = req.WithContext(auth.WithUser(req.Context(), &user.User{ID: 1, Name: "Tester"}))
	req.SetPathValue("file", file) // no mux is involved in handler tests
	rec := httptest.NewRecorder()
	h.Analyze(rec, req)
	return rec
}

// fakeGWTYPE1 asserts the gwtype1 AI Gateway /chat request shape and returns
// a canned answer with the given text and pre-parsed json fields.
func fakeGWTYPE1(t *testing.T, text string, jsonField any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// Unknown keys are rejected with a 400 on this API, so the request
		// must carry exactly the documented set.
		allowed := map[string]bool{
			"model": true, "system": true, "messages": true, "schema": true,
			"max_tokens": true, "temperature": true, "caller": true,
		}
		for k := range body {
			if !allowed[k] {
				t.Errorf("unexpected request key %q", k)
			}
		}
		if got, _ := body["model"]; string(got) != `"claude-haiku-4-5"` {
			t.Errorf("model = %s", got)
		}
		var msgs []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"content"`
		}
		if err := json.Unmarshal(body["messages"], &msgs); err != nil {
			t.Errorf("decode messages: %v", err)
		}
		if len(msgs) != 1 || msgs[0].Role != "user" || len(msgs[0].Content) != 2 {
			t.Fatalf("unexpected messages: %+v", msgs)
		}
		file, txt := msgs[0].Content[0], msgs[0].Content[1]
		if file.Type != "file" || file.MediaType != "image/png" || file.Data == "" {
			t.Errorf("bad file part: %+v", file)
		}
		if txt.Type != "text" || txt.Text == "" {
			t.Errorf("bad text part: %+v", txt)
		}
		if schema, ok := body["schema"]; !ok || len(schema) == 0 {
			t.Error("missing schema")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"text": text, "json": jsonField, "stop_reason": "end",
		})
	}))
	return srv
}

func TestScanGWTYPE1(t *testing.T) {
	text := "Here you go:\n```json\n" +
		`{"merchant":"Cafe","date":"2026-08-31","total":9.2,"tax":1.2,"tip":0,
		  "items":[{"description":"Coffee","amount":3.4}]}` +
		"\n```"

	t.Run("parsed json field", func(t *testing.T) {
		srv := fakeGWTYPE1(t, "ignored", map[string]any{
			"merchant": "Cafe", "date": "2026-08-31", "total": 9.2, "tax": 1.2, "tip": 0,
			"items": []any{map[string]any{"description": "Coffee", "amount": 3.4}},
		})
		defer srv.Close()
		h := &Handlers{Store: mustStore(t), Client: NewClient(srv.URL+"/api/v1", "test-key", "claude-haiku-4-5", "gwtype1")}
		rec, _ := postScan(t, h)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp scanResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.OK || resp.Merchant != "Cafe" || resp.Total != 9.2 || len(resp.Items) != 1 {
			t.Errorf("unexpected response: %+v", resp)
		}
	})

	t.Run("text fallback", func(t *testing.T) {
		srv := fakeGWTYPE1(t, text, nil)
		defer srv.Close()
		h := &Handlers{Store: mustStore(t), Client: NewClient(srv.URL+"/api/v1", "test-key", "claude-haiku-4-5", "gwtype1")}
		rec, _ := postScan(t, h)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp scanResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.OK || resp.Merchant != "Cafe" || len(resp.Items) != 1 || resp.Items[0].Description != "Coffee" {
			t.Errorf("unexpected response: %+v", resp)
		}
	})
}

func TestScanGWTYPE1QuotaError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"type": "quota", "message": "allowance spent"})
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/api/v1", "k", "claude-haiku-4-5", "gwtype1")
	_, err := c.Scan(context.Background(), "image/png", testPNG(t))
	if err == nil || !strings.Contains(err.Error(), "quota: allowance spent") {
		t.Errorf("err = %v, want quota message", err)
	}
}

// fakeGWTYPE2 asserts the gwtype2 multipart /api/execute request shape and
// returns the given canned response body.
func fakeGWTYPE2(t *testing.T, response []byte) *httptest.Server {
	t.Helper()
	wantPNG := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/execute" {
			t.Errorf("path = %q", r.URL.Path)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Fatalf("multipart: %v", err)
		}
		form := map[string]string{}
		var fileHeader textproto.MIMEHeader
		var fileBody []byte
		hasFile := false
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("next part: %v", err)
			}
			body, err := io.ReadAll(p)
			if err != nil {
				t.Fatalf("read part: %v", err)
			}
			switch p.FormName() {
			case "workflow":
				form["workflow"] = string(body)
			case "client":
				form["client"] = string(body)
			case "args":
				form["args"] = string(body)
			case "files":
				hasFile = true
				fileHeader = p.Header
				fileBody = body
			default:
				t.Errorf("unexpected form part %q", p.FormName())
			}
		}
		if form["workflow"] != "extract-receipt" {
			t.Errorf("workflow = %q", form["workflow"])
		}
		if form["client"] != "splitpayments" {
			t.Errorf("client = %q", form["client"])
		}
		if !json.Valid([]byte(form["args"])) {
			t.Errorf("args = %q, not JSON", form["args"])
		}
		if !hasFile {
			t.Fatal("missing files part")
		}
		if got := fileHeader.Get("Content-Type"); got != "image/png" {
			t.Errorf("file content type = %q", got)
		}
		if !bytes.Equal(fileBody, wantPNG) {
			t.Error("uploaded bytes differ from the receipt image")
		}
		w.Write(response)
	}))
	return srv
}

func TestScanGWTYPE2(t *testing.T) {
	receiptJSON := `{"merchant":"Corner Shop","date":"2026-08-30","total":13.45,"tax":0.45,"tip":0,
	  "items":[{"description":"Milk","amount":2.0},{"description":"Bread","amount":3.0}]}`
	fenced, err := json.Marshal(map[string]string{
		"text": "Sure!\n```json\n" + receiptJSON + "\n```",
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body []byte
	}{
		{"bare receipt object", []byte(receiptJSON)},
		{"wrapped in result key", []byte(`{"result":` + receiptJSON + `}`)},
		{"fenced json in text key", fenced},
		{"plain prose with json", []byte("The receipt reads:\n" + receiptJSON)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeGWTYPE2(t, tc.body)
			defer srv.Close()
			c := NewClient(srv.URL, "unused-key", "", "gwtype2")
			c.Workflow = "extract-receipt"
			c.Args = `{"currency":"GBP"}`
			h := &Handlers{Store: mustStore(t), Client: c}
			rec, _ := postScan(t, h)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var resp scanResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.OK || resp.Merchant != "Corner Shop" || resp.Total != 13.45 || len(resp.Items) != 2 {
				t.Errorf("unexpected response: %+v", resp)
			}
		})
	}
}

func TestScanGWTYPE2NeedsWorkflow(t *testing.T) {
	c := NewClient("http://unused", "", "", "gwtype2")
	_, err := c.Scan(context.Background(), "image/png", testPNG(t))
	if err == nil || !strings.Contains(err.Error(), "workflow") {
		t.Errorf("err = %v, want a missing-workflow error", err)
	}
}

// fakeGWTYPE3 asserts the gwtype3 workflow request shape and returns a
// canned run answer wrapping the given output object.
func fakeGWTYPE3(t *testing.T, output any) *httptest.Server {
	t.Helper()
	wantPNG := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workflows/expenses-receipt-reader" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// Unknown keys are rejected with a 400 on this API, so the request
		// must carry exactly the documented set.
		allowed := map[string]bool{"args": true, "files": true, "caller": true, "tags": true}
		for k := range body {
			if !allowed[k] {
				t.Errorf("unexpected request key %q", k)
			}
		}
		var args map[string]any
		if err := json.Unmarshal(body["args"], &args); err != nil || len(args) != 0 {
			t.Errorf("args = %s, want an empty JSON object", body["args"])
		}
		if got := string(body["caller"]); got != `"splitpayments"` {
			t.Errorf("caller = %s", got)
		}
		var files []struct {
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			Name      string `json:"name"`
		}
		if err := json.Unmarshal(body["files"], &files); err != nil {
			t.Fatalf("decode files: %v", err)
		}
		if len(files) != 1 {
			t.Fatalf("files = %d entries, want 1", len(files))
		}
		if files[0].MediaType != "image/png" || files[0].Name != "receipt.png" {
			t.Errorf("file part = %+v", files[0])
		}
		if got, err := base64.StdEncoding.DecodeString(files[0].Data); err != nil || !bytes.Equal(got, wantPNG) {
			t.Error("uploaded bytes differ from the receipt image")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id":          "run-1",
			"workflow":    "expenses-receipt-reader",
			"model":       map[string]any{"slug": "claude-haiku-4-5", "provider": "anthropic"},
			"output":      output,
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
			"duration_ms": 12,
		})
	}))
	return srv
}

func TestScanGWTYPE3(t *testing.T) {
	output := map[string]any{
		"date":        "2026-08-30",
		"description": "Team lunch",
		"number":      "INV-42",
		"details":     "non-GBP currency",
		"lineItems": []any{
			map[string]any{"lineDescription": "Milk", "category": "other", "gross": 2.0, "vat": 0.2},
			map[string]any{"lineDescription": "Bread", "category": "hotelAndMeal", "gross": 3.0, "vat": 0.3},
		},
		"statedTotal": 13.45,
		"statedVat":   0.45,
	}

	t.Run("plain base", func(t *testing.T) {
		srv := fakeGWTYPE3(t, output)
		defer srv.Close()
		h := &Handlers{Store: mustStore(t), Client: newGWTYPE3Client(srv.URL, "test-key")}
		assertGWTYPE3Scan(t, h)
	})

	t.Run("base with /api/v1", func(t *testing.T) {
		// Must not produce /api/v1/api/v1/workflows/…
		srv := fakeGWTYPE3(t, output)
		defer srv.Close()
		h := &Handlers{Store: mustStore(t), Client: newGWTYPE3Client(srv.URL+"/api/v1", "test-key")}
		assertGWTYPE3Scan(t, h)
	})
}

func newGWTYPE3Client(baseURL, key string) *Client {
	c := NewClient(baseURL, key, "", "gwtype3")
	c.Workflow = "expenses-receipt-reader"
	return c
}

func assertGWTYPE3Scan(t *testing.T, h *Handlers) {
	t.Helper()
	rec, _ := postScan(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp scanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// The workflow answers no merchant, so that field stays empty.
	if !resp.OK || resp.Merchant != "" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if resp.Date != "2026-08-30" || resp.Total != 13.45 || resp.Tax != 0.45 {
		t.Errorf("date/total/tax = %q/%v/%v", resp.Date, resp.Total, resp.Tax)
	}
	if len(resp.Items) != 2 || resp.Items[0].Description != "Milk" || resp.Items[0].Amount != 2.0 {
		t.Errorf("items = %+v", resp.Items)
	}
}

func TestScanGWTYPE3NeedsWorkflow(t *testing.T) {
	c := NewClient("http://unused", "", "", "gwtype3")
	_, err := c.Scan(context.Background(), "image/png", testPNG(t))
	if err == nil || !strings.Contains(err.Error(), "workflow") {
		t.Errorf("err = %v, want a missing-workflow error", err)
	}
}

func TestResolveFormat(t *testing.T) {
	cases := []struct{ url, flag, want string }{
		{"https://api.anthropic.com", "", "anthropic"},
		{"https://api.anthropic.com/v1", "", "anthropic"},
		{"https://API.Anthropic.com", "", "anthropic"},
		{"https://api.openai.com/v1", "", "openai"},
		{"http://localhost:11434/v1", "", "openai"},
		{"https://gateway.example.com/anthropic", "anthropic", "anthropic"},
		{"https://gateway.example.com/api/v1", "gwtype1", "gwtype1"},
		{"https://gm.yak-wall.ts.net", "gwtype2", "gwtype2"},
		{"https://gm.yak-wall.ts.net", "GWTYPE2", "gwtype2"},
		{"https://aigateway.example.com", "gwtype3", "gwtype3"},
		{"https://aigateway.example.com/api/v1", "gwtype3", "gwtype3"},
		{"https://api.anthropic.com", "openai", "openai"},
		{"https://api.openai.com/v1", "bogus", "openai"},
	}
	for _, tc := range cases {
		if got := resolveFormat(tc.url, tc.flag); got != tc.want {
			t.Errorf("resolveFormat(%q, %q) = %q, want %q", tc.url, tc.flag, got, tc.want)
		}
	}
}

func TestParseResultTolerance(t *testing.T) {
	good, err := ParseResult(`Here you go: {"merchant":"M","date":"2026-01-02","total":5,"tax":0,"tip":0,"items":[]}`)
	if err != nil || good.Merchant != "M" {
		t.Errorf("parse fenced JSON failed: %v %+v", err, good)
	}
	if _, err := ParseResult("no json here"); err == nil {
		t.Error("expected error for reply without JSON")
	}
	if _, err := ParseResult(`{"merchant":123}`); err == nil {
		t.Error("expected error for wrong-typed JSON")
	}
}

func TestClientScanContext(t *testing.T) {
	// A slow server must not block the caller: the client has a timeout.
	// (The handler must eventually return on its own; Server.Close waits
	// for in-flight requests, so a forever-blocking handler would deadlock.)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Second)
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/v1", "k", "m", "")
	c.HTTP = &http.Client{Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := c.Scan(context.Background(), "image/png", testPNG(t))
	if err == nil {
		t.Error("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Errorf("Scan took %v; the client timeout did not fire", elapsed)
	}
}
