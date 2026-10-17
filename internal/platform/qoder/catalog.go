package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"all2api/internal/adapter"
)

// catalog.go fetches the live model/list catalog the desktop client reads its
// per-model metadata from (context window, completion cap, vision, thinking).
// The fetch authenticates with the COSY session bearer (same as chat) — the
// bare machine-signature family does not cover this endpoint. Any failure
// falls the caller back to the static table; nothing here is fabricated.

const pathModelList = "/algo/api/v2/model/list?Encode=1"

// catalogEntry is one scene="chat" row of the model/list response, keeping
// only the fields the panel surfaces.
type catalogEntry struct {
	Key             string
	DisplayName     string
	MaxInputTokens  int
	MaxOutputTokens int
	IsVL            bool
	IsReasoning     bool
	ThinkingTiers   []string
}

// fetchCatalogMap GETs the catalog and returns the decoded JSON document.
func (s *senderImpl) fetchCatalogMap(ctx context.Context, sess *session) (map[string]any, error) {
	url := s.authBase() + pathModelList
	date := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = chatHeaders(sess, date, sess.buildBearer(date, "", sigPath(url)), "application/json")
	resp, err := s.shortClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<21))
	drainBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, classifyUpstream(resp.StatusCode, raw, resp.Header)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	// The document may be bare or enveloped ({code,message,body}); the chat
	// scene sits at whichever level the gateway answered with.
	if _, ok := doc["chat"]; !ok {
		if inner, ok := doc["body"].(map[string]any); ok {
			if _, ok := inner["chat"]; ok {
				return inner, nil
			}
		}
	}
	return doc, nil
}

// extractCatalogRows walks the chat scene list with the reference's field
// precedence (thinking_config.enabled wins over is_reasoning; enable=false
// rows drop; key "auto" drops).
func extractCatalogRows(doc map[string]any) []catalogEntry {
	scene, ok := doc["chat"].([]any)
	if !ok {
		return nil
	}
	rows := []catalogEntry{}
	for _, item := range scene {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if enable, has := m["enable"]; has && !truthy(enable) {
			continue
		}
		key, _ := m["key"].(string)
		name, _ := m["display_name"].(string)
		if key == "" || name == "" || key == "auto" {
			continue
		}
		row := catalogEntry{Key: key, DisplayName: name, IsReasoning: true}
		if vl, ok := m["is_vl"].(bool); ok {
			row.IsVL = vl
		}
		if n, ok := positiveInt(m["max_input_tokens"]); ok {
			row.MaxInputTokens = n
		}
		if n, ok := positiveInt(m["max_output_tokens"]); ok {
			row.MaxOutputTokens = n
		} else {
			row.MaxOutputTokens = 32000 // the official client's fallback cap
		}
		if tc, ok := m["thinking_config"]; ok {
			if tcMap, isMap := tc.(map[string]any); isMap {
				if enabled, present := tcMap["enabled"]; present && enabled != nil {
					row.IsReasoning = truthy(enabled)
				}
				// efforts ships as either a string list or an object keyed
				// by tier name — accept both shapes.
				if efforts, ok := tcMap["efforts"].([]any); ok {
					for _, e := range efforts {
						if es, ok := e.(string); ok && es != "" {
							row.ThinkingTiers = append(row.ThinkingTiers, es)
						}
					}
				} else if effortsMap, ok := tcMap["efforts"].(map[string]any); ok {
					for name := range effortsMap {
						if name != "" {
							row.ThinkingTiers = append(row.ThinkingTiers, name)
						}
					}
					sort.Strings(row.ThinkingTiers)
				}
			}
		} else if v, present := m["is_reasoning"]; present {
			row.IsReasoning = truthy(v)
		}
		rows = append(rows, row)
	}
	return rows
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != "false" && x != "0" && x != "FALSE"
	case float64:
		return x != 0
	default:
		return true
	}
}

func positiveInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		if n := int(x); float64(n) == x && n > 0 {
			return n, true
		}
	case int:
		if x > 0 {
			return x, true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// liveModelInfos resolves the catalog through the first credentialled account
// session and folds it onto the panel's ModelInfo rows. It errors when no
// account session is available (tests, cold states) — callers fall back to
// the static table, so this path never fabricates rows.
func (s *senderImpl) liveModelInfos(ctx context.Context) ([]*adapter.ModelInfo, error) {
	var st *acctState
	s.statesMu.Lock()
	for _, v := range s.states {
		if v.credToken != "" {
			st = v
			break
		}
	}
	s.statesMu.Unlock()
	if st == nil {
		return nil, fmt.Errorf("qoder: no credentialled session for the catalog fetch")
	}
	sess, err := s.ensureSession(ctx, st.credToken, st)
	if err != nil {
		return nil, err
	}
	doc, err := s.fetchCatalogMap(ctx, sess)
	if err != nil {
		return nil, err
	}
	rows := extractCatalogRows(doc)
	if len(rows) == 0 {
		return nil, fmt.Errorf("qoder: catalog chat scene empty")
	}
	out := make([]*adapter.ModelInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, &adapter.ModelInfo{
			ID:              "qoder/" + row.DisplayName,
			Name:            row.DisplayName,
			ContextSize:     row.MaxInputTokens,
			MaxOutputTokens: row.MaxOutputTokens,
			SupportsStream:  true,
			SupportsVision:  row.IsVL,
			ThinkingTiers:   row.ThinkingTiers,
		})
	}
	return out, nil
}
