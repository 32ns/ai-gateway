package controlplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

const EmailVerificationPurposeRegister = "register"

const defaultEmailSMTPTimeout = 20 * time.Second

var cloudMailHTTPClient = &http.Client{Timeout: 15 * time.Second}

type EmailVerificationInput struct {
	Purpose string
	Email   string
}

type verificationEmailContent struct {
	Subject string
	Text    string
	HTML    string
}

func (s *Service) EmailVerificationRequiredForRegistration() bool {
	return s.currentSystemSettings().Email.RegistrationVerificationEnabled
}

func (s *Service) SendEmailVerificationCode(ctx context.Context, input EmailVerificationInput) error {
	settings := s.currentSystemSettings()
	purpose := strings.TrimSpace(input.Purpose)
	email, err := normalizeVerificationEmail(input.Email)
	if err != nil {
		return err
	}
	if purpose != EmailVerificationPurposeRegister {
		return fmt.Errorf("email verification purpose is invalid")
	}
	if !settings.Email.RegistrationVerificationEnabled {
		return fmt.Errorf("email verification is disabled")
	}
	if err := validateEmailVerificationSettings(settings.Email); err != nil {
		return err
	}
	now := time.Now().UTC()
	if latest, err := s.repo.LatestEmailVerificationCode(purpose, email); err == nil {
		cooldown := time.Duration(settings.Email.SendCooldownSeconds) * time.Second
		if latest.CreatedAt.Add(cooldown).After(now) {
			return fmt.Errorf("verification code was sent recently")
		}
	}
	if count := s.repo.CountEmailVerificationCodesSince(purpose, email, now.Add(-time.Hour)); count >= settings.Email.HourlySendLimit {
		return fmt.Errorf("too many verification codes sent")
	}
	code, err := randomNumericCode(6)
	if err != nil {
		return err
	}
	record := core.EmailVerificationCode{
		ID:          fmt.Sprintf("email_code_%d_%s", now.UnixNano(), randomSuffix()),
		Purpose:     purpose,
		Email:       email,
		MaxAttempts: settings.Email.MaxAttempts,
		ExpiresAt:   now.Add(time.Duration(settings.Email.CodeTTLSeconds) * time.Second),
		CreatedAt:   now,
	}
	record.CodeHash = emailVerificationCodeHash(purpose, email, code, record.ID)
	if err := s.repo.CreateEmailVerificationCode(record); err != nil {
		return err
	}
	if err := sendVerificationEmail(ctx, settings.Email, email, code); err != nil {
		_ = s.repo.DeleteEmailVerificationCode(record.ID)
		return err
	}
	return nil
}

func (s *Service) TestEmailVerificationSettings(ctx context.Context, settings core.SystemEmailSettings, toEmail string) error {
	settings = core.NormalizeSystemSettings(core.SystemSettings{Email: settings}).Email
	email, err := normalizeVerificationEmail(toEmail)
	if err != nil {
		return err
	}
	if err := validateEmailVerificationSettings(settings); err != nil {
		return err
	}
	return sendVerificationEmail(ctx, settings, email, "123456")
}

func (s *Service) VerifyEmailCode(purpose, email, code string) error {
	purpose = strings.TrimSpace(purpose)
	email, err := normalizeVerificationEmail(email)
	if err != nil {
		return err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("verification code is required")
	}
	record, err := s.repo.LatestEmailVerificationCode(purpose, email)
	if err != nil {
		if err == storage.ErrNotFound {
			return fmt.Errorf("verification code is invalid or expired")
		}
		return err
	}
	now := time.Now().UTC()
	if record.UsedAt != nil || !record.ExpiresAt.After(now) || record.Attempts >= record.MaxAttempts {
		return fmt.Errorf("verification code is invalid or expired")
	}
	record.Attempts++
	expected := emailVerificationCodeHash(purpose, email, code, record.ID)
	if subtle.ConstantTimeCompare([]byte(record.CodeHash), []byte(expected)) != 1 {
		_ = s.repo.UpdateEmailVerificationCode(record)
		return fmt.Errorf("verification code is invalid or expired")
	}
	record.UsedAt = &now
	return s.repo.UpdateEmailVerificationCode(record)
}

func normalizeVerificationEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", fmt.Errorf("email is required")
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || strings.ToLower(strings.TrimSpace(parsed.Address)) != email || !strings.Contains(email, "@") {
		return "", fmt.Errorf("email is invalid")
	}
	return email, nil
}

func emailVerificationCodeHash(purpose, email, code, salt string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(purpose) + "\x00" + strings.ToLower(strings.TrimSpace(email)) + "\x00" + strings.TrimSpace(code) + "\x00" + strings.TrimSpace(salt)))
	return hex.EncodeToString(sum[:])
}

func randomNumericCode(length int) (string, error) {
	var builder strings.Builder
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		digit := n.Int64()
		if digit < 0 || digit > 9 {
			return "", fmt.Errorf("random digit out of range")
		}
		builder.WriteByte(byte('0' + digit))
	}
	return builder.String(), nil
}

func randomSuffix() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "rand"
	}
	return hex.EncodeToString(buf)
}

func validateEmailVerificationSettings(settings core.SystemEmailSettings) error {
	switch settings.Provider {
	case "", core.EmailProviderSMTP:
		return validateSMTPSettings(settings)
	case core.EmailProviderCloudMail:
		return validateCloudMailSettings(settings)
	default:
		return fmt.Errorf("email provider is invalid")
	}
}

func validateSMTPSettings(settings core.SystemEmailSettings) error {
	if strings.TrimSpace(settings.SMTPHost) == "" {
		return fmt.Errorf("smtp host is required")
	}
	if settings.SMTPPort <= 0 {
		return fmt.Errorf("smtp port is required")
	}
	if strings.TrimSpace(settings.FromEmail) == "" {
		return fmt.Errorf("sender email is required")
	}
	fromEmail := strings.TrimSpace(settings.FromEmail)
	parsed, err := mail.ParseAddress(fromEmail)
	if err != nil || strings.TrimSpace(parsed.Address) != fromEmail {
		return fmt.Errorf("sender email is invalid")
	}
	return nil
}

func validateCloudMailSettings(settings core.SystemEmailSettings) error {
	baseURL := strings.TrimSpace(settings.CloudMailBaseURL)
	if baseURL == "" {
		return fmt.Errorf("cloudmail base url is required")
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return fmt.Errorf("cloudmail base url is invalid")
	}
	switch strings.ToLower(parsedURL.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("cloudmail base url scheme must be http or https")
	}
	email := strings.TrimSpace(settings.CloudMailEmail)
	if email == "" {
		return fmt.Errorf("cloudmail email is required")
	}
	parsedEmail, err := mail.ParseAddress(email)
	if err != nil || strings.TrimSpace(parsedEmail.Address) != email {
		return fmt.Errorf("cloudmail email is invalid")
	}
	if strings.TrimSpace(settings.CloudMailPassword) == "" {
		return fmt.Errorf("cloudmail password is required")
	}
	if settings.CloudMailAccountID <= 0 {
		return fmt.Errorf("cloudmail account id is required")
	}
	return nil
}

func sendVerificationEmail(ctx context.Context, settings core.SystemEmailSettings, toEmail, code string) error {
	if settings.Provider == core.EmailProviderCloudMail {
		return sendVerificationEmailCloudMail(ctx, settings, toEmail, code)
	}
	return sendVerificationEmailSMTP(ctx, settings, toEmail, code)
}

func sendVerificationEmailSMTP(ctx context.Context, settings core.SystemEmailSettings, toEmail, code string) error {
	ctx, cancel := emailSMTPContext(ctx)
	defer cancel()

	host := strings.TrimSpace(settings.SMTPHost)
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", settings.SMTPPort))
	from := strings.TrimSpace(settings.FromEmail)
	fromName := strings.TrimSpace(settings.FromName)
	if fromName != "" {
		from = (&mail.Address{Name: fromName, Address: from}).String()
	}
	content := buildVerificationEmailContent(settings, toEmail, code)
	message := strings.Join([]string{
		"From: " + from,
		"To: " + toEmail,
		"Subject: " + mime.QEncoding.Encode("UTF-8", sanitizeEmailHeader(content.Subject)),
		"MIME-Version: 1.0",
	}, "\r\n") + "\r\n" + verificationEmailMIMEBody(content)
	auth := smtp.Auth(nil)
	if strings.TrimSpace(settings.SMTPUsername) != "" {
		auth = smtp.PlainAuth("", strings.TrimSpace(settings.SMTPUsername), strings.TrimSpace(settings.SMTPPassword), host)
	}
	if settings.SMTPPort == 465 {
		return sendMailTLS(ctx, addr, host, auth, fromEmailEnvelope(settings), []string{toEmail}, []byte(message))
	}
	return sendMailStartTLS(ctx, addr, host, auth, fromEmailEnvelope(settings), []string{toEmail}, []byte(message))
}

func sendVerificationEmailCloudMail(ctx context.Context, settings core.SystemEmailSettings, toEmail, code string) error {
	ctx, cancel := emailSMTPContext(ctx)
	defer cancel()

	baseURL := strings.TrimRight(strings.TrimSpace(settings.CloudMailBaseURL), "/")
	token, err := cloudMailLogin(ctx, baseURL, strings.TrimSpace(settings.CloudMailEmail), strings.TrimSpace(settings.CloudMailPassword))
	if err != nil {
		return err
	}
	accountID := settings.CloudMailAccountID
	if resolvedAccountID, err := resolveCloudMailAccountID(ctx, baseURL, token, settings); err == nil && resolvedAccountID > 0 {
		accountID = resolvedAccountID
	}
	content := buildVerificationEmailContent(settings, toEmail, code)
	name := strings.TrimSpace(settings.FromName)
	if name == "" {
		name = "AI Gateway"
	}
	payload := map[string]any{
		"accountId":    accountID,
		"name":         name,
		"receiveEmail": []string{toEmail},
		"subject":      sanitizeEmailHeader(content.Subject),
		"text":         content.Text,
		"content":      content.HTML,
		"sendType":     "",
		"emailId":      0,
		"attachments":  []any{},
	}
	return cloudMailPost(ctx, baseURL+"/api/email/send", token, payload, nil)
}

func buildVerificationEmailContent(settings core.SystemEmailSettings, toEmail, code string) verificationEmailContent {
	minutes := fmt.Sprintf("%d", max(1, settings.CodeTTLSeconds/60))
	seconds := fmt.Sprintf("%d", max(60, settings.CodeTTLSeconds))
	plainValues := map[string]string{
		"code":        strings.TrimSpace(code),
		"minutes":     minutes,
		"ttl_minutes": minutes,
		"ttl_seconds": seconds,
		"email":       strings.TrimSpace(toEmail),
		"to_email":    strings.TrimSpace(toEmail),
	}
	htmlValues := map[string]string{
		"code":        htmlEscapePlain(strings.TrimSpace(code)),
		"minutes":     htmlEscapePlain(minutes),
		"ttl_minutes": htmlEscapePlain(minutes),
		"ttl_seconds": htmlEscapePlain(seconds),
		"email":       htmlEscapePlain(strings.TrimSpace(toEmail)),
		"to_email":    htmlEscapePlain(strings.TrimSpace(toEmail)),
	}

	subjectTemplate := firstNonBlank(settings.VerificationSubjectTemplate, core.DefaultEmailSubjectTemplate)
	textTemplate := firstNonBlank(settings.VerificationTextTemplate, core.DefaultEmailTextTemplate)
	htmlTemplate := firstNonBlank(settings.VerificationHTMLTemplate, core.DefaultEmailHTMLTemplate)

	subject := strings.TrimSpace(renderVerificationTemplate(subjectTemplate, plainValues))
	text := strings.TrimSpace(renderVerificationTemplate(textTemplate, plainValues))
	html := strings.TrimSpace(renderVerificationTemplate(htmlTemplate, htmlValues))
	if subject == "" {
		subject = core.DefaultEmailSubjectTemplate
	}
	if text == "" {
		text = renderVerificationTemplate(core.DefaultEmailTextTemplate, plainValues)
	}
	if html == "" {
		html = "<p>" + htmlEscapePlain(text) + "</p>"
	}
	return verificationEmailContent{Subject: subject, Text: text, HTML: html}
}

func renderVerificationTemplate(template string, values map[string]string) string {
	replacements := make([]string, 0, len(values)*8)
	for name, value := range values {
		replacements = append(
			replacements,
			"{{"+name+"}}", value,
			"{{ "+name+" }}", value,
			"{"+name+"}", value,
			"%"+name+"%", value,
		)
	}
	return strings.NewReplacer(replacements...).Replace(template)
}

func verificationEmailMIMEBody(content verificationEmailContent) string {
	text := strings.TrimSpace(content.Text)
	html := strings.TrimSpace(content.HTML)
	if html == "" {
		return strings.Join([]string{
			"Content-Type: text/plain; charset=UTF-8",
			"Content-Transfer-Encoding: quoted-printable",
			"",
			quotedPrintableString(text),
		}, "\r\n")
	}
	boundary := "ag-email-" + randomSuffix()
	return strings.Join([]string{
		"Content-Type: multipart/alternative; boundary=\"" + boundary + "\"",
		"",
		"--" + boundary,
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		quotedPrintableString(text),
		"--" + boundary,
		"Content-Type: text/html; charset=UTF-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		quotedPrintableString(html),
		"--" + boundary + "--",
	}, "\r\n")
}

func quotedPrintableString(value string) string {
	var builder strings.Builder
	writer := quotedprintable.NewWriter(&builder)
	_, _ = writer.Write([]byte(value))
	_ = writer.Close()
	return builder.String()
}

func sanitizeEmailHeader(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func resolveCloudMailAccountID(ctx context.Context, baseURL, token string, settings core.SystemEmailSettings) (int, error) {
	accounts, err := cloudMailListAccounts(ctx, baseURL, token)
	if err != nil {
		return 0, err
	}
	if len(accounts) == 0 {
		return 0, nil
	}
	for _, account := range accounts {
		if account.AccountID == settings.CloudMailAccountID {
			return account.AccountID, nil
		}
	}
	loginEmail := strings.ToLower(strings.TrimSpace(settings.CloudMailEmail))
	for _, account := range accounts {
		if strings.ToLower(strings.TrimSpace(account.Email)) == loginEmail {
			return account.AccountID, nil
		}
	}
	if settings.CloudMailAccountID <= 0 && len(accounts) == 1 {
		return accounts[0].AccountID, nil
	}
	return settings.CloudMailAccountID, nil
}

type cloudMailAccount struct {
	AccountID int    `json:"accountId"`
	Email     string `json:"email"`
}

func cloudMailListAccounts(ctx context.Context, baseURL, token string) ([]cloudMailAccount, error) {
	var response struct {
		Code    int                `json:"code"`
		Message string             `json:"message"`
		Data    []cloudMailAccount `json:"data"`
	}
	endpoint := baseURL + "/api/account/list?accountId=0&size=30&lastSort=9999999999"
	if err := cloudMailGet(ctx, endpoint, token, &response); err != nil {
		return nil, err
	}
	if response.Code != 200 {
		message := strings.TrimSpace(response.Message)
		if message == "" {
			message = "cloudmail account list failed"
		}
		return nil, fmt.Errorf("%s", message)
	}
	return response.Data, nil
}

func cloudMailLogin(ctx context.Context, baseURL, email, password string) (string, error) {
	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	payload := map[string]any{
		"email":    email,
		"password": password,
	}
	if err := cloudMailPost(ctx, baseURL+"/api/login", "", payload, &response); err != nil {
		return "", err
	}
	if response.Code != 200 {
		message := strings.TrimSpace(response.Message)
		if message == "" {
			message = "cloudmail login failed"
		}
		return "", fmt.Errorf("%s", message)
	}
	if strings.TrimSpace(response.Data.Token) == "" {
		return "", fmt.Errorf("cloudmail login did not return a token")
	}
	return response.Data.Token, nil
}

func cloudMailGet(ctx context.Context, endpoint, token string, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", strings.TrimSpace(token))
	}
	resp, err := cloudMailHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cloudmail request failed: %s", strings.TrimSpace(string(respBody)))
	}
	if output != nil {
		return json.Unmarshal(respBody, output)
	}
	return nil
}

func cloudMailPost(ctx context.Context, endpoint, token string, payload any, output any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", strings.TrimSpace(token))
	}
	resp, err := cloudMailHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cloudmail request failed: %s", strings.TrimSpace(string(respBody)))
	}
	if output != nil {
		return json.Unmarshal(respBody, output)
	}
	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return err
	}
	if envelope.Code != 200 {
		message := strings.TrimSpace(envelope.Message)
		if message == "" {
			message = "cloudmail request failed"
		}
		return fmt.Errorf("%s", message)
	}
	return nil
}

func htmlEscapePlain(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	value = strings.ReplaceAll(value, "'", "&#39;")
	return value
}

func fromEmailEnvelope(settings core.SystemEmailSettings) string {
	return strings.TrimSpace(settings.FromEmail)
}

func emailSMTPContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), defaultEmailSMTPTimeout)
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultEmailSMTPTimeout)
}

func emailSMTPDeadline(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Now().Add(defaultEmailSMTPTimeout)
}

func sendMailTLS(ctx context.Context, addr, host string, auth smtp.Auth, from string, to []string, msg []byte) error {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: defaultEmailSMTPTimeout},
		Config:    &tls.Config{ServerName: host},
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(emailSMTPDeadline(ctx))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	return sendSMTPMessage(client, auth, from, to, msg)
}

func sendMailStartTLS(ctx context.Context, addr, host string, auth smtp.Auth, from string, to []string, msg []byte) error {
	dialer := net.Dialer{Timeout: defaultEmailSMTPTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(emailSMTPDeadline(ctx))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			_ = client.Close()
			return err
		}
	}
	return sendSMTPMessage(client, auth, from, to, msg)
}

func sendSMTPMessage(client *smtp.Client, auth smtp.Auth, from string, to []string, msg []byte) error {
	defer client.Close()
	if auth != nil {
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := client.Mail(strings.TrimSpace(from)); err != nil {
		return err
	}
	for _, recipient := range to {
		if err := client.Rcpt(strings.TrimSpace(recipient)); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(msg); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}
