package receipt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseReceiptMarkdown(t *testing.T) {
	md := "# CORNER SHOP\n123 High Street\nSat 2026-08-30 14:03\n" +
		"MILK ............. 2.00\nBREAD ............. 3.00\n" +
		"SUBTOTAL 5.00\nTAX 0.45\nTOTAL 5.45\n" +
		"VISA CARD ****1234 5.45\nTHANK YOU"
	got, err := parseReceiptMarkdown(md)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Merchant != "CORNER SHOP" {
		t.Errorf("merchant = %q", got.Merchant)
	}
	if got.Date != "2026-08-30" {
		t.Errorf("date = %q", got.Date)
	}
	if got.Total != 5.45 {
		t.Errorf("total = %v, want 5.45", got.Total)
	}
	if got.Tax != 0.45 {
		t.Errorf("tax = %v, want 0.45", got.Tax)
	}
	if len(got.Items) != 2 || got.Items[0].Description != "MILK" || got.Items[0].Amount != 2.0 ||
		got.Items[1].Description != "BREAD" || got.Items[1].Amount != 3.0 {
		t.Errorf("items = %+v", got.Items)
	}
}

func TestParseReceiptMarkdownHTMLTable(t *testing.T) {
	md := "<h1>KIOSK CAFE</h1>\n<table>\n" +
		"<tr><td>FLAT WHITE</td><td>3.20</td></tr>\n" +
		"<tr><td>TEA</td><td>1.80</td></tr>\n" +
		"<tr><td>Total</td><td>£5.00</td></tr>\n" +
		"</table>\n30/08/2026"
	got, err := parseReceiptMarkdown(md)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Merchant != "KIOSK CAFE" || got.Total != 5.0 || got.Date != "2026-08-30" {
		t.Errorf("unexpected result: %+v", got)
	}
	if len(got.Items) != 2 || got.Items[1].Amount != 1.8 {
		t.Errorf("items = %+v", got.Items)
	}
}

func TestParseReceiptMarkdownFallbacks(t *testing.T) {
	// No total line: fall back to the item sum; ambiguous 08/30 style is
	// read as day-first when unmarked, but a forced month-first date is
	// recognised.
	got, err := parseReceiptMarkdown("08/30/2026\nGUM 1.20\nMINTS 0.80")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Total != 2.0 || got.Date != "2026-08-30" || len(got.Items) != 2 {
		t.Errorf("unexpected result: %+v", got)
	}

	if _, err := parseReceiptMarkdown("no prices in here at all"); err == nil {
		t.Error("expected error for text without prices")
	}
}

func TestParseMoney(t *testing.T) {
	cases := map[string]float64{
		"13.45":     13.45,
		"$1,234.56": 1234.56,
		"13,45":     13.45,
		"1.234,56":  1234.56,
		"3.00-":     -3,
		"-1.50":     -1.5,
		"2.00":      2,
		"€ 7,00":    7,
		"1,234":     1234,
		"junk":      0,
	}
	for in, want := range cases {
		if got := parseMoney(in); got != want {
			t.Errorf("parseMoney(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestClientScanLayoutParsing(t *testing.T) {
	var path, auth, model, file string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		var body layoutParsingRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		model, file = body.Model, body.File
		json.NewEncoder(w).Encode(map[string]any{
			"md_results": "BAKERY\nCROISSANT 2.50\nTOTAL 2.50\n",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "glm-key", "glm-ocr", "")
	got, err := c.Scan(context.Background(), "image/jpeg", []byte("fake jpeg bytes"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if path != "/layout_parsing" {
		t.Errorf("path = %q, want /layout_parsing", path)
	}
	if auth != "Bearer glm-key" {
		t.Errorf("authorization = %q", auth)
	}
	if model != "glm-ocr" || file != base64.StdEncoding.EncodeToString([]byte("fake jpeg bytes")) {
		t.Errorf("request body: model=%q file=%q", model, file)
	}
	if got.Total != 2.5 || len(got.Items) != 1 || got.Items[0].Description != "CROISSANT" {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestClientScanLayoutParsingError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": 1214, "message": "OCR only supports PDF, JPG, PNG, JPEG formats"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "glm-ocr", "")
	if _, err := c.Scan(context.Background(), "image/jpeg", []byte("x")); err == nil {
		t.Error("expected error from non-200 response")
	}
}
