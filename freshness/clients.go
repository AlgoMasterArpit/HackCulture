package freshness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// InternalHeader marks the plugin's own model calls so its hooks skip them.
const InternalHeader = "x-bestbefore-internal"

type LLM interface {
	CompleteJSON(ctx context.Context, system, user string) (string, error)
}

type Searcher interface {
	Search(ctx context.Context, query string) ([]Source, error)
}

type Source struct {
	URL           string  `json:"url"`
	Title         string  `json:"title"`
	Content       string  `json:"content"`
	PublishedDate string  `json:"published_date,omitempty"`
	Relevance     float64 `json:"score"`
}

// GatewayLLM sends judge calls back through Bifrost, so they get the same
// failover, budgets and logging as any other request.
type GatewayLLM struct {
	BaseURL    string
	Model      string
	VirtualKey string
	HTTP       *http.Client
}

func (g *GatewayLLM) CompleteJSON(ctx context.Context, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":           g.Model,
		"temperature":     0,
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	headers := map[string]string{InternalHeader: "1"}
	if g.VirtualKey != "" {
		headers["x-bf-vk"] = g.VirtualKey
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := postJSON(ctx, g.HTTP, strings.TrimRight(g.BaseURL, "/")+"/v1/chat/completions", headers, body, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("judge model returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// Tavily talks to api.tavily.com directly, or to a Bifrost relay when BaseURL
// points at /relay/<name> and VirtualKey is set.
type Tavily struct {
	BaseURL    string
	APIKey     string
	VirtualKey string
	MaxResults int
	HTTP       *http.Client
}

func (t *Tavily) Search(ctx context.Context, query string) ([]Source, error) {
	body, _ := json.Marshal(map[string]any{
		"query":        query,
		"search_depth": "basic",
		"max_results":  t.MaxResults,
	})
	headers := map[string]string{}
	if t.APIKey != "" {
		headers["Authorization"] = "Bearer " + t.APIKey
	}
	if t.VirtualKey != "" {
		headers["x-bf-vk"] = t.VirtualKey
	}
	var out struct {
		Results []Source `json:"results"`
	}
	if err := postJSON(ctx, t.HTTP, strings.TrimRight(t.BaseURL, "/")+"/search", headers, body, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %d: %.200s", url, resp.StatusCode, raw)
	}
	return json.Unmarshal(raw, out)
}
