package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
	"golang.org/x/crypto/scrypt"
)

const (
	encryptedValuePrefixV3 = "enc:v3:aesgcm:"
)

type credentialCodec struct {
	dataAEAD cipher.AEAD
}

func newCredentialCodec(masterKey string) (*credentialCodec, error) {
	masterKey = strings.TrimSpace(masterKey)
	if masterKey == "" {
		return nil, nil
	}

	dataAEAD, err := deriveV3AEAD(masterKey)
	if err != nil {
		return nil, err
	}
	return &credentialCodec{dataAEAD: dataAEAD}, nil
}

func (c *credentialCodec) DecryptAccount(account core.Account) (core.Account, error) {
	decoded := cloneAccount(account)
	var err error
	if decoded.ProxyURL, err = c.decryptValue(decoded.ProxyURL); err != nil {
		return core.Account{}, err
	}
	if decoded.Credential.AccessToken, err = c.decryptValue(decoded.Credential.AccessToken); err != nil {
		return core.Account{}, err
	}
	if decoded.Credential.RefreshToken, err = c.decryptValue(decoded.Credential.RefreshToken); err != nil {
		return core.Account{}, err
	}
	if decoded.Credential.SessionToken, err = c.decryptValue(decoded.Credential.SessionToken); err != nil {
		return core.Account{}, err
	}
	for key, value := range decoded.Credential.Metadata {
		decoded.Credential.Metadata[key], err = c.decryptValue(value)
		if err != nil {
			return core.Account{}, err
		}
	}
	return decoded, nil
}

func (c *credentialCodec) EncryptCredential(credential core.Credential) (core.Credential, error) {
	if c == nil {
		return cloneCredential(credential), nil
	}

	encoded := cloneCredential(credential)
	var err error
	if encoded.AccessToken, err = c.encryptValue(encoded.AccessToken); err != nil {
		return core.Credential{}, err
	}
	if encoded.RefreshToken, err = c.encryptValue(encoded.RefreshToken); err != nil {
		return core.Credential{}, err
	}
	if encoded.SessionToken, err = c.encryptValue(encoded.SessionToken); err != nil {
		return core.Credential{}, err
	}
	for key, value := range encoded.Metadata {
		encoded.Metadata[key], err = c.encryptValue(value)
		if err != nil {
			return core.Credential{}, err
		}
	}
	return encoded, nil
}

func (c *credentialCodec) DecryptCredential(credential core.Credential) (core.Credential, error) {
	decoded := cloneCredential(credential)
	var err error
	if decoded.AccessToken, err = c.decryptValue(decoded.AccessToken); err != nil {
		return core.Credential{}, err
	}
	if decoded.RefreshToken, err = c.decryptValue(decoded.RefreshToken); err != nil {
		return core.Credential{}, err
	}
	if decoded.SessionToken, err = c.decryptValue(decoded.SessionToken); err != nil {
		return core.Credential{}, err
	}
	for key, value := range decoded.Metadata {
		decoded.Metadata[key], err = c.decryptValue(value)
		if err != nil {
			return core.Credential{}, err
		}
	}
	return decoded, nil
}

func (c *credentialCodec) EncryptAccountGroup(group core.AccountGroup) (core.AccountGroup, error) {
	if c == nil {
		return cloneAccountGroup(group), nil
	}

	encoded := cloneAccountGroup(group)
	var err error
	if encoded.ProxyURL, err = c.encryptValue(encoded.ProxyURL); err != nil {
		return core.AccountGroup{}, err
	}
	return encoded, nil
}

func (c *credentialCodec) DecryptAccountGroup(group core.AccountGroup) (core.AccountGroup, error) {
	decoded := cloneAccountGroup(group)
	var err error
	if decoded.ProxyURL, err = c.decryptValue(decoded.ProxyURL); err != nil {
		return core.AccountGroup{}, err
	}
	return decoded, nil
}

func (c *credentialCodec) EncryptClient(client core.APIClient) (core.APIClient, error) {
	if c == nil {
		return cloneClient(client), nil
	}

	encoded := cloneClient(client)
	var err error
	if encoded.APIKey, err = c.encryptValue(encoded.APIKey); err != nil {
		return core.APIClient{}, err
	}
	return encoded, nil
}

func (c *credentialCodec) DecryptClient(client core.APIClient) (core.APIClient, error) {
	decoded := cloneClient(client)
	var err error
	if decoded.APIKey, err = c.decryptValue(decoded.APIKey); err != nil {
		return core.APIClient{}, err
	}
	return decoded, nil
}

func (c *credentialCodec) EncryptSystemSettings(settings core.SystemSettings) (core.SystemSettings, error) {
	if c == nil {
		return settings, nil
	}

	encoded := settings
	if err := c.encryptStringFields(systemSettingsSecretFields(&encoded)...); err != nil {
		return core.SystemSettings{}, err
	}
	return encoded, nil
}

func (c *credentialCodec) DecryptSystemSettings(settings core.SystemSettings) (core.SystemSettings, error) {
	decoded := settings
	if err := c.decryptStringFields(systemSettingsSecretFields(&decoded)...); err != nil {
		return core.SystemSettings{}, err
	}
	return decoded, nil
}

func encryptedAccountGroupSample(group core.AccountGroup) string {
	if isEncryptedValue(group.ProxyURL) {
		return group.ProxyURL
	}
	return ""
}

func encryptedClientSample(client core.APIClient) string {
	if isEncryptedValue(client.APIKey) {
		return client.APIKey
	}
	return ""
}

func encryptedCredentialSample(credential core.Credential) string {
	for _, value := range []string{
		credential.AccessToken,
		credential.RefreshToken,
		credential.SessionToken,
	} {
		if isEncryptedValue(value) {
			return value
		}
	}
	for _, value := range credential.Metadata {
		if isEncryptedValue(value) {
			return value
		}
	}
	return ""
}

func encryptedSystemSettingsSample(settings core.SystemSettings) string {
	for _, field := range systemSettingsSecretFields(&settings) {
		if field != nil && isEncryptedValue(*field) {
			return *field
		}
	}
	return ""
}

func validateEncryptedCredentialValue(codec *credentialCodec, value string) error {
	if !isEncryptedValue(value) {
		return nil
	}
	_, err := codec.decryptValue(value)
	return err
}

func systemSettingsSecretFields(settings *core.SystemSettings) []*string {
	return []*string{
		&settings.Network.SystemProxyURL,
		&settings.OAuth.GitHubLoginSecret,
		&settings.OAuth.GoogleLoginSecret,
		&settings.Email.SMTPPassword,
		&settings.Email.CloudMailPassword,
		&settings.Registration.TurnstileSecretKey,
		&settings.Payment.WeChatPay.APIV3Key,
		&settings.Payment.WeChatPay.MerchantPrivateKeyPEM,
		&settings.Payment.Alipay.PrivateKeyPEM,
		&settings.Payment.PersonalPay.AndroidToken,
	}
}

func (c *credentialCodec) encryptStringFields(fields ...*string) error {
	for _, field := range fields {
		value, err := c.encryptValue(*field)
		if err != nil {
			return err
		}
		*field = value
	}
	return nil
}

func (c *credentialCodec) decryptStringFields(fields ...*string) error {
	for _, field := range fields {
		value, err := c.decryptValue(*field)
		if err != nil {
			return err
		}
		*field = value
	}
	return nil
}

func (c *credentialCodec) encryptValue(value string) (string, error) {
	if c == nil || value == "" {
		return value, nil
	}

	aead := c.dataAEAD
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, nonce, []byte(value), nil)
	return encryptedValuePrefixV3 +
		base64.StdEncoding.EncodeToString(nonce) + ":" +
		base64.StdEncoding.EncodeToString(sealed), nil
}

func (c *credentialCodec) decryptValue(value string) (string, error) {
	if !isEncryptedValue(value) {
		return value, nil
	}
	if c == nil {
		return "", fmt.Errorf("state store contains encrypted credentials but config master_key is not configured")
	}

	if strings.HasPrefix(value, encryptedValuePrefixV3) {
		return c.decryptV3Value(value)
	}
	return "", fmt.Errorf("unsupported encrypted credential format")
}

func (c *credentialCodec) decryptV3Value(value string) (string, error) {
	payload := strings.TrimPrefix(value, encryptedValuePrefixV3)
	noncePart, cipherPart, ok := strings.Cut(payload, ":")
	if !ok {
		return "", fmt.Errorf("encrypted credential payload is malformed")
	}
	return decryptWithAEAD(c.dataAEAD, noncePart, cipherPart)
}

func deriveV3AEAD(masterKey string) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(masterKey), []byte("ai-gateway-state-secret-v3"), 1<<15, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func decryptWithAEAD(aead cipher.AEAD, noncePart, cipherPart string) (string, error) {
	nonce, err := base64.StdEncoding.DecodeString(noncePart)
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(cipherPart)
	if err != nil {
		return "", err
	}
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt credential: %w", err)
	}
	return string(plain), nil
}

func isEncryptedValue(value string) bool {
	return strings.HasPrefix(value, encryptedValuePrefixV3)
}
