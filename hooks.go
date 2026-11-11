package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlgoMasterArpit/HackCulture/freshness"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	verifyModel = "bestbefore/verify"
	statusModel = "bestbefore/status"
	sweepModel  = "bestbefore/sweep"

	questionKey schemas.BifrostContextKey = "bestbefore-question"
	streamKey   schemas.BifrostContextKey = "bestbefore-stream"
	// cacheKeyHeader is how a caller opts a request into the semantic cache.
	cacheKeyHeader = "x-bf-cache-key"
	// cacheNoStoreKey is the semantic cache's own per-request switch.
	cacheNoStoreKey schemas.BifrostContextKey = "semantic_cache-no_store"
)

// PreLLMHook runs only when the semantic cache missed. It remembers the
// question so PostLLMHook can check the model's answer.
func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	p := activePlugin()
	if p == nil || req.ChatRequest == nil || p.isInternalCall(ctx) || !p.inScope(ctx) {
		return req, nil, nil
	}
	switch req.RequestType {
	case schemas.ChatCompletionRequest:
	case schemas.ChatCompletionStreamRequest:
		ctx.SetValue(streamKey, &streamText{})
	default:
		return req, nil, nil
	}
	ctx.SetValue(questionKey, lastUserText(req.ChatRequest.Input))
	return req, nil, nil
}

// inScope reports whether this request's answer should be checked. By
// default only answers that are going into the semantic cache are, because
// those are the ones replayed to other users; the rest pass with no delay.
func (p *plugin) inScope(ctx *schemas.BifrostContext) bool {
	if p.cfg.CheckScope == "all" {
		return true
	}
	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	return headers[cacheKeyHeader] != ""
}

// streamText collects a streamed answer chunk by chunk.
type streamText struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *streamText) add(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.WriteString(text)
}

func (s *streamText) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// PostLLMHook runs before the semantic cache stores the answer, so a stale
// answer is rewritten with the verified current values and the cache stores
// the corrected version. A stale answer that cannot be rewritten is never
// stored at all.
func PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	p := activePlugin()
	question, asked := ctx.Value(questionKey).(string)
	if p == nil || !asked || bifrostErr != nil || resp == nil || resp.ChatResponse == nil {
		return resp, bifrostErr, nil
	}
	if stream, ok := ctx.Value(streamKey).(*streamText); ok {
		p.checkStream(ctx, question, stream, resp.ChatResponse)
		return resp, bifrostErr, nil
	}
	if len(resp.ChatResponse.Choices) != 1 {
		return resp, bifrostErr, nil
	}
	msg := firstMessage(resp.ChatResponse)
	answer := contentText(msg)
	if answer == "" {
		return resp, bifrostErr, nil
	}
	out := p.check(question, answer, true)
	switch {
	case out.Healed:
		healed := out.answer
		msg.Content = &schemas.ChatMessageContent{ContentStr: &healed}
	case out.Stale() && p.evictsStale():
		ctx.SetValue(cacheNoStoreKey, true)
	}
	p.pending.put(pendingKey(resp.ChatResponse.ID, contentText(msg)), question, out)
	return resp, bifrostErr, nil
}

// checkStream gathers a streamed answer and checks it on the final chunk.
// The text has already reached the user, so a stale answer cannot be
// rewritten in place: the correction is appended as one last delta, the
// report rides on the final chunk, and the stale stream is kept out of the
// cache so the next caller gets a fresh, checked answer.
func (p *plugin) checkStream(ctx *schemas.BifrostContext, question string, stream *streamText, chunk *schemas.BifrostChatResponse) {
	for _, choice := range chunk.Choices {
		if choice.Index == 0 && choice.ChatStreamResponseChoice != nil && choice.Delta != nil && choice.Delta.Content != nil {
			stream.add(*choice.Delta.Content)
		}
	}
	if final, _ := ctx.Value(schemas.BifrostContextKeyStreamEndIndicator).(bool); !final {
		return
	}
	answer := stream.String()
	if answer == "" {
		return
	}
	out := p.check(question, answer, true)
	if out.Stale() || out.Healed {
		ctx.SetValue(cacheNoStoreKey, true)
	}
	if out.Healed {
		appendDelta(chunk, "\n\n---\nCorrected by BestBefore: "+out.Corrected)
	}
	if chunk.ExtraFields.RawResponse == nil {
		chunk.ExtraFields.RawResponse = map[string]any{"bestbefore": out}
	}
}

// appendDelta adds text to the final chunk's first choice, creating one if
// the provider sent a final chunk without choices.
func appendDelta(chunk *schemas.BifrostChatResponse, text string) {
	for i := range chunk.Choices {
		c := &chunk.Choices[i]
		if c.Index != 0 || c.ChatStreamResponseChoice == nil {
			continue
		}
		if c.Delta == nil {
			c.Delta = &schemas.ChatStreamResponseChoiceDelta{}
		}
		joined := text
		if c.Delta.Content != nil {
			joined = *c.Delta.Content + text
		}
		c.Delta.Content = &joined
		return
	}
	chunk.Choices = append(chunk.Choices, schemas.BifrostResponseChoice{
		ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
			Delta: &schemas.ChatStreamResponseChoiceDelta{Content: &text},
		},
	})
}

// HTTPTransportPreHook serves three virtual models, so everything stays inside
// the OpenAI-compatible API: bestbefore/verify checks a pasted answer,
// bestbefore/status lists every cached answer with its best-before date, and
// bestbefore/sweep re-checks all of them now.
func HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	p := activePlugin()
	if p == nil || !isChatCompletion(req) || p.isInternal(req) {
		return nil, nil
	}
	model := gjson.GetBytes(req.Body, "model").String()
	switch {
	case model == verifyModel:
		return p.serveVerify(req), nil
	case (model == statusModel || model == sweepModel) && !p.cfg.AdminModels:
		return jsonResponse(403, map[string]any{"error": map[string]string{
			"message": model + " is turned off; set admin_models in the bestbefore plugin config",
		}}), nil
	case model == statusModel:
		return p.serveStatus(nil), nil
	case model == sweepModel:
		swept, ran := p.sweep(true)
		if !ran {
			return chatJSON(sweepModel, "A sweep is already running.", "bestbefore_status", map[string]any{"busy": true}), nil
		}
		return p.serveStatus(swept), nil
	}
	return nil, nil
}

// HTTPTransportPostHook annotates every non-streaming chat answer. For a cache
// miss it reports what PostLLMHook already did. For a cache hit it serves the
// stored report while the answer is within its best-before date, and checks
// it again once that date has passed.
func HTTPTransportPostHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponse) error {
	p := activePlugin()
	if p == nil || !isChatCompletion(req) || p.isInternal(req) || resp.StatusCode != 200 || len(resp.Body) == 0 {
		return nil
	}
	switch gjson.GetBytes(req.Body, "model").String() {
	case verifyModel, statusModel, sweepModel:
		return nil
	}
	if gjson.GetBytes(req.Body, "stream").Bool() || !gjson.ValidBytes(resp.Body) || gjson.GetBytes(resp.Body, "choices.#").Int() != 1 {
		return nil
	}
	answer := messageText(gjson.GetBytes(resp.Body, "choices.0.message"))
	if answer == "" {
		return nil
	}
	cacheID := gjson.GetBytes(resp.Body, "extra_fields.cache_debug.cache_id").String()
	cacheHit := gjson.GetBytes(resp.Body, "extra_fields.cache_debug.cache_hit").Bool()
	question := lastUserMessage(req.Body)
	if p.cfg.CheckScope == "cached" && cacheID == "" {
		return nil
	}

	if done, ok := p.takePending(cacheHit, gjson.GetBytes(resp.Body, "id").String(), answer); ok {
		out := done.outcome
		if cacheID != "" {
			switch {
			case out.Stale():
				p.ledger.Delete(cacheID)
				if p.evictsStale() {
					out.CacheEvicted = p.evictTwice(cacheID)
				}
			case out.Status != freshness.StatusUnchecked:
				p.ledger.Put(cacheID, out.ledgerEntry(done.question))
			}
		}
		return annotate(resp, out)
	}

	if stored, ok := p.ledger.Current(cacheID, time.Now()); ok && cacheHit {
		p.stats.ledgerHits.Add(1)
		return annotate(resp, outcome{Report: stored, Origin: "ledger", CacheHit: true, Healed: stored.Status == freshness.StatusHealed})
	}

	if cacheHit && p.cfg.Revalidate == "background" {
		// Past its best-before date: answer now, re-check behind the scenes.
		// If it went stale, it is evicted and the next caller gets it healed.
		p.revalidateLater(cacheID, question, answer)
		last, known := p.ledger.Get(cacheID)
		out := outcome{Origin: "revalidating", CacheHit: true}
		if known {
			out.Report, out.Healed = last.Report, last.Report.Status == freshness.StatusHealed
		} else {
			out.Report = freshness.Report{Status: freshness.StatusUnchecked, Score: 0.5, Claims: []freshness.ClaimReport{}, Error: "being re-checked in the background"}
		}
		return annotate(resp, out)
	}

	out := p.check(question, answer, true)
	out.CacheHit = cacheHit
	if out.Healed {
		if body, err := sjson.SetBytes(resp.Body, "choices.0.message.content", out.answer); err == nil {
			resp.Body = body
		}
	}
	if cacheID != "" {
		switch {
		case out.Stale() || out.Healed:
			// The cache still holds the stale text, so clear it; the next miss
			// stores a corrected answer through PostLLMHook.
			p.ledger.Delete(cacheID)
			if p.evictsStale() {
				out.CacheEvicted = p.evictTwice(cacheID)
			}
		case out.Status != freshness.StatusUnchecked:
			p.ledger.Put(cacheID, out.ledgerEntry(question))
		}
	}
	return annotate(resp, out)
}

func (p *plugin) serveVerify(req *schemas.HTTPRequest) *schemas.HTTPResponse {
	messages := gjson.GetBytes(req.Body, "messages").Array()
	if len(messages) == 0 {
		return jsonResponse(400, map[string]any{"error": map[string]string{
			"message": "bestbefore/verify needs the answer to check as the last message",
		}})
	}
	answer := messageText(messages[len(messages)-1])
	question := ""
	if len(messages) > 1 {
		question = messageText(messages[len(messages)-2])
	}
	out := p.check(question, answer, true)
	return chatJSON(verifyModel, summarize(out), "bestbefore", out)
}

func (p *plugin) serveStatus(swept []sweepResult) *schemas.HTTPResponse {
	rows := p.ledger.Snapshot()
	status := map[string]any{
		"now":     time.Now().UTC(),
		"entries": rows,
		"counts":  p.stats.snapshot(),
	}
	text := fmt.Sprintf("%d cached answers are tracked.", len(rows))
	model := statusModel
	if swept != nil {
		status["swept"] = swept
		evicted := 0
		for _, r := range swept {
			if r.Evicted {
				evicted++
			}
		}
		text = fmt.Sprintf("Re-checked %d cached answers and evicted %d that had gone stale.", len(swept), evicted)
		model = sweepModel
	}
	return chatJSON(model, text, "bestbefore_status", status)
}

func chatJSON(model, text, field string, payload any) *schemas.HTTPResponse {
	now := time.Now()
	return jsonResponse(200, map[string]any{
		"id":      "bestbefore-" + strconv.FormatInt(now.UnixNano(), 36),
		"object":  "chat.completion",
		"created": now.Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message":       map[string]string{"role": "assistant", "content": text},
		}},
		field: payload,
	})
}

func annotate(resp *schemas.HTTPResponse, out outcome) error {
	body, err := sjson.SetBytes(resp.Body, "bestbefore", out)
	if err != nil {
		return nil
	}
	resp.Body = body
	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	resp.Headers["x-bestbefore-status"] = string(out.Status)
	resp.Headers["x-bestbefore-score"] = strconv.FormatFloat(out.Score, 'f', 2, 64)
	if out.CacheEvicted {
		resp.Headers["x-bestbefore-cache-evicted"] = "true"
	}
	if out.Healed {
		resp.Headers["x-bestbefore-healed"] = "true"
	}
	return nil
}

// pendingKey pairs the provider's response id with the answer text, so two
// responses that share an id (some providers and proxies reuse them) cannot
// pick up each other's result.
func pendingKey(id, answer string) string {
	sum := sha256.Sum256([]byte(answer))
	return id + "|" + hex.EncodeToString(sum[:8])
}

// takePending returns PostLLMHook's result for this response. A cache hit
// never has one, so it must not consume an entry that belongs to a miss.
func (p *plugin) takePending(cacheHit bool, id, answer string) (pendingCheck, bool) {
	if cacheHit {
		return pendingCheck{}, false
	}
	return p.pending.take(pendingKey(id, answer))
}

func isChatCompletion(req *schemas.HTTPRequest) bool {
	return req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/chat/completions")
}

func (p *plugin) isInternal(req *schemas.HTTPRequest) bool {
	return req.CaseInsensitiveHeaderLookup(freshness.InternalHeader) == p.token
}

func (p *plugin) isInternalCall(ctx *schemas.BifrostContext) bool {
	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	return headers[freshness.InternalHeader] == p.token
}

func firstMessage(resp *schemas.BifrostChatResponse) *schemas.ChatMessage {
	if len(resp.Choices) == 0 || resp.Choices[0].ChatNonStreamResponseChoice == nil {
		return nil
	}
	return resp.Choices[0].ChatNonStreamResponseChoice.Message
}

func contentText(msg *schemas.ChatMessage) string {
	if msg == nil || msg.Content == nil {
		return ""
	}
	if msg.Content.ContentStr != nil {
		return *msg.Content.ContentStr
	}
	var parts []string
	for _, block := range msg.Content.ContentBlocks {
		if block.Text != nil {
			parts = append(parts, *block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func lastUserText(messages []schemas.ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == schemas.ChatMessageRoleUser {
			return contentText(&messages[i])
		}
	}
	return ""
}

func lastUserMessage(body []byte) string {
	users := gjson.GetBytes(body, `messages.#(role=="user")#`).Array()
	if len(users) == 0 {
		return ""
	}
	return messageText(users[len(users)-1])
}

// messageText reads both plain string content and multi-part content arrays.
func messageText(message gjson.Result) string {
	content := message.Get("content")
	if !content.IsArray() {
		return content.String()
	}
	var parts []string
	for _, part := range content.Array() {
		if text := part.Get("text").String(); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func summarize(out outcome) string {
	var b strings.Builder
	if out.Healed {
		b.WriteString("Corrected answer:\n")
		b.WriteString(out.answer)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Freshness: %s (score %.2f)\n", out.Status, out.Score)
	if out.Error != "" {
		fmt.Fprintf(&b, "Could not verify: %s\n", out.Error)
	}
	for _, c := range out.Claims {
		fmt.Fprintf(&b, "\n[%s %.2f] %s", strings.ToUpper(string(c.Status)), c.Score, c.Claim)
		if c.CurrentValue != "" {
			fmt.Fprintf(&b, "\n  now: %s", c.CurrentValue)
		}
		if c.SourceURL != "" {
			fmt.Fprintf(&b, "\n  source: %s", c.SourceURL)
		}
	}
	if len(out.Claims) == 0 && out.Error == "" {
		b.WriteString("No time-sensitive claims found.")
	}
	return b.String()
}

func jsonResponse(status int, payload any) *schemas.HTTPResponse {
	body, _ := json.Marshal(payload)
	return &schemas.HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       body,
	}
}
