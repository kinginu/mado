package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kinginu/mado/internal/messages"
)

const (
	imageTimeout     = 3 * time.Minute
	maxImageResponse = 64 << 20
)

// OpenAIImages calls the OpenAI Images API, POST {base}/v1/images/generations.
// gpt-image models always answer base64 (response_format is not accepted, and
// output_format picks the encoding); dall-e models need response_format b64_json.
type OpenAIImages struct {
	BaseURL, Model, Key string
	Size                string // default size; "" = the model's default
	Client              *http.Client
}

// imageSizes are the sizes the Images API documents per model family.
func imageSizes(model string) []string {
	switch {
	case strings.HasPrefix(model, "dall-e-2"):
		return []string{"256x256", "512x512", "1024x1024"}
	case strings.HasPrefix(model, "dall-e-3"):
		return []string{"1024x1024", "1792x1024", "1024x1792"}
	}
	return []string{"1024x1024", "1536x1024", "1024x1536", "auto"}
}

func (o *OpenAIImages) Sizes() []string { return imageSizes(o.Model) }

func gptImage(model string) bool { return !strings.HasPrefix(model, "dall-e") }

func (o *OpenAIImages) Generate(ctx context.Context, prompt, size string) (*Image, error) {
	if size == "" {
		size = o.Size
	}
	body := map[string]any{"model": o.Model, "prompt": prompt, "n": 1}
	if size != "" {
		body["size"] = size
	}
	if gptImage(o.Model) {
		body["output_format"] = "png"
	} else {
		body["response_format"] = "b64_json"
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/v1/images/generations", bytes.NewReader(b))
	if err != nil {
		return nil, Error("image backend misconfigured")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if o.Key != "" {
		req.Header.Set("Authorization", "Bearer "+o.Key)
	}
	c := o.Client
	if c == nil {
		c = &http.Client{Timeout: imageTimeout}
	}
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, Error("image generation failed: " + transportError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageResponse))
	if err != nil {
		return nil, Error("image generation failed: could not read the answer")
	}
	var out struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &out)
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("image generation failed: HTTP %d", resp.StatusCode)
		if out.Error != nil && out.Error.Message != "" {
			msg += ": " + oneLine(out.Error.Message, 300)
		}
		return nil, Error(msg)
	}
	if len(out.Data) == 0 || out.Data[0].B64JSON == "" {
		return nil, Error("image generation failed: no image in the answer")
	}
	img, err := base64.StdEncoding.DecodeString(out.Data[0].B64JSON)
	if err != nil {
		return nil, Error("image generation failed: bad base64")
	}
	mt := http.DetectContentType(img)
	switch mt {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return nil, Error("image generation failed: the answer is not an image")
	}
	res := &Image{Data: img, Mime: mt, Model: o.Model}
	if out.Usage != nil {
		res.Usage = &messages.Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}
	}
	return res, nil
}
