package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

const passwordResetTokenBytes = 32

var ErrPasswordResetTokenInvalid = errors.New("password reset link is invalid or expired")

type PasswordResetRequest struct {
	Email   string
	BaseURL string
}

func (s *Service) RequestPasswordReset(ctx context.Context, input PasswordResetRequest) error {
	settings := s.currentSystemSettings()
	email, err := normalizeVerificationEmail(input.Email)
	if err != nil {
		return err
	}
	if err := validateEmailVerificationSettings(settings.Email); err != nil {
		return fmt.Errorf("email delivery: %w", err)
	}
	baseURL, err := normalizePasswordResetBaseURL(firstNonBlank(settings.Runtime.PublicBaseURL, input.BaseURL))
	if err != nil {
		return err
	}
	user, err := s.repo.FindUserByEmail(email)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	if !user.Enabled || !strings.EqualFold(strings.TrimSpace(user.Email), email) {
		return nil
	}

	now := time.Now().UTC()
	cooldown := time.Duration(settings.Email.SendCooldownSeconds) * time.Second
	if latest, err := s.repo.LatestPasswordResetToken(email); err == nil && latest.CreatedAt.Add(cooldown).After(now) {
		return nil
	}
	if count := s.repo.CountPasswordResetTokensSince(email, now.Add(-time.Hour)); count >= settings.Email.HourlySendLimit {
		return nil
	}

	rawToken, err := randomHex(passwordResetTokenBytes)
	if err != nil {
		return err
	}
	token := core.PasswordResetToken{
		ID:        fmt.Sprintf("pwd_reset_%d_%s", now.UnixNano(), randomSuffix()),
		UserID:    user.ID,
		Email:     email,
		TokenHash: passwordResetTokenHash(rawToken),
		ExpiresAt: now.Add(time.Duration(settings.Email.CodeTTLSeconds) * time.Second),
		CreatedAt: now,
	}
	if err := s.repo.CreatePasswordResetToken(token); err != nil {
		return err
	}
	resetURL := passwordResetURL(baseURL, rawToken)
	if err := sendPasswordResetEmail(ctx, settings.Email, email, resetURL, token.ExpiresAt); err != nil {
		_ = s.repo.DeletePasswordResetToken(token.ID)
		return err
	}
	return nil
}

func (s *Service) ValidatePasswordResetToken(rawToken string) error {
	_, _, err := s.validPasswordResetToken(rawToken)
	return err
}

func (s *Service) CompletePasswordReset(rawToken, nextPassword string) error {
	token, user, err := s.validPasswordResetToken(rawToken)
	if err != nil {
		return err
	}
	if strings.TrimSpace(nextPassword) == "" {
		return fmt.Errorf("password is required")
	}
	passwordHash, err := hashPassword(nextPassword)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	token.UsedAt = &now
	if err := s.repo.UpdatePasswordResetToken(token); err != nil {
		return err
	}
	user.PasswordHash = passwordHash
	user.ForcePasswordChange = false
	if err := s.saveUserMetadata(user); err != nil {
		return err
	}
	return s.repo.DeleteUserSessionsByUser(user.ID)
}

func (s *Service) validPasswordResetToken(rawToken string) (core.PasswordResetToken, core.User, error) {
	hash := passwordResetTokenHash(rawToken)
	if hash == "" {
		return core.PasswordResetToken{}, core.User{}, ErrPasswordResetTokenInvalid
	}
	token, err := s.repo.GetPasswordResetTokenByHash(hash)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return core.PasswordResetToken{}, core.User{}, ErrPasswordResetTokenInvalid
		}
		return core.PasswordResetToken{}, core.User{}, err
	}
	now := time.Now().UTC()
	if token.UsedAt != nil || token.ExpiresAt.IsZero() || !token.ExpiresAt.After(now) {
		return core.PasswordResetToken{}, core.User{}, ErrPasswordResetTokenInvalid
	}
	user, err := s.repo.GetUser(token.UserID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return core.PasswordResetToken{}, core.User{}, ErrPasswordResetTokenInvalid
		}
		return core.PasswordResetToken{}, core.User{}, err
	}
	if !user.Enabled || !strings.EqualFold(strings.TrimSpace(user.Email), strings.TrimSpace(token.Email)) {
		return core.PasswordResetToken{}, core.User{}, ErrPasswordResetTokenInvalid
	}
	return token, user, nil
}

func normalizePasswordResetBaseURL(baseURL string) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "", fmt.Errorf("public base url is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("public base url must be a valid URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("public base url scheme must be http or https")
	}
	return baseURL, nil
}

func passwordResetURL(baseURL, rawToken string) string {
	values := url.Values{}
	values.Set("token", strings.TrimSpace(rawToken))
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/password/reset?" + values.Encode()
}

func passwordResetTokenHash(rawToken string) string {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("password-reset-token\x00" + rawToken))
	return hex.EncodeToString(sum[:])
}

func sendPasswordResetEmail(ctx context.Context, settings core.SystemEmailSettings, toEmail, resetURL string, expiresAt time.Time) error {
	content := buildPasswordResetEmailContent(settings, toEmail, resetURL, expiresAt)
	return sendEmail(ctx, settings, toEmail, content)
}

func buildPasswordResetEmailContent(settings core.SystemEmailSettings, toEmail, resetURL string, expiresAt time.Time) verificationEmailContent {
	ttl := time.Until(expiresAt)
	if ttl < time.Minute {
		ttl = time.Minute
	}
	minutes := fmt.Sprintf("%d", int(ttl.Round(time.Minute)/time.Minute))
	brand := strings.TrimSpace(settings.FromName)
	if brand == "" {
		brand = "AI Gateway"
	}
	subject := "Reset your " + brand + " password"
	text := strings.Join([]string{
		"We received a request to reset the password for " + strings.TrimSpace(toEmail) + ".",
		"Open this link to set a new password:",
		strings.TrimSpace(resetURL),
		"This link expires in " + minutes + " minutes. If you did not request this, you can ignore this email.",
	}, "\n\n")
	html := strings.Join([]string{
		"<p>We received a request to reset the password for " + htmlEscapePlain(strings.TrimSpace(toEmail)) + ".</p>",
		"<p><a href=\"" + htmlEscapePlain(strings.TrimSpace(resetURL)) + "\">Reset your password</a></p>",
		"<p>This link expires in " + htmlEscapePlain(minutes) + " minutes. If you did not request this, you can ignore this email.</p>",
	}, "")
	return verificationEmailContent{Subject: subject, Text: text, HTML: html}
}
