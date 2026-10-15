//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// growth.go implements the growth credit-task surface of the WorkBuddy
// upstream — the M7a counterpart of the scheduled-task methods in tasks.go.
// Ported endpoint-for-endpoint from the wb2api panel reference
// (workbuddy2api-panel internal/upstream/tasks.go + panel/tasks.go, itself
// sourced from upstream scripts/task_common.py live testing):
//
//	GET  {chat}/v2/activity/growth/tasks          full task list (default realm)
//	GET  {chat}/v2/activity/growth/tasks          mp list (X-Client-Platform:
//	                                              miniprogram — superset, carries
//	                                              the mp-exclusive task codes)
//	POST {chat}/v2/activity/growth/tasks/accept   {"task_codes":[...]} batch sign-up
//	POST {web}/activity/growth/tasks/{code}/claim claim one reward, code in the
//	                                              PATH (the /v2/.../reward/claim
//	                                              body form does not exist —
//	                                              wb2api's recorded 400 trap)
//
// Semantics ported verbatim from the reference:
//   - The task list is the merge of the default and miniprogram lists,
//     deduped by task_code; an mp-list failure is silent (normal once an mp
//     campaign ends).
//   - accept is a sign-up, never progress: progress lights up through server
//     behavior events. "Accept all" = every task not yet accepted/completed/
//     claimed and not locked, in batches of 20 with a 1.05s inter-batch gap
//     (anti-risk-control cadence); mp codes are accepted through the mp
//     header, whose failure does not block the default pass.
//   - claim only pays out once progress is full; the reply data carries
//     {"already_claimed":bool,"credit":n,"energy":n}. Default codes claim on
//     the web domain (workbuddy.cn, x-client-platform: web); mp codes claim
//     on the chat domain with the mp header and fall back to the web domain
//     on chat-domain 400.
//
// The wire status derivation (claimed | claimable | completed | in_progress |
// accepted | not_accepted) is this package's mapping of the upstream
// accept_status state machine (report §7: not_accepted → accepted →
// in_progress → completed → claimed; completed ≠ claimed): anything with full
// progress and no payout yet — including accept_status "completed", the
// async-scoring form where the progress counters lag — is "claimable", the
// exact bucket the claim-auto sweep targets.
//
// The client is stdlib-only; every base is injectable (TaskClientOptions), so
// tests run against httptest stubs only — the suite never contacts the real
// endpoints (task red line).
package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"all2api/internal/adapter"
)

// Growth endpoint paths and realm bases (wb2api tasks.go paths, verbatim).
const (
	pathGrowthTasks       = "/v2/activity/growth/tasks"
	pathGrowthTasksAccept = "/v2/activity/growth/tasks/accept"
	pathGrowthClaimPrefix = "/activity/growth/tasks/"

	// webDefaultBaseCN is the growth-center web base for the CN realm (the
	// claim family; wb2api webBase default). The global realm serves the same
	// paths on the realm base already defined in client.go (wireOriginGlobal).
	webDefaultBaseCN = "https://www.workbuddy.cn"

	// mpClientPlatform is the miniprogram header value: mp-exclusive tasks
	// (Sequential family / school_season) require it on list, accept AND claim
	// — without it accept answers "task not found" (wb2api recorded wire fact).
	mpClientPlatform = "miniprogram"

	// acceptBatchSize / acceptBatchGap port wb2api's taskAcceptAll batching:
	// the upstream publishes no task_codes length limit, so batches stay
	// conservative at 20 with the 1.05s inter-batch cadence.
	acceptBatchSize = 20
	acceptBatchGap  = 1050 * time.Millisecond
)

// mpTaskCodes are the growth tasks handed down ONLY in the miniprogram list
// (wb2api mpTaskCodes table): the default list never carries them and their
// accept/claim require the mp header. Sequential_Tasks_4..7 existence is live
// tested (chained prerequisite unlocks, one link per day at midnight CST).
var mpTaskCodes = map[string]bool{
	"school_season":      true,
	"Sequential_Tasks_1": true,
	"Sequential_Tasks_2": true,
	"Sequential_Tasks_3": true,
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// IsMPTaskCode reports whether a task code belongs to the miniprogram realm.
func IsMPTaskCode(code string) bool { return mpTaskCodes[code] }

// growthTaskTitles maps known task codes to the Chinese display titles shown
// by the panel (a plain Go-side table — deliberately outside any i18n
// machinery; the frontend passes the title through verbatim). Titles come
// from the wb2api panel's action descriptions and the frozen M7a wire
// example. Unknown codes fall back to the upstream title, then the code.
var growthTaskTitles = map[string]string{
	"chat_5":                 "完成 5 次对话",
	"first_buddy":            "领取第一只 Buddy",
	"Model_chat_GLM5.2":      "使用 GLM-5.2 对话",
	"RichMeow_Chat":          "桌面端对话 1 次",
	"Buddy_App":              "进入 Buddy 应用",
	"Buddy_App_QQ":           "进入企鹅教师助手",
	"automation_1":           "设置自动化任务",
	"Library_read":           "体验资料库",
	"template_5":             "使用 5 个模板创建任务",
	"playbook_prompt":        "灵感案例发送 Prompt",
	"create_canvas":          "创建设计画布",
	"expert_5":               "使用 5 个平台专家",
	"Expert_team_use_3":      "使用 3 个专家团",
	"Expert_lighthouse":      "体验轻量云专家",
	"Expert_Philanthropy":    "参与公益捐赠",
	"Hp_Appearance":          "更换主题外观",
	"skill_1":                "尝鲜热门技能",
	"black_cat":              "夜猫子对话",
	"school_season":          "校园日",
	"Sequential_Tasks_1":     "小程序首对话",
	"Sequential_Tasks_2":     "小程序专家对话",
	"Sequential_Tasks_3":     "小程序 5 次对话",
	"Sequential_Tasks_4":     "小程序定时任务",
	"Sequential_Tasks_5":     "小程序使用 GLM5.2",
	"Sequential_Tasks_6":     "小程序 10 次对话",
	"Sequential_Tasks_7":     "体验灵感功能",
	"wb_wechat_oa_subscribe": "关注微信公众号",
}

// growthDisplayTitle resolves the panel display title for one task: the
// Go-side table first, then the upstream title, then the raw code.
func growthDisplayTitle(code, upstream string) string {
	if zh, ok := growthTaskTitles[code]; ok && zh != "" {
		return zh
	}
	if upstream != "" {
		return upstream
	}
	return code
}

// GrowthTask is one upstream growth task folded into the panel view. The
// frozen M7a wire consumes Code/Title/Current/Target/RewardCredit/
// RewardEnergy/Status; the raw fields stay for callers that need the
// upstream state verbatim.
type GrowthTask struct {
	Code           string // upstream task_code
	Title          string // display title (Go-side map → upstream title → code)
	UpstreamTitle  string // raw upstream title
	Description    string //操作指引（含跳转说明）
	TaskDesc       string // 达成条件简述
	RewardCredit   int64  // reward_credit
	RewardEnergy   int64  // reward_energy
	HasReward      bool
	Locked         bool  // upstream unlock gate (Sequential daily chain)
	Current        int64 // progress counters (progress object overrides the flat fields)
	Target         int64
	AcceptStatus   string // raw accept_status (claimed/accepted/completed/not_accepted/…)
	UpstreamStatus string // raw status field ("complete" marker family)
	MP             bool   // true → miniprogram-realm task (mp header required)
	Claimed        bool   // accept_status == claimed
	Claimable      bool   // payout waiting: full progress (or accept_status completed) and unclaimed
	Status         string // frozen wire status: claimed|claimable|completed|in_progress|accepted|not_accepted
}

// wireStatus derives the frozen wire status from the upstream fields. Order
// matters: claimed wins over everything; then the claimable bucket (the
// claim-auto target); then the plain state-machine ladder. An unknown
// non-empty accept_status passes through verbatim rather than being folded
// into a wrong bucket.
func (t *GrowthTask) wireStatus() string {
	switch {
	case t.Claimed:
		return "claimed"
	case t.Claimable:
		return "claimable"
	case t.UpstreamStatus == "complete":
		return "completed"
	case t.AcceptStatus == "in_progress":
		return "in_progress"
	case t.AcceptStatus == "accepted":
		return "accepted"
	case t.AcceptStatus == "" || t.AcceptStatus == "not_accepted":
		return "not_accepted"
	default:
		return t.AcceptStatus
	}
}

// ---------- list ----------

// GrowthTasks pulls the full task list for one account: the default list
// merged with the miniprogram list (superset, deduped by code; mp failure
// silent — wb2api panel/tasks.go accountTasks semantics).
func (c *TaskClient) GrowthTasks(ctx context.Context, cred *Credential) ([]GrowthTask, error) {
	body, err := c.do(ctx, cred, http.MethodGet, pathGrowthTasks, nil, false)
	if err != nil {
		return nil, err
	}
	tasks, err := parseGrowthTasks(body, false)
	if err != nil {
		return nil, err
	}
	mpBody, mpErr := c.growthMP(ctx, cred, http.MethodGet, pathGrowthTasks, nil)
	if mpErr == nil {
		if mpTasks, perr := parseGrowthTasks(mpBody, true); perr == nil {
			seen := make(map[string]bool, len(tasks))
			for _, t := range tasks {
				seen[t.Code] = true
			}
			for _, t := range mpTasks {
				if !seen[t.Code] {
					tasks = append(tasks, t)
					seen[t.Code] = true
				}
			}
		}
	}
	return tasks, nil
}

// parseGrowthTasks parses data.tasks[] from one list reply. The progress
// counters may arrive flat (current/target) or as a {"current","target"}
// object; the object form wins when it carries values (wb2api recorded
// shapes, both live).
func parseGrowthTasks(body string, mp bool) ([]GrowthTask, error) {
	var resp struct {
		Data struct {
			Tasks []struct {
				TaskCode     string          `json:"task_code"`
				Title        string          `json:"title"`
				Description  string          `json:"description"`
				TaskDesc     string          `json:"task_desc"`
				RewardCredit int64           `json:"reward_credit"`
				RewardEnergy int64           `json:"reward_energy"`
				HasReward    bool            `json:"has_reward"`
				Locked       bool            `json:"locked"`
				AcceptStatus string          `json:"accept_status"`
				Status       string          `json:"status"`
				Target       int64           `json:"target"`
				Current      int64           `json:"current"`
				Progress     json.RawMessage `json:"progress"`
			} `json:"tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("workbuddy: parse growth tasks: %w", err)
	}
	out := make([]GrowthTask, 0, len(resp.Data.Tasks))
	for _, t := range resp.Data.Tasks {
		cur, tgt := t.Current, t.Target
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current int64 `json:"current"`
				Target  int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil && (pr.Target > 0 || pr.Current > 0) {
				cur, tgt = pr.Current, pr.Target
			}
		}
		g := GrowthTask{
			Code:           t.TaskCode,
			UpstreamTitle:  t.Title,
			Description:    t.Description,
			TaskDesc:       t.TaskDesc,
			RewardCredit:   t.RewardCredit,
			RewardEnergy:   t.RewardEnergy,
			HasReward:      t.HasReward,
			Locked:         t.Locked,
			Current:        cur,
			Target:         tgt,
			AcceptStatus:   t.AcceptStatus,
			UpstreamStatus: t.Status,
			MP:             mp,
		}
		g.Title = growthDisplayTitle(g.Code, g.UpstreamTitle)
		g.Claimed = g.AcceptStatus == "claimed"
		// Claimable = payout waiting. accept_status "completed" is the
		// upstream's own "done, unpaid" form (async scoring can leave the
		// progress counters behind), so it joins the numeric full-progress
		// test. Locked tasks are excluded — upstream rejects claims on them.
		g.Claimable = !g.Claimed && !g.Locked &&
			(g.AcceptStatus == "completed" || (g.Target > 0 && g.Current >= g.Target))
		g.Status = g.wireStatus()
		out = append(out, g)
	}
	return out, nil
}

// ---------- accept ----------

// GrowthAcceptAll signs up every task that is not yet accepted/completed/
// claimed and not locked (accept never produces progress; it normalizes the
// state machine so behavior events can land). Default-realm codes go in
// batches of 20 with the 1.05s cadence; mp codes ride the mp header and
// their failure is silent (campaign-ended normality, wb2api semantics).
// Returns the number of task codes whose accept call succeeded.
func (c *TaskClient) GrowthAcceptAll(ctx context.Context, cred *Credential) (int, error) {
	tasks, err := c.GrowthTasks(ctx, cred)
	if err != nil {
		return 0, err
	}
	var defCodes, mpCodes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		if t.MP {
			mpCodes = append(mpCodes, t.Code)
		} else {
			defCodes = append(defCodes, t.Code)
		}
	}

	accepted := 0
	for start := 0; start < len(defCodes); start += acceptBatchSize {
		end := start + acceptBatchSize
		if end > len(defCodes) {
			end = len(defCodes)
		}
		body, merr := json.Marshal(map[string]any{"task_codes": defCodes[start:end]})
		if merr != nil {
			return accepted, merr
		}
		if _, err := c.do(ctx, cred, http.MethodPost, pathGrowthTasksAccept, body, false); err != nil {
			return accepted, err
		}
		accepted += end - start
		// ctx-aware inter-batch cadence (the 1.05s anti-risk-control gap).
		if end < len(defCodes) {
			select {
			case <-ctx.Done():
				return accepted, ctx.Err()
			case <-time.After(acceptBatchGap):
			}
		}
	}

	if len(mpCodes) > 0 {
		body, merr := json.Marshal(map[string]any{"task_codes": mpCodes})
		if merr != nil {
			return accepted, merr
		}
		if _, err := c.growthMP(ctx, cred, http.MethodPost, pathGrowthTasksAccept, body); err == nil {
			accepted += len(mpCodes)
		}
		// mp accept failure: silent skip, matching the reference (the mp list
		// disappearing once a campaign ends is normal, not an error).
	}
	return accepted, nil
}

// ---------- claim ----------

// GrowthClaimAuto sweeps every claimable task (full progress, unclaimed —
// the wire's "claimable" bucket) and claims each one, returning the number
// of claims that went through and the credit that actually landed
// (already_claimed replies count as claimed but add no credit). The first
// claim failure aborts with the upstream error — claims are idempotent
// upstream (repeat claim answers business-rejected or already_claimed), so
// the caller can simply retry the sweep.
func (c *TaskClient) GrowthClaimAuto(ctx context.Context, cred *Credential) (claimed int, credit int64, err error) {
	tasks, err := c.GrowthTasks(ctx, cred)
	if err != nil {
		return 0, 0, err
	}
	for _, t := range tasks {
		if !t.Claimable {
			continue
		}
		cr, _, cerr := c.GrowthClaimOne(ctx, cred, t.Code)
		if cerr != nil {
			return claimed, credit, cerr
		}
		claimed++
		credit += cr
	}
	return claimed, credit, nil
}

// GrowthClaimOne claims one task reward. Default codes claim on the web
// domain (the growth-center SPA route); mp codes claim on the chat domain
// with the mp header and fall back to the web domain when the chat domain
// answers 400 (wb2api ClaimRewardMP semantics). Returns the credit that
// landed this call (0 on already_claimed).
func (c *TaskClient) GrowthClaimOne(ctx context.Context, cred *Credential, code string) (credit, energy int64, err error) {
	if IsMPTaskCode(code) {
		claimURL := strings.TrimRight(c.baseFor(cred, false), "/") + pathGrowthClaimPrefix + url.PathEscape(code) + "/claim"
		body, cerr := c.growthDo(ctx, cred, http.MethodPost, claimURL, nil, mpPlatformHeader)
		if cerr != nil {
			var ue *adapter.UpstreamError
			if errors.As(cerr, &ue) && ue.Status == http.StatusBadRequest {
				// Chat domain 400 on this path (task/tenant shape) → web
				// domain fallback, live-tested by the reference.
				return c.webClaim(ctx, cred, code)
			}
			return 0, 0, cerr
		}
		return parseGrowthClaim(body)
	}
	return c.webClaim(ctx, cred, code)
}

// webClaim POSTs {web}/activity/growth/tasks/{code}/claim with the exact
// growth-center SPA header shape the reference recorded (wb2api ClaimReward):
// Bearer auth, browser Accept, workbuddy Origin/Referer and the
// x-client-platform: web marker — the identity headers (X-User-Id,
// X-Enterprise-Id/X-Tenant-Id, X-Domain) ride the shared account-header set.
func (c *TaskClient) webClaim(ctx context.Context, cred *Credential, code string) (credit, energy int64, err error) {
	base := c.webBaseFor(cred)
	claimURL := strings.TrimRight(base, "/") + pathGrowthClaimPrefix + url.PathEscape(code) + "/claim"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/profile/growth-center")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("User-Agent", userAgent(cred))
	setAccountHeaders(req, cred)
	body, err := c.growthRoundTrip(ctx, req, claimURL)
	if err != nil {
		return 0, 0, err
	}
	return parseGrowthClaim(body)
}

// parseGrowthClaim reads the claim reply data:
// {"already_claimed":bool,"credit":n,"energy":n}. already_claimed folds to
// (0,0) — an idempotent no-op, not an error. Reward numbers go through the
// bounded creditsFrom so protocol noise never lands as a credit value;
// missing fields are a 0 award, not a failure.
func parseGrowthClaim(body string) (credit, energy int64, err error) {
	data := envelopeData(body)
	if data == nil {
		return 0, 0, fmt.Errorf("workbuddy: growth claim reply missing data object")
	}
	if b, ok := data["already_claimed"].(bool); ok && b {
		return 0, 0, nil
	}
	if n, ok := numberOf(data["credit"]); ok {
		credit, _ = creditsFrom(n)
	}
	if n, ok := numberOf(data["energy"]); ok {
		energy, _ = creditsFrom(n)
	}
	return credit, energy, nil
}

// ---------- transport (growth-domain variants of do()) ----------

// webBaseFor returns the growth-center web base for the credential's realm
// (TaskClientOptions.WebBaseURL overrides; tests point it at the stub).
func (c *TaskClient) webBaseFor(cred *Credential) string {
	if override := c.opts.WebBaseURL; override != "" {
		return override
	}
	if cred != nil && cred.Global {
		return wireOriginGlobal
	}
	return webDefaultBaseCN
}

// growthMP performs one growth-domain round trip on the chat base with the
// miniprogram platform marker (the mp list/accept/claim family).
func (c *TaskClient) growthMP(ctx context.Context, cred *Credential, method, path string, body []byte) (string, error) {
	rawURL := strings.TrimRight(c.baseFor(cred, false), "/") + path
	return c.growthDo(ctx, cred, method, rawURL, body, mpPlatformHeader)
}

// mpPlatformHeader stamps the miniprogram header (kept as a named func so
// growthDo's hook parameter documents itself).
func mpPlatformHeader(req *http.Request) {
	req.Header.Set("X-Client-Platform", mpClientPlatform)
}

// growthDo performs one growth-domain round trip against an explicit URL —
// the same request/response contract as tasks.go do() (shared header family,
// bounded 1 MiB body, taskReplyText classification) with a per-call header
// hook for the mp/web realm variants.
func (c *TaskClient) growthDo(ctx context.Context, cred *Credential, method, rawURL string, body []byte, setExtra func(*http.Request)) (string, error) {
	var payload io.Reader
	if body != nil {
		payload = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, payload)
	if err != nil {
		return "", err
	}
	setCommonHeaders(req, cred)
	req.Header.Set("Accept", "application/json")
	setAccountHeaders(req, cred)
	if setExtra != nil {
		setExtra(req)
	}
	return c.growthRoundTrip(ctx, req, rawURL)
}

// growthRoundTrip sends one prepared growth request and folds the reply
// through the shared taskReplyText classification (transport failures map
// onto the canonical UpstreamError reasons, exactly like tasks.go do()).
func (c *TaskClient) growthRoundTrip(ctx context.Context, req *http.Request, name string) (string, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", &adapter.UpstreamError{Status: http.StatusGatewayTimeout, Reason: adapter.ReasonTimeout, Message: "workbuddy: growth " + name + ": " + ctx.Err().Error()}
		}
		return "", &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: growth " + name + " transport: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: read growth " + name + " body: " + err.Error()}
	}
	return taskReplyText(resp.StatusCode, resp.Header, string(raw))
}
