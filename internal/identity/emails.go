package identity

import (
	"fmt"
	"strings"
)

// verificationEmail renders the verification message in the user's language. The link is the only
// thing in the message that matters, so the text is plain and short.
func verificationEmail(locale, link string) (subject, text string) {
	if locale == "en" {
		return "Verify your email address for Orion POS",
			strings.Join([]string{
				"Welcome to Orion POS.",
				"",
				"Open this link to verify your email address. It works once and expires in 24 hours:",
				link,
				"",
				"If you did not create an account, ignore this email.",
			}, "\n")
	}
	return "Verifikasi alamat email Anda untuk Orion POS",
		strings.Join([]string{
			"Selamat datang di Orion POS.",
			"",
			"Buka tautan ini untuk memverifikasi alamat email Anda. Tautan hanya berlaku sekali dan kedaluwarsa dalam 24 jam:",
			link,
			"",
			"Jika Anda tidak membuat akun, abaikan email ini.",
		}, "\n")
}

func verificationLink(baseURL, token string) string {
	return fmt.Sprintf("%s/verify-email?token=%s", strings.TrimRight(baseURL, "/"), token)
}
