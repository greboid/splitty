package receipt

// Minimal client for vision endpoints that accept receipt images: an
// OpenAI-compatible chat/completions API, the Anthropic Messages API, or
// the gwtype1/gwtype2/gwtype3 AI Gateway APIs.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// Item is one scanned line item.
type Item struct {
	Description string  `json:"description"`
	Amount      float64 `json:"amount"`
}

// ScanResult is the structured extraction from one receipt.
type ScanResult struct {
	Merchant string  `json:"merchant"`
	Date     string  `json:"date"`
	Total    float64 `json:"total"`
	Tax      float64 `json:"tax"`
	Tip      float64 `json:"tip"`
	Items    []Item  `json:"items"`
}

const scanPrompt = `You read receipts. Extract structured data from this receipt image.
Reply with ONLY a JSON object, no markdown fences, matching exactly:
{"merchant": string, "date": "YYYY-MM-DD", "total": number, "tax": number, "tip": number,
 "items": [{"description": string, "amount": number}]}
Amounts are numbers in the receipt's currency (major units, e.g. 12.34).
Every line item on the receipt must appear in items with its price; omit items only if unreadable.
If tax or tip is not shown, use 0. The date is the receipt date.`

// Wire protocol spoken by the receipt API. OpenAI-compatible endpoints
// (including local ones such as Ollama) use "openai"; api.anthropic.com and
// Anthropic-compatible gateways use "anthropic".
const (
	FormatOpenAI    = "openai"
	FormatAnthropic = "anthropic"
	FormatGWTYPE1   = "gwtype1"
	FormatGWTYPE2   = "gwtype2"
	FormatGWTYPE3   = "gwtype3"

	anthropicVersion = "2023-06-01"

	// gwtype2 form fields: the gateway runs named workflows on uploaded files.
	gwtype2Client = "splitpayments"

	// gwtype3 caller field: free text kept on the gateway's log row.
	gwtype3Caller = "splitpayments"
)

// Client talks to a vision endpoint for receipt scanning.
type Client struct {
	BaseURL  string
	APIKey   string
	Model    string
	Format   string // resolved protocol: FormatOpenAI, FormatAnthropic or one of the FormatGWTYPE* gateways
	Workflow string // gwtype2/gwtype3: workflow executed on the gateway
	Args     string // gwtype2/gwtype3: JSON object passed as the workflow args
	HTTP     *http.Client
}

func NewClient(baseURL, apiKey, model, format string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Format:  resolveFormat(baseURL, format),
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// resolveFormat picks the wire protocol: an explicit format wins, otherwise
// the official Anthropic host is detected and everything else is treated as
// OpenAI-compatible.
func resolveFormat(baseURL, format string) string {
	switch f := strings.ToLower(strings.TrimSpace(format)); f {
	case FormatOpenAI, FormatAnthropic, FormatGWTYPE1, FormatGWTYPE2, FormatGWTYPE3:
		return f
	case "":
		if u, err := url.Parse(baseURL); err == nil && strings.EqualFold(u.Hostname(), "api.anthropic.com") {
			return FormatAnthropic
		}
		return FormatOpenAI
	default:
		slog.Warn("unknown receipt API format, using openai", "format", format)
		return FormatOpenAI
	}
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
}

// apiError is the error envelope shared by the chat-completions and
// layout-parsing endpoints.
type apiError struct {
	Code    json.RawMessage `json:"code,omitempty"`
	Message string          `json:"message"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *apiError `json:"error"`
}

type layoutParsingRequest struct {
	Model string `json:"model"`
	File  string `json:"file"` // URL or base64-encoded document
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"` // required by the Messages API
	Temperature float64            `json:"temperature"`
	Messages    []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type   string          `json:"type"` // "text" or "image"
	Text   string          `json:"text,omitempty"`
	Source *anthropicImage `json:"source,omitempty"`
}

type anthropicImage struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *apiError `json:"error"`
}

// receiptSchema is the JSON Schema sent to gwtype1-style gateways so the
// answer comes back pre-parsed in the response's json field.
var receiptSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"merchant": map[string]any{"type": "string"},
		"date":     map[string]any{"type": "string"},
		"total":    map[string]any{"type": "number"},
		"tax":      map[string]any{"type": "number"},
		"tip":      map[string]any{"type": "number"},
		"items": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"description": map[string]any{"type": "string"},
					"amount":      map[string]any{"type": "number"},
				},
				"required":             []string{"description", "amount"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"merchant", "date", "total", "items"},
	"additionalProperties": false,
}

// gwtype1 gateway payloads. Unknown request keys are rejected with a 400,
// so only documented fields are marshalled (omitempty on the optionals).
type gwtype1Request struct {
	Model       string           `json:"model"`
	System      string           `json:"system,omitempty"`
	Messages    []gwtype1Message `json:"messages"`
	Schema      any              `json:"schema,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature float64          `json:"temperature"`
	Caller      string           `json:"caller,omitempty"`
}

type gwtype1Message struct {
	Role    string        `json:"role"`
	Content []gwtype1Part `json:"content"`
}

type gwtype1Part struct {
	Type      string `json:"type"` // "text" or "file"
	Text      string `json:"text,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

type gwtype1Response struct {
	Text       string          `json:"text"`
	JSON       json.RawMessage `json:"json"` // parsed answer when a schema was sent, else null
	StopReason string          `json:"stop_reason"`
}

type layoutParsingResponse struct {
	MDResults     string `json:"md_results"`
	LayoutDetails []struct {
		Label   string `json:"label"`
		Content string `json:"content"`
	} `json:"layout_details"`
	Error *apiError `json:"error"`
}

// Scan sends the image bytes to the configured model and parses the reply.
func (c *Client) Scan(ctx context.Context, contentType string, data []byte) (ScanResult, error) {
	var result ScanResult
	// The workflow formats run a named workflow instead of a chat model, so
	// they are gated on the workflow name rather than the model.
	if c.Format == FormatGWTYPE2 || c.Format == FormatGWTYPE3 {
		if c.Workflow == "" {
			return result, fmt.Errorf("receipt scanning with the %s format needs a workflow name", c.Format)
		}
		if c.Format == FormatGWTYPE2 {
			return c.scanGWTYPE2(ctx, contentType, data)
		}
		return c.scanGWTYPE3(ctx, contentType, data)
	}
	if c.Model == "" {
		return result, errors.New("receipt scanning is not configured")
	}
	if c.Format == FormatAnthropic {
		return c.scanAnthropic(ctx, contentType, data)
	}
	if c.Format == FormatGWTYPE1 {
		return c.scanGWTYPE1(ctx, contentType, data)
	}
	// OCR models are not served through /chat/completions at all; posting
	// them there fails with error 1214 ("OCR only supports ..."). They only
	// answer on the layout-parsing endpoint.
	if strings.HasPrefix(strings.ToLower(c.Model), "glm-ocr") {
		return c.scanLayoutParsing(ctx, data)
	}
	return c.scanChat(ctx, contentType, data)
}

func (c *Client) scanChat(ctx context.Context, contentType string, data []byte) (ScanResult, error) {
	var result ScanResult
	dataURI := fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(data))

	req := chatRequest{
		Model: c.Model,
		Messages: []chatMessage{{
			Role: "user",
			Content: []contentPart{
				{Type: "text", Text: scanPrompt},
				{Type: "image_url", ImageURL: &imageURL{URL: dataURI}},
			},
		}},
		MaxTokens:   2000,
		Temperature: 0,
	}

	respBody, err := c.postJSON(ctx, c.BaseURL+"/chat/completions", req)
	if err != nil {
		return result, err
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return result, fmt.Errorf("receipt API response was not JSON: %w", err)
	}
	if parsed.Error != nil {
		return result, fmt.Errorf("receipt API error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return result, errors.New("receipt API returned no choices")
	}
	return ParseResult(parsed.Choices[0].Message.Content)
}

// scanAnthropic calls the Anthropic Messages API. The image block precedes
// the prompt, as the API documentation recommends. A base URL that already
// ends in /v1 (the OpenAI-style convention) is tolerated.
func (c *Client) scanAnthropic(ctx context.Context, contentType string, data []byte) (ScanResult, error) {
	var result ScanResult
	endpoint := strings.TrimSuffix(c.BaseURL, "/v1") + "/v1/messages"
	req := anthropicRequest{
		Model:       c.Model,
		MaxTokens:   2000,
		Temperature: 0,
		Messages: []anthropicMessage{{
			Role: "user",
			Content: []anthropicBlock{
				{
					Type: "image",
					Source: &anthropicImage{
						Type:      "base64",
						MediaType: contentType,
						Data:      base64.StdEncoding.EncodeToString(data),
					},
				},
				{Type: "text", Text: scanPrompt},
			},
		}},
	}

	respBody, err := c.postJSON(ctx, endpoint, req)
	if err != nil {
		return result, err
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return result, fmt.Errorf("receipt API response was not JSON: %w", err)
	}
	if parsed.Error != nil {
		return result, fmt.Errorf("receipt API error: %s", parsed.Error.Message)
	}

	var reply strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			reply.WriteString(block.Text)
		}
	}
	if strings.TrimSpace(reply.String()) == "" {
		return result, errors.New("receipt API returned no text")
	}
	return ParseResult(reply.String())
}

// scanGWTYPE1 calls the gwtype1 AI Gateway: POST {base}/chat with Bearer
// auth, the receipt image as a base64 "file" part on the user turn, and the
// receipt schema so the gateway returns a pre-parsed answer. The gateway's
// json field is preferred; the raw text reply is the fallback.
func (c *Client) scanGWTYPE1(ctx context.Context, contentType string, data []byte) (ScanResult, error) {
	var result ScanResult
	req := gwtype1Request{
		Model:  c.Model,
		System: scanPrompt,
		Messages: []gwtype1Message{{
			Role: "user",
			Content: []gwtype1Part{
				{Type: "file", MediaType: contentType, Data: base64.StdEncoding.EncodeToString(data)},
				{Type: "text", Text: "Extract the data from this receipt."},
			},
		}},
		Schema:      receiptSchema,
		MaxTokens:   2000,
		Temperature: 0,
		Caller:      "splitpayments",
	}

	respBody, err := c.postJSON(ctx, c.BaseURL+"/chat", req)
	if err != nil {
		return result, err
	}

	var parsed gwtype1Response
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return result, fmt.Errorf("receipt API response was not JSON: %w", err)
	}
	if parsed.StopReason == "refusal" || parsed.StopReason == "filtered" {
		return result, fmt.Errorf("receipt API did not read this image (%s)", parsed.StopReason)
	}
	if len(parsed.JSON) > 0 && string(parsed.JSON) != "null" {
		if err := json.Unmarshal(parsed.JSON, &result); err != nil {
			return result, fmt.Errorf("receipt API json was not valid receipt JSON: %w", err)
		}
		return result, nil
	}
	if strings.TrimSpace(parsed.Text) == "" {
		return result, errors.New("receipt API returned no text")
	}
	return ParseResult(parsed.Text)
}

// scanGWTYPE2 executes the configured workflow on an gwtype2 gateway: a multipart
// form POST to {base}/api/execute with the receipt as a "files" part. The
// answer is expected to be receipt JSON — bare, wrapped in a common
// envelope key, or embedded in a text reply.
func (c *Client) scanGWTYPE2(ctx context.Context, contentType string, data []byte) (ScanResult, error) {
	var result ScanResult
	args := c.Args
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return result, fmt.Errorf("receipt API args are not valid JSON: %s", truncate(args, 100))
	}

	form := &bytes.Buffer{}
	mw := multipart.NewWriter(form)
	for _, f := range []struct{ name, value string }{
		{"workflow", c.Workflow},
		{"client", gwtype2Client},
		{"args", args},
	} {
		if err := mw.WriteField(f.name, f.value); err != nil {
			return result, err
		}
	}
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename=%q`, receiptFileName(contentType)))
	partHeader.Set("Content-Type", contentType)
	part, err := mw.CreatePart(partHeader)
	if err != nil {
		return result, err
	}
	if _, err := part.Write(data); err != nil {
		return result, err
	}
	if err := mw.Close(); err != nil {
		return result, err
	}

	respBody, err := c.post(ctx, c.BaseURL+"/api/execute", mw.FormDataContentType(), form)
	if err != nil {
		return result, err
	}
	return parseGWTYPE2Body(respBody, 3)
}

// gwtype3 gateway payloads: a named workflow receives a JSON body with the
// receipt as a base64 entry in the required files list. Unknown request keys
// are rejected with a 400, so only documented fields are marshalled
// (omitempty on the optional ones).
type gwtype3Request struct {
	Args   json.RawMessage `json:"args"`
	Files  []gwtype3File   `json:"files"`
	Caller string          `json:"caller,omitempty"`
}

type gwtype3File struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	Name      string `json:"name,omitempty"`
}

type gwtype3Response struct {
	ID     string          `json:"id"`
	Output json.RawMessage `json:"output"`
}

// gwtype3Output is the expenses-receipt-reader workflow's answer schema: an
// expenses-claim extraction with its own field names, mapped onto ScanResult
// afterwards.
type gwtype3Output struct {
	Date        string `json:"date"`
	Description string `json:"description"`
	Number      string `json:"number"`
	Details     string `json:"details"`
	LineItems   []struct {
		LineDescription string  `json:"lineDescription"`
		Category        string  `json:"category"`
		Gross           float64 `json:"gross"`
		VAT             float64 `json:"vat"`
	} `json:"lineItems"`
	StatedTotal float64 `json:"statedTotal"`
	StatedVat   float64 `json:"statedVat"`
}

// scanGWTYPE3 runs the configured workflow on a gwtype3 gateway: a JSON POST
// to {base}/api/v1/workflows/{workflow} with the receipt as the single
// base64 file (the gateway refuses calls without one). The workflow's
// output is an expense-claim object rather than a receipt: the stated total
// and VAT become the receipt total and tax, and each line item's gross
// amount becomes an item. The workflow answers no merchant name, so that
// field stays empty; its free-text description and exceptions are logged.
func (c *Client) scanGWTYPE3(ctx context.Context, contentType string, data []byte) (ScanResult, error) {
	var result ScanResult
	args := c.Args
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return result, fmt.Errorf("receipt API args are not valid JSON: %s", truncate(args, 100))
	}

	req := gwtype3Request{
		Args: json.RawMessage(args),
		Files: []gwtype3File{{
			MediaType: contentType,
			Data:      base64.StdEncoding.EncodeToString(data),
			Name:      receiptFileName(contentType),
		}},
		Caller: gwtype3Caller,
	}

	// A base URL that already ends in /api/v1 (the gwtype1 convention) is
	// tolerated.
	endpoint := strings.TrimSuffix(c.BaseURL, "/api/v1") + "/api/v1/workflows/" + c.Workflow
	respBody, err := c.postJSON(ctx, endpoint, req)
	if err != nil {
		return result, err
	}

	var parsed gwtype3Response
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return result, fmt.Errorf("receipt API response was not JSON: %w", err)
	}
	if len(parsed.Output) == 0 || string(parsed.Output) == "null" {
		return result, errors.New("receipt workflow returned no output")
	}
	var out gwtype3Output
	if err := json.Unmarshal(parsed.Output, &out); err != nil {
		return result, fmt.Errorf("receipt workflow output was not valid receipt JSON: %w", err)
	}

	if out.Details != "" || out.Description != "" {
		slog.Info("receipt workflow notes", "workflow", c.Workflow, "description", out.Description, "details", out.Details)
	}
	result.Date = out.Date
	result.Total = out.StatedTotal
	result.Tax = out.StatedVat
	for _, li := range out.LineItems {
		result.Items = append(result.Items, Item{Description: li.LineDescription, Amount: li.Gross})
	}
	return result, nil
}

// receiptFileName gives an uploaded receipt a plausible file name from its
// content type.
func receiptFileName(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return "receipt.jpg"
	case "image/png":
		return "receipt.png"
	case "image/webp":
		return "receipt.webp"
	}
	return "receipt"
}

// gwtype2EnvelopeKeys are response keys the receipt answer might be wrapped in.
var gwtype2EnvelopeKeys = []string{"json", "result", "output", "data", "response", "receipt", "text"}

// parseGWTYPE2Body recovers the receipt from a workflow response whose shape is
// not pinned down: the body may be the receipt object itself, an envelope
// around it (or around its JSON text), or prose with embedded JSON. depth
// bounds envelope unwrapping.
func parseGWTYPE2Body(body []byte, depth int) (ScanResult, error) {
	var result ScanResult
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		if err := json.Unmarshal(trimmed, &result); err == nil && looksLikeReceipt(result) {
			return result, nil
		}
	}
	if depth > 0 {
		var obj map[string]json.RawMessage
		if json.Unmarshal(trimmed, &obj) == nil {
			for _, key := range gwtype2EnvelopeKeys {
				raw, ok := obj[key]
				if !ok {
					continue
				}
				var s string
				if json.Unmarshal(raw, &s) == nil {
					if r, err := ParseResult(s); err == nil && looksLikeReceipt(r) {
						return r, nil
					}
					continue
				}
				if r, err := parseGWTYPE2Body(raw, depth-1); err == nil {
					return r, nil
				}
			}
		}
	}
	return ParseResult(string(body))
}

// looksLikeReceipt rejects objects that unmarshal cleanly but clearly hold
// no receipt (e.g. {"status": "ok"}).
func looksLikeReceipt(r ScanResult) bool {
	return r.Merchant != "" || r.Date != "" || r.Total != 0 || len(r.Items) > 0
}

// scanLayoutParsing uses the layout-parsing endpoint behind Z.ai's OCR
// models (e.g. glm-ocr). The model only transcribes the document, so the
// receipt structure is recovered afterwards by parseReceiptMarkdown.
func (c *Client) scanLayoutParsing(ctx context.Context, data []byte) (ScanResult, error) {
	var result ScanResult
	req := layoutParsingRequest{
		Model: c.Model,
		File:  base64.StdEncoding.EncodeToString(data),
	}

	respBody, err := c.postJSON(ctx, c.BaseURL+"/layout_parsing", req)
	if err != nil {
		return result, err
	}

	var parsed layoutParsingResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return result, fmt.Errorf("receipt API response was not JSON: %w", err)
	}
	if parsed.Error != nil {
		return result, fmt.Errorf("receipt API error: %s", parsed.Error.Message)
	}

	text := parsed.MDResults
	if text == "" {
		var b strings.Builder
		for _, block := range parsed.LayoutDetails {
			b.WriteString(block.Content)
			b.WriteByte('\n')
		}
		text = b.String()
	}
	if strings.TrimSpace(text) == "" {
		return result, errors.New("receipt API returned no text for this image")
	}
	return parseReceiptMarkdown(text)
}

// postJSON sends a JSON API request and returns the body of a 200 response,
// logging and wrapping API errors otherwise.
func (c *Client) postJSON(ctx context.Context, endpoint string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.post(ctx, endpoint, "application/json", bytes.NewReader(body))
}

// post sends a request body and returns the body of a 200 response, logging
// and wrapping API errors otherwise.
func (c *Client) post(ctx context.Context, endpoint, contentType string, body io.Reader) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Format == FormatAnthropic {
		// Anthropic authenticates with x-api-key and pins an API version;
		// the Bearer header above is kept for gateways proxying the API.
		httpReq.Header.Set("x-api-key", c.APIKey)
		httpReq.Header.Set("anthropic-version", anthropicVersion)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("contacting the receipt API: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		slog.Error("receipt API error", "status", resp.StatusCode, "body", truncate(string(respBody), 500))
		if msg, ok := apiErrorMessage(respBody); ok {
			return nil, fmt.Errorf("receipt API error (%d): %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("receipt API returned status %d", resp.StatusCode)
	}
	return respBody, nil
}

// apiErrorMessage extracts a human-readable message from an API error body,
// which is either nested under "error" (OpenAI/Anthropic) or a top-level
// {"type", "message"} object (gwtype1).
func apiErrorMessage(body []byte) (string, bool) {
	var nested struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(body, &nested) == nil && nested.Error != nil && nested.Error.Message != "" {
		return nested.Error.Message, true
	}
	var plain struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &plain) == nil && plain.Message != "" {
		if plain.Type != "" {
			return plain.Type + ": " + plain.Message, true
		}
		return plain.Message, true
	}
	return "", false
}

// ParseResult extracts the JSON object from a model reply, tolerating
// markdown fences and surrounding prose.
func ParseResult(reply string) (ScanResult, error) {
	var result ScanResult
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return result, errors.New("no JSON object found in the model reply")
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &result); err != nil {
		return result, fmt.Errorf("model reply was not valid receipt JSON: %w", err)
	}
	return result, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
