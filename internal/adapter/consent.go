package adapter

// This file freezes the server-side informed-consent surface (G3, PM seq=340/360)
// ahead of its M1 endpoint implementation, so frontend/Go/UI share one naming.

// TimeLocal is the Q9 dual time representation: epoch seconds plus a server
// local date string, so countdowns use unix and calendars use local without
// browser-timezone inference.
type TimeLocal struct {
	Unix  int64  `json:"unix"`
	Local string `json:"local"`
}

// ConsentAck is the consent member of the POST /api/auth/verify request body.
// Per PM O-5 ruling the request carries ONLY source + notice_version; the
// timestamp is stamped server-side (see ConsentTrail) — a client-reported
// clock would pollute the ToS self-attestation evidence chain.
type ConsentAck struct {
	Source        string `json:"source"`
	NoticeVersion string `json:"notice_version"`
}

// ConsentTrail is the persisted audit record — exactly the three fields of
// UI §5.19 留痕: source + notice_version + server timestamp. It never records
// account identifiers, credential content, or the notice text, and is never
// uploaded anywhere.
type ConsentTrail struct {
	Source        string    `json:"source"`
	NoticeVersion string    `json:"notice_version"`
	AckAt         TimeLocal `json:"ack_at"` // server-set, Q9 dual representation
}

// FieldError is a per-field rejection entry shared by AuthVerifyResponse and
// ConfigSaveResponse (v0.5 named type). Code is the machine-readable stable
// identifier the frontend dispatches on — it never matches Chinese message text.
type FieldError struct {
	Key     string `json:"key"`
	Message string `json:"message"` // copy key (UI §5.16: 文案 key, not inline Chinese)
	Code    string `json:"code,omitempty"`
}

// FieldError machine codes (frozen strings, api-contract.ts v0.5 FieldError).
const (
	CodeRange          = "range"
	CodeConsentMissing = "consent_missing"
	CodeConsentStale   = "consent_stale"
)

// CheckConsent is the server-side half of M1 acceptance criterion #6: a save
// for a consent-required source (Qoder is always required) is rejected when
// consent is missing or its notice_version lags config.NoticeVersion — even if
// the client bypassed the frontend ConsentGate. Returns nil when accepted.
//
// Rejections go through FieldError at key "consent.notice_version" and never
// extend FailureReason: consent gaps are not source-health failures and the
// REASON_COPY/402 display table must stay unpolluted (G3 hard rule).
func CheckConsent(ack *ConsentAck, source, currentNoticeVersion string) *FieldError {
	if ack == nil || ack.Source != source || ack.NoticeVersion == "" {
		return &FieldError{
			Key:     "consent.notice_version",
			Message: "CONSENT.MISSING",
			Code:    CodeConsentMissing,
		}
	}
	if ack.NoticeVersion != currentNoticeVersion {
		return &FieldError{
			Key:     "consent.notice_version",
			Message: "CONSENT.STALE",
			Code:    CodeConsentStale,
		}
	}
	return nil
}
