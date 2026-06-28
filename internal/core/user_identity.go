package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

func UserInvitationSignature(user User) string {
	signature := userInvitationSignatureBytes(user)
	if len(signature) < 15 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(signature[:15])
}

func userInvitationSignatureBytes(user User) []byte {
	if strings.TrimSpace(user.ID) == "" || strings.TrimSpace(user.PasswordHash) == "" {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(user.PasswordHash))
	mac.Write([]byte(user.ID))
	return mac.Sum(nil)
}
