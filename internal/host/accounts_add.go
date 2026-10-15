package host

// AddAccount flows (v1.2 主人裁定：添加账号是面板内一等操作，文件只是落盘位置).
// Both paths validate, persist to auth_dir, register in the pool + store and
// are idempotent on the account identity: re-adding replaces the credential
// in place (the pool entry is updated, not duplicated).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"all2api/internal/pool"
)

// AddWorkbuddyCredential parses a raw workbuddy auth JSON (the exact shape of
// the official desktop client's auth\*.info files), persists it under
// auth_dir/workbuddy-<uid>.json and registers the account. Re-adding an
// existing uid replaces the credential atomically (same file name). Returns
// the uid for the panel's confirmation message.
func (s *CredentialStore) AddWorkbuddyCredential(rawJSON string) (string, error) {
	var doc authFileDoc
	if err := json.Unmarshal([]byte(rawJSON), &doc); err != nil {
		return "", fmt.Errorf("凭证 JSON 无法解析: %w", err)
	}
	if strings.TrimSpace(doc.Account.UID) == "" {
		return "", errors.New("凭证缺少 account.uid")
	}
	if strings.TrimSpace(doc.Auth.AccessToken) == "" || strings.TrimSpace(doc.Auth.RefreshToken) == "" {
		return "", errors.New("凭证缺少 auth.accessToken / auth.refreshToken")
	}

	s.mu.Lock()
	dir := s.dir
	s.mu.Unlock()
	if dir == "" {
		return "", errors.New("auth_dir 未初始化（网关启动时未加载凭证目录）")
	}

	cred, err := parseAuthFileContent(rawJSON)
	if err != nil {
		return "", err
	}

	// Persist first (source of truth), then register in memory. Write via the
	// same raw-map round-trip LoadAuthDir uses so unknown fields survive a
	// later token rotation write-back.
	if err := writeAuthFileNew(filepath.Join(dir, authFilePrefix+cred.UID+authFileSuffix), rawJSON); err != nil {
		return "", fmt.Errorf("写入凭证文件失败: %w", err)
	}

	s.put(credKey(WorkbuddyPlatform, cred.UID), cred)
	if s.pool != nil {
		s.pool.AddAccount(&pool.Account{
			ID:        cred.UID,
			Platform:  WorkbuddyPlatform,
			ExpiresAt: cred.ExpiresAt,
			Status:    "active",
			Health:    1,
		})
	}
	return cred.UID, nil
}

// AddQoderPAT stores a Qoder Personal Access Token (auth_dir/qoder-pat.txt,
// one line) and registers the fingerprint account. Idempotent: the same PAT
// re-added is a no-op success; a different PAT replaces it.
func (s *CredentialStore) AddQoderPAT(pat string) (string, error) {
	pat = strings.TrimSpace(pat)
	if !strings.HasPrefix(pat, qoderPatPrefix) {
		return "", errors.New("PAT 必须以 pt- 开头")
	}

	s.mu.Lock()
	dir := s.dir
	s.mu.Unlock()
	if dir == "" {
		return "", errors.New("auth_dir 未初始化（网关启动时未加载凭证目录）")
	}

	uid := patFingerprint(pat)
	path := filepath.Join(dir, qoderPatFile)
	if err := os.WriteFile(path, []byte(pat+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("写入 PAT 文件失败: %w", err)
	}

	cred := fileCred{AccessToken: pat, UID: uid, Path: path}
	s.put(credKey(QoderPlatform, uid), cred)
	if s.pool != nil {
		s.pool.AddAccount(&pool.Account{
			ID:       uid,
			Platform: QoderPlatform,
			Status:   "active",
			Health:   1,
		})
	}
	return uid, nil
}

// parseAuthFileContent is parseAuthFile over an in-memory payload (the panel
// paste path). Same validation, no file read.
func parseAuthFileContent(raw string) (fileCred, error) {
	var doc authFileDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return fileCred{}, fmt.Errorf("parse: %w", err)
	}
	if strings.TrimSpace(doc.Account.UID) == "" {
		return fileCred{}, errors.New("account.uid missing")
	}
	if strings.TrimSpace(doc.Auth.AccessToken) == "" {
		return fileCred{}, errors.New("auth.accessToken missing")
	}
	cred := fileCred{
		AccessToken:  doc.Auth.AccessToken,
		RefreshToken: doc.Auth.RefreshToken,
		UID:          doc.Account.UID,
		Nickname:     doc.Account.Nickname,
		EnterpriseID: doc.Account.EnterpriseID,
		Domain:       doc.Auth.Domain,
		Global:       strings.EqualFold(strings.TrimSpace(doc.Auth.Realm), "global"),
	}
	if doc.Auth.ExpiresAt > 0 {
		cred.ExpiresAt = time.Unix(doc.Auth.ExpiresAt, 0)
	}
	return cred, nil
}

// writeAuthFileNew writes a brand-new auth file verbatim (atomic temp+rename
// to match the rotation write-back's durability posture).
func writeAuthFileNew(path, rawJSON string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".auth-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(rawJSON); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
