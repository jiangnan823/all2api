//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// tasks.go implements the scheduled-task surface of the WorkBuddy upstream
// (docs/upstream/workbuddy-protocol.md §7), the M3b counterpart of the chat
// sender in client.go:
//
//   - daily check-in          POST {billing}/v2/billing/meter/daily-checkin
//   - checkin activity status POST {billing}/v2/billing/meter/checkin-activity-status
//   - active report           POST {billing}/v2/report (event array)
//   - cat travel cycle        GET/POST {chat}/activity/growth/buddy/travel/{status,depart,claim}
//   - balance query           POST {billing}/v2/billing/meter/get-user-resource
//   - token keepalive         the M3a refresh chain (refresh.go), reused as-is
//
// Wire facts coded here, all sourced from the report:
//   - Billing domain header family: the chat header set with a single-segment
//     UA "WorkBuddy/<ver>" (no platform/CLI suffixes, report §7) and
//     X-Tenant-Id from the enterprise id (report §3). The growth endpoints
//     live on the chat base and use the full chat header family.
//   - Business envelope {code,msg,data} is decoupled from the HTTP status
//     (report §6 headnote): code 0 is success, everything else is classified
//     through the shared classifyError EXCEPT the two task-domain special
//     cases — the check-in idempotent reject (codes 10001/14001 carrying the
//     已签到 wording, report §7) and the travel daily_limit_reached marker.
//     The wording, not the bare code, is the discriminator: 14001 without it
//     is the chat-domain quota-exhaustion meaning (report §6).
//   - The balance shape is data.Response.Data.Accounts[] filtered by
//     CapacityUnit=="credits", reading CycleCapacityRemain (spendable) and
//     CycleCapacitySize (package capacity, panel credit_total) per row
//     (report §2).
//
// The client is stdlib-only and every endpoint base is injectable, so tests
// run against httptest stubs only — the suite never contacts the real
// endpoints (task red line).
package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/pool"
)

// Task endpoint bases and paths (report §2/§7). The billing domain is a
// separate base from chat: CN www.codebuddy.cn, global www.workbuddy.ai.
// The report documents a /v2-less billing path preference for the global
// realm with a 404 fallback to /v2 — not implemented here (recorded gap; the
// /v2 form is the contract this batch codes to).
const (
	billingDefaultBaseCN     = "https://www.codebuddy.cn"
	billingDefaultBaseGlobal = "https://www.workbuddy.ai"

	pathDailyCheckin    = "/v2/billing/meter/daily-checkin"
	pathCheckinStatus   = "/v2/billing/meter/checkin-activity-status"
	pathGetUserResource = "/v2/billing/meter/get-user-resource"
	pathReport          = "/v2/report"

	pathTravelStatus = "/activity/growth/buddy/travel/status"
	pathTravelDepart = "/activity/growth/buddy/travel/depart"
	pathTravelClaim  = "/activity/growth/buddy/travel/claim"
)

// task-domain sentinels. They never escape the package: the methods fold them
// into their success semantics (already-signed → delta 0; daily limit → the
// task is done for today).
var (
	errCheckinAlreadyDone = errors.New("workbuddy: already checked in today (idempotent reject treated as success)")
	errTravelDailyLimit   = errors.New("workbuddy: travel daily limit reached (done for today)")
)

// TaskClientOptions configures one task client. Zero value is
// production-shaped: realm-default bases, no credential wiring (Refresh then
// fails with an explicit error), a 30s-bounded transport (task calls are short
// JSON round-trips, unlike SSE chat).
type TaskClientOptions struct {
	// ChatBaseURL overrides the chat-realm base (growth travel endpoints and
	// the refresh chain). Tests point this at an httptest server.
	ChatBaseURL string
	// BillingBaseURL overrides the billing-realm base (check-in, report,
	// balance). Tests point this at the same stub server.
	BillingBaseURL string
	// HTTPClient overrides the transport. Nil uses a 30s-timeout client.
	HTTPClient *http.Client
	// Credentials resolves stored credentials for the Refresh chain. Nil only
	// disables Refresh; every other method takes the credential directly.
	Credentials CredentialSource
	// Refresher receives tokens rotated by Refresh (the credential store).
	Refresher CredentialSink
	// Now overrides the clock for refresh expiry stamps (tests).
	Now func() time.Time
}

// TaskClient performs the per-account scheduled-task operations. Methods take
// the credential explicitly (the caller — the host schedule upstream —
// resolves it from the credential store), so the client itself is stateless
// and safe for concurrent use across accounts. Refresh is the one exception:
// it runs the gated M3a refresh chain keyed by account.
type TaskClient struct {
	opts   TaskClientOptions
	http   *http.Client
	sender *senderImpl // hosts the M3a refresh chain (gating + sink write-back)
}

// NewTaskClient builds a task client. The embedded sender reuses the exact
// M3a refresh configuration (same credential source/sink/base overrides) so
// token_keepalive and the 401 path cannot drift apart.
func NewTaskClient(opts TaskClientOptions) *TaskClient {
	hc := opts.HTTPClient
	if hc == nil {
		// Unlike chat SSE, task calls are bounded request/reply exchanges: a
		// transport timeout keeps a hung billing endpoint from stalling a
		// cron batch (or startup seeding) forever. ctx stays the primary
		// cancellation surface.
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	s := newSender()
	s.Configure(ClientOptions{
		BaseURL:     opts.ChatBaseURL,
		HTTPClient:  hc,
		Credentials: opts.Credentials,
		Refresher:   opts.Refresher,
		Now:         opts.Now,
	})
	return &TaskClient{opts: opts, http: hc, sender: s}
}

// baseFor returns the endpoint base for the credential's realm.
func (c *TaskClient) baseFor(cred *Credential, billing bool) string {
	override := c.opts.ChatBaseURL
	if billing {
		override = c.opts.BillingBaseURL
	}
	if override != "" {
		return override
	}
	global := cred != nil && cred.Global
	if billing {
		if global {
			return billingDefaultBaseGlobal
		}
		return billingDefaultBaseCN
	}
	if global {
		return wireDefaultBaseGlobal
	}
	return wireDefaultBaseCN
}

// billingUserAgent is the billing-domain UA variant (report §7): the single
// "WorkBuddy/<ver>" segment, no platform/CLI suffixes — the documented
// BillingHeaders shape, shared by both realms.
func billingUserAgent() string {
	return "WorkBuddy/" + wireClientVersion
}

// do performs one task round-trip. Returns the raw reply body on success.
// Classification order mirrors client.go: transport/ctx failures map onto the
// canonical UpstreamError reasons, HTTP >= 400 goes through classifyError,
// and an HTTP-2xx body carrying a non-zero {code} envelope goes through
// taskBusinessError first (the task-domain idempotent/limit codes must win
// over the generic buckets).
func (c *TaskClient) do(ctx context.Context, cred *Credential, method, path string, body []byte, billing bool) (string, error) {
	if cred == nil || cred.AccessToken == "" {
		return "", errors.New("workbuddy: task call needs a credential with an access token")
	}
	url := strings.TrimRight(c.baseFor(cred, billing), "/") + path
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return "", err
	}
	setCommonHeaders(req, cred)
	if billing {
		req.Header.Set("User-Agent", billingUserAgent())
	}
	req.Header.Set("Accept", "application/json")
	setAccountHeaders(req, cred)

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", &adapter.UpstreamError{Status: http.StatusGatewayTimeout, Reason: adapter.ReasonTimeout, Message: "workbuddy: task " + path + ": " + ctx.Err().Error()}
		}
		return "", &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: task " + path + " transport: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: read task " + path + " body: " + err.Error()}
	}
	text := string(raw)
	if resp.StatusCode >= 400 {
		return "", classifyError(resp.StatusCode, text, resp.Header)
	}
	if code, ok := businessCode(text); ok && code != "" && code != "0" {
		return "", taskBusinessError(resp.StatusCode, text, code)
	}
	return text, nil
}

// taskBusinessError classifies a {code,msg,data} rejection delivered inside
// HTTP 2xx (report §6 headnote: business codes are decoupled from the HTTP
// status). Task-domain special cases come first:
//
//   - check-in idempotent reject: codes 10001/14001 carrying the 已签到
//     wording mean "already checked in today" (report §7). 14001 without the
//     wording is the chat-domain quota-exhaustion meaning (report §6) and
//     must classify as quota, so the wording — not the bare code — is the
//     discriminator.
//   - travel daily_limit_reached anywhere in the body: today's travel quota
//     is used up, which is completion (resets 00:00 CST, report §7).
//
// Everything else falls through to the shared chat classification (quota
// exhaustion, auth/session death, rate limits, WAF, platform faults).
func taskBusinessError(status int, body, code string) error {
	if (code == "10001" || code == "14001") && strings.Contains(envelopeMessage(body), "已签到") {
		return errCheckinAlreadyDone
	}
	if strings.Contains(body, "daily_limit_reached") {
		return errTravelDailyLimit
	}
	return classifyError(status, body, nil)
}

// ---------- envelope helpers ----------

// envelopeData parses the reply's data object tolerantly; missing or
// non-object data yields nil.
func envelopeData(body string) map[string]any {
	var root map[string]any
	if json.Unmarshal([]byte(body), &root) != nil {
		return nil
	}
	data, _ := root["data"].(map[string]any)
	return data
}

// numberOf reads a JSON number tolerantly: the native float64 form or a
// digit string form both parse.
func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

// creditsFrom converts a parsed award/balance number into a bounded int64:
// NaN/Inf, negatives and absurd magnitudes are rejected so protocol noise
// never lands in the pool as a credit value.
func creditsFrom(n float64) (int64, bool) {
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 1e15 {
		return 0, false
	}
	return int64(math.Round(n)), true
}

// firstString returns the first non-empty string under the given keys.
func firstString(data map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := data[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// taskAward extracts the credit a task run awarded. The protocol report pins
// reward_credit (travel claim) and daily_credit (checkin activity status) as
// field names but not the check-in reply's own award field, so the lookup
// walks the documented names best-effort and defaults to 0 — an unknown shape
// awards nothing rather than inventing a number.
func taskAward(data map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if n, ok := numberOf(data[k]); ok {
			if credits, ok := creditsFrom(n); ok {
				return credits
			}
		}
	}
	return 0
}

// ---------- task operations ----------

// Signin executes the daily check-in: POST {billing}/v2/billing/meter/
// daily-checkin with the empty object body (report §7). Returns the credit
// this run awarded. An idempotent "already checked in" reply is success with
// delta 0 — nothing new was earned by THIS run, and the caller would
// double-count a delta written for it. An unparseable award shape is delta 0
// (see taskAward); the real field name is an E2E observation point.
func (c *TaskClient) Signin(ctx context.Context, cred *Credential) (int64, error) {
	body, err := c.do(ctx, cred, http.MethodPost, pathDailyCheckin, []byte("{}"), true)
	if err != nil {
		if errors.Is(err, errCheckinAlreadyDone) {
			return 0, nil
		}
		return 0, err
	}
	return taskAward(envelopeData(body), "reward_credit", "daily_credit", "credit", "add_credit"), nil
}

// CheckinActivityStatus queries POST {billing}/v2/billing/meter/
// checkin-activity-status — POST is mandatory, GET answers 404 (report §7).
// Returns the raw data object (documented fields: active, today_checked_in,
// streak_days, daily_credit, start_time, end_time, theme_name).
func (c *TaskClient) CheckinActivityStatus(ctx context.Context, cred *Credential) (map[string]any, error) {
	body, err := c.do(ctx, cred, http.MethodPost, pathCheckinStatus, []byte("{}"), true)
	if err != nil {
		return nil, err
	}
	return envelopeData(body), nil
}

// ReportActive posts the daily active signal: an event array carrying one
// chat_request_send event whose userId is the account uid — the documented
// required field (missing → HTTP 200 but silently dropped upstream, §7).
//
// Disambiguation decision (report §7 ⚠): xiaofan6ya's reverse-engineering
// found that growth progress is driven by the chat body's extra_vars.
// growthEvent, while /v2/report does not drive accounting. This client
// therefore treats the report purely as a presence signal — success is the
// envelope code 0, and no credit or streak value is ever derived from it.
func (c *TaskClient) ReportActive(ctx context.Context, cred *Credential) error {
	if cred == nil || cred.UID == "" {
		return errors.New("workbuddy: active report needs the account uid (userId is the required event field)")
	}
	// Full client chat_request_send shape (wb_research reference: "do not use a
	// minimal field set, in case the upstream tightens validation") — live
	// testing 2026-10-15 10:00 cron rejected a bare {eventCode,userId} event on
	// all four accounts. Every field stays present with zero values;
	// conversationId is synthetic (the server does not cross-check it), userId
	// is the account uid.
	now := time.Now().UnixMilli()
	conv := "all2api-" + strconv.FormatInt(now, 10)
	ev := map[string]any{
		"eventCode":             "chat_request_send",
		"timestamp":             now,
		"reportDelay":           0,
		"mode":                  "craft",
		"conversationId":        conv,
		"requestId":             conv,
		"inputLength":           12,
		"requestModelId":        "deepseek-v4-flash",
		"requestModelName":      "DeepSeek V4 Flash",
		"isPlan":                false,
		"isAutoExecuteTerminal": false,
		"isAutoModify":          false,
		"codebaseEnable":        false,
		"maxToken":              0,
		"maxSteps":              0,
		"temperature":           0,
		"maxRetries":            0,
		"mentionContexts":       []any{},
		"knowledgeId":           []any{},
		"knowledgeName":         []any{},
		"codebaseId":            "",
		"mentionContextCount":   0,
		"command":               "",
		"expertId":              "",
		"recommendId":           "",
		"skillId":               "",
		"skillCount":            0,
		"totalCount":            0,
		"fileUri":               "",
		"presentAt":             now,
		"traceId":               "",
		"rootRequestId":         conv,
		"parentConversationId":  conv,
		"agentName":             "default",
		"agentType":             "conversation",
		"userId":                cred.UID,
	}
	body, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cred, http.MethodPost, pathReport, body, true)
	return err
}

// TravelSnapshot is the parsed travel/status reply.
type TravelSnapshot struct {
	// State is the travel state machine value: idle / traveling / arrived
	// (report §7), lowercased.
	State string
	// RecordID is the claimable record when arrived. The report does not pin
	// the field name, so it is read best-effort from record_id / recordId / id.
	RecordID int64
}

// TravelStatus GETs {chat}/activity/growth/buddy/travel/status (chat base,
// full chat header family).
func (c *TaskClient) TravelStatus(ctx context.Context, cred *Credential) (TravelSnapshot, error) {
	body, err := c.do(ctx, cred, http.MethodGet, pathTravelStatus, nil, false)
	if err != nil {
		return TravelSnapshot{}, err
	}
	data := envelopeData(body)
	snap := TravelSnapshot{
		State: strings.ToLower(firstString(data, "status", "state", "travel_status", "travel_state")),
	}
	for _, k := range []string{"record_id", "recordId", "id"} {
		if n, ok := numberOf(data[k]); ok {
			if id, ok := creditsFrom(n); ok {
				snap.RecordID = id
				break
			}
		}
	}
	return snap, nil
}

// TravelDepart starts one travel: POST depart with {"location_id": 1..4}
// (report §7). The report pins no reward difference between locations, so
// the caller chooses.
func (c *TaskClient) TravelDepart(ctx context.Context, cred *Credential, locationID int64) error {
	if locationID < 1 || locationID > 4 {
		return fmt.Errorf("workbuddy: travel location_id %d out of range 1..4", locationID)
	}
	body, err := json.Marshal(map[string]any{"location_id": locationID})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cred, http.MethodPost, pathTravelDepart, body, false)
	return err
}

// TravelClaim collects one arrived travel: POST claim with
// {"record_id":<int64>}; the reply's data.reward_credit is the award
// (report §7).
func (c *TaskClient) TravelClaim(ctx context.Context, cred *Credential, recordID int64) (int64, error) {
	body, err := json.Marshal(map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	respBody, err := c.do(ctx, cred, http.MethodPost, pathTravelClaim, body, false)
	if err != nil {
		return 0, err
	}
	if n, ok := numberOf(envelopeData(respBody)["reward_credit"]); ok {
		if credits, ok := creditsFrom(n); ok {
			return credits, nil
		}
	}
	return 0, nil
}

// TravelCycle advances the cat-travel state machine by exactly one step
// (report §7):
//
//	arrived   → claim the waiting reward, return it as the credit delta
//	traveling → nothing collectable yet, delta 0
//	idle      → depart (location 1); the reward arrives on a later run
//
// daily_limit_reached on any leg folds into (0, nil) — today's quota is used
// up, which is completion, not failure. One step per call: the twice-daily
// cron cadence (depart 09:00, claim 21:00) drives the cycle. An empty state
// string is probed with a departure — the depart reply is what reveals the
// real state (a daily-limit reject folds to done).
func (c *TaskClient) TravelCycle(ctx context.Context, cred *Credential) (int64, error) {
	snap, err := c.TravelStatus(ctx, cred)
	if err != nil {
		if errors.Is(err, errTravelDailyLimit) {
			return 0, nil
		}
		return 0, err
	}
	switch snap.State {
	case "arrived":
		if snap.RecordID == 0 {
			return 0, errors.New("workbuddy: travel arrived but the status reply carried no record id to claim")
		}
		reward, err := c.TravelClaim(ctx, cred, snap.RecordID)
		if err != nil {
			if errors.Is(err, errTravelDailyLimit) {
				return 0, nil
			}
			return 0, err
		}
		return reward, nil
	case "traveling":
		return 0, nil
	case "idle", "":
		if err := c.TravelDepart(ctx, cred, 1); err != nil {
			if errors.Is(err, errTravelDailyLimit) {
				return 0, nil
			}
			return 0, err
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("workbuddy: unknown travel state %q", snap.State)
	}
}

// GetUserResource posts {billing}/v2/billing/meter/get-user-resource with the
// empty object body and reads the credits wallet: data.Response.Data.Accounts
// filtered to CapacityUnit=="credits", SUMMED over every package row. It
// returns two sums — remain (CycleCapacityRemain, the spendable balance the
// credit floor consumes) and total (CycleCapacitySize, the account's full
// package capacity the panel renders as credit_total).
//
// Live evidence (2026-10-15, owner account with 32 packages): the wallet is
// NOT one row — an account holds a monthly pack plus many bonus packs, and
// the first credits row can be an exhausted monthly allowance (CycleCapacity-
// Remain 0 while other packs hold thousands). "First row wins" read 0 for an
// account the official app shows ≈5,300 credits. The honest roll-up is the
// sum over all credits rows; non-numeric rows are skipped rather than
// aborting the sum (a single malformed pack must not hide the rest of the
// balance). An absent/empty Accounts array stays an explicit error — never an
// invented 0.
func (c *TaskClient) GetUserResource(ctx context.Context, cred *Credential) (remain, total int64, err error) {
	body, err := c.do(ctx, cred, http.MethodPost, pathGetUserResource, []byte("{}"), true)
	if err != nil {
		return 0, 0, err
	}
	resp, ok := envelopeData(body)["Response"].(map[string]any)
	if !ok {
		return 0, 0, errors.New("workbuddy: user-resource reply missing data.Response object")
	}
	inner, ok := resp["Data"].(map[string]any)
	if !ok {
		return 0, 0, errors.New("workbuddy: user-resource reply missing data.Response.Data object")
	}
	accounts, ok := inner["Accounts"].([]any)
	if !ok {
		return 0, 0, errors.New("workbuddy: user-resource reply missing data.Response.Data.Accounts array")
	}
	var remainSum, totalSum int64
	rows := 0
	for _, row := range accounts {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		unit, _ := m["CapacityUnit"].(string)
		if !strings.EqualFold(strings.TrimSpace(unit), "credits") {
			continue // token/compute wallets are not the credit balance
		}
		rows++
		if n, ok := numberOf(m["CycleCapacityRemain"]); ok {
			if credits, ok := creditsFrom(n); ok {
				remainSum += credits
			}
		}
		if n, ok := numberOf(m["CycleCapacitySize"]); ok {
			if size, ok := creditsFrom(n); ok {
				totalSum += size
			}
		}
		// Precise decimal variants (CapacityRemainPrecise family) exist but
		// carry fractional dust; the integer cycle sums are the display truth
		// the panel and the credit floor both consume. A row without a size
		// field contributes to remain only — total stays partial but honest.
	}
	if rows == 0 {
		return 0, 0, errors.New("workbuddy: user-resource reply has no CapacityUnit=credits account row")
	}
	return remainSum, totalSum, nil
}

// Refresh rotates the account credential through the M3a refresh chain
// (refresh.go), reused untouched for the nightly token_keepalive task: stored
// credential → POST {chat}/v2/plugin/auth/token/refresh (X-Refresh-Token +
// X-Auth-Refresh-Source: plugin, never an Authorization bearer) → sink
// write-back, under the single-flight gate and quiet window. The rotated
// pair's ExpiresAt (expiresIn seconds, observed constant 60d) is the honest
// keepalive result; zero means the upstream gave no expiry.
func (c *TaskClient) Refresh(ctx context.Context, acct *pool.Account) (*Credential, error) {
	return c.sender.rotateCredential(ctx, acct, nil)
}
