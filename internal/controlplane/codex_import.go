package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
)

type codexAuthFile struct {
	AuthMode     string           `json:"auth_mode"`
	OpenAIAPIKey *string          `json:"OPENAI_API_KEY"`
	Tokens       *codexAuthTokens `json:"tokens"`
	AccessToken  string           `json:"access_token"`
	RefreshToken string           `json:"refresh_token"`
	SessionToken string           `json:"session_token"`
	IDToken      string           `json:"id_token"`
	AccountID    string           `json:"account_id"`
	Email        string           `json:"email"`
	Expired      string           `json:"expired"`
}

type codexAuthTokens struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	IDToken      string  `json:"id_token"`
	AccountID    *string `json:"account_id"`
}

type CodexOpenAIAuthUpload struct {
	Name    string
	Payload []byte
	Error   string
}

func (s *Service) ImportCodexOpenAIAuthPayload(source string, payload []byte) (OpenAIOAuthConnectResult, error) {
	imported, err := codexOpenAIConnectImportFromPayload(source, payload)
	if err != nil {
		return OpenAIOAuthConnectResult{}, err
	}
	importID, err := generateConnectImportID()
	if err != nil {
		return OpenAIOAuthConnectResult{}, err
	}
	s.oauthMu.Lock()
	storeMapValue(&s.openAIImports, importID, imported)
	s.oauthMu.Unlock()
	return OpenAIOAuthConnectResult{
		ImportID: importID,
		Label:    imported.Label,
		Email:    imported.OAuthEmail,
	}, nil
}

func codexOpenAIConnectImportFromPayload(source string, payload []byte) (oauthConnectImport, error) {
	var auth codexAuthFile
	if err := json.Unmarshal(payload, &auth); err != nil {
		return oauthConnectImport{}, fmt.Errorf("parse codex auth.json: %w", err)
	}
	auth.normalize()
	if auth.Tokens == nil {
		if auth.OpenAIAPIKey == nil || strings.TrimSpace(*auth.OpenAIAPIKey) == "" {
			return oauthConnectImport{}, fmt.Errorf("codex auth.json does not contain ChatGPT OAuth tokens or OPENAI_API_KEY")
		}
		return newOAuthConnectImport(
			"OpenAI API key from Codex",
			strings.TrimSpace(*auth.OpenAIAPIKey),
			"",
			nil,
			"manual-token",
			"",
			"",
			"",
			source,
		), nil
	}
	if strings.TrimSpace(auth.Tokens.AccessToken) == "" {
		return oauthConnectImport{}, fmt.Errorf("codex auth.json is missing tokens.access_token")
	}

	tokenSet := providers.OpenAITokenSet{
		AccessToken:  strings.TrimSpace(auth.Tokens.AccessToken),
		RefreshToken: strings.TrimSpace(auth.Tokens.RefreshToken),
		IDToken:      strings.TrimSpace(auth.Tokens.IDToken),
	}
	accountID, email := providers.ExtractOpenAIIdentity(tokenSet)
	if accountID == "" && auth.Tokens.AccountID != nil {
		accountID = strings.TrimSpace(*auth.Tokens.AccountID)
	}
	if email == "" {
		email = strings.TrimSpace(auth.Email)
	}

	expiresAt := providers.ExtractOpenAITokenExpiry(tokenSet)
	if expiresAt == nil {
		expiresAt = parseCodexAuthExpired(auth.Expired)
	}
	imported := newOAuthConnectImport(
		oauthImportLabel(core.ProviderOpenAI, email, accountID),
		tokenSet.AccessToken,
		tokenSet.RefreshToken,
		expiresAt,
		providers.OpenAIOAuthModeValue(),
		providers.OpenAICodexAuthTokenSourceValue(),
		accountID,
		email,
		source,
	)
	imported.SessionToken = strings.TrimSpace(auth.SessionToken)
	return imported, nil
}

func (s *Service) ImportCodexOpenAIAuthUploads(files []CodexOpenAIAuthUpload, group string) (CodexOpenAIAuthUploadResult, error) {
	group = normalizeAccountGroup(group)
	if len(files) == 0 {
		return CodexOpenAIAuthUploadResult{}, fmt.Errorf("no account files uploaded")
	}
	result := CodexOpenAIAuthUploadResult{Path: "upload"}
	for _, file := range files {
		source := strings.TrimSpace(file.Name)
		if source == "" {
			source = "uploaded-auth.json"
		}
		item := CodexOpenAIAuthUploadItem{Path: source}
		if strings.TrimSpace(file.Error) != "" {
			item.Error = strings.TrimSpace(file.Error)
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		payloads, err := codexUploadPayloads(file.Payload)
		if err != nil {
			item.Error = err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		for i, payload := range payloads {
			recordSource := source
			if len(payloads) > 1 {
				recordSource = fmt.Sprintf("%s#%d", source, i+1)
			}
			item := CodexOpenAIAuthUploadItem{Path: recordSource}
			imported, err := codexOpenAIConnectImportFromPayload("upload:"+recordSource, payload)
			if err != nil {
				item.Error = err.Error()
				result.Failed++
				result.Items = append(result.Items, item)
				continue
			}
			item.Label = imported.Label
			item.Email = imported.OAuthEmail
			item.AccountID = imported.OAuthAccountID
			if s.openAICodexAccountExists("", imported.OAuthEmail) {
				item.Skipped = true
				item.Error = "account already exists"
				result.Skipped++
				result.Items = append(result.Items, item)
				continue
			}
			if err := s.CompleteManualConnect(ManualConnectInput{
				Provider:       core.ProviderOpenAI,
				Label:          imported.Label,
				AccessToken:    imported.AccessToken,
				RefreshToken:   imported.RefreshToken,
				SessionToken:   imported.SessionToken,
				Group:          group,
				BaseURL:        imported.BaseURL,
				ExpiresAt:      cloneTimePtr(imported.CredentialExpiresAt),
				Backup:         imported.Backup,
				Priority:       imported.Priority,
				Weight:         imported.Weight,
				CredentialMode: imported.CredentialMode,
				TokenSource:    imported.TokenSource,
				OAuthAccountID: imported.OAuthAccountID,
				OAuthEmail:     imported.OAuthEmail,
				CodexAuthPath:  "",
			}); err != nil {
				item.Error = err.Error()
				result.Failed++
				result.Items = append(result.Items, item)
				continue
			}
			item.Imported = true
			result.Imported++
			result.Items = append(result.Items, item)
		}
	}
	return result, nil
}

func codexUploadPayloads(payload []byte) ([][]byte, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("uploaded account file is empty")
	}
	if trimmed[0] == '[' {
		var records []json.RawMessage
		if err := json.Unmarshal(trimmed, &records); err != nil {
			return nil, fmt.Errorf("parse uploaded account array: %w", err)
		}
		if len(records) == 0 {
			return nil, fmt.Errorf("uploaded account array is empty")
		}
		out := make([][]byte, 0, len(records))
		for _, record := range records {
			out = append(out, append([]byte(nil), record...))
		}
		return out, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	var out [][]byte
	for {
		var record json.RawMessage
		if err := decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parse uploaded account file: %w", err)
		}
		out = append(out, append([]byte(nil), record...))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("uploaded account file is empty")
	}
	return out, nil
}

func (auth *codexAuthFile) normalize() {
	if auth == nil || auth.Tokens != nil {
		return
	}
	if strings.TrimSpace(auth.AccessToken) == "" && strings.TrimSpace(auth.RefreshToken) == "" {
		return
	}
	accountID := strings.TrimSpace(auth.AccountID)
	auth.Tokens = &codexAuthTokens{
		AccessToken:  strings.TrimSpace(auth.AccessToken),
		RefreshToken: strings.TrimSpace(auth.RefreshToken),
		IDToken:      strings.TrimSpace(auth.IDToken),
	}
	if accountID != "" {
		auth.Tokens.AccountID = &accountID
	}
}

func parseCodexAuthExpired(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			utc := parsed.UTC()
			return &utc
		}
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

func (s *Service) openAICodexAccountExists(authPath, email string) bool {
	authPath = strings.TrimSpace(authPath)
	email = strings.TrimSpace(email)
	for _, account := range s.repo.ListAccounts() {
		if account.Provider != core.ProviderOpenAI {
			continue
		}
		metadata := account.Credential.Metadata
		if authPath != "" && strings.EqualFold(strings.TrimSpace(metadata[providers.OpenAICodexAuthPathMetadataKey]), authPath) {
			return true
		}
		if email != "" && strings.EqualFold(strings.TrimSpace(metadata["email"]), email) {
			return true
		}
	}
	return false
}
